package restream

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/mohamed/m314dl/internal/engine"
)

// Continuous MPEG-TS output.
//
// A TS-input stream's segments are already MPEG-TS; concatenated in order they
// ARE a valid continuous transport stream — no re-mux, no FFmpeg. The sink
// renumbers continuity counters across segment boundaries (the one genuinely
// good idea in the drm worker's tscontinuity.go, applied where it belongs:
// once, in-process, with no reconnect seam to bridge) and hands each whole
// segment to a broadcaster that fans it out to every connected HTTP client.
//
// The broadcaster fixes the worker's worst flaw. There, a slow subscriber
// stalled the producer — and therefore every other viewer — for up to two
// seconds each, serially (audit #20). Here every send is non-blocking: a client
// that falls a byte budget behind is dropped, and no one else ever waits on it.
//
// Publishing whole segments (not arbitrary byte chunks) is deliberate: each HLS
// TS segment begins with PAT/PMT and is independently decodable, so a client
// that joins mid-stream always starts on a clean, playable boundary.

// maxSubQueuedBytes is how many bytes a subscriber may have queued (published
// but not yet written to its socket) before it is dropped. Slack must be
// measured in bytes, not publish units: the remux pump publishes whatever each
// pipe read returned (often a few KB), so a unit-counted buffer collapsed to a
// fraction of a second of slack and kicked every viewer within seconds. 32 MiB
// is ~30-60s at typical bitrates — generous for a network hiccup, bounded so
// one stuck client can't grow memory without limit. A var so tests can shrink
// it.
var maxSubQueuedBytes = int64(32 << 20)

// tsSubChanCap sizes each subscriber's channel. Large enough that the byte
// budget, not the slot count, is what triggers a kick even for small chunks;
// it only bounds slice headers (~24B each), not payload.
const tsSubChanCap = 4096

// TSBroadcaster fans one ordered segment stream out to many HTTP subscribers.
// publish is called by exactly one producer goroutine (the TSSink writer or
// the remux pump).
type TSBroadcaster struct {
	mu   sync.RWMutex
	subs map[*tsSub]struct{}
	last []byte // most recent published unit, primed into new subscribers
	done chan struct{}

	segments atomic.Int64
	bytes    atomic.Int64
}

// tsSub is one connected client. data carries whole publish units (shared,
// never mutated after publish); queued tracks the bytes in data not yet taken
// by the handler; kicked closes when the client falls too far behind.
type tsSub struct {
	data   chan []byte
	queued atomic.Int64
	kicked chan struct{}
}

func NewTSBroadcaster() *TSBroadcaster {
	return &TSBroadcaster{subs: map[*tsSub]struct{}{}, done: make(chan struct{})}
}

// Subscribe registers a client. The caller must Unsubscribe when it
// disconnects. The new subscriber is primed with the most recent published
// unit so a joiner has something to decode immediately instead of waiting up
// to a whole segment interval for the first byte; holding b.mu across prime
// and registration keeps the sequence gapless against a concurrent publish.
func (b *TSBroadcaster) Subscribe() *tsSub {
	s := &tsSub{data: make(chan []byte, tsSubChanCap), kicked: make(chan struct{})}
	b.mu.Lock()
	if b.last != nil {
		s.queued.Store(int64(len(b.last)))
		s.data <- b.last
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *TSBroadcaster) Unsubscribe(s *tsSub) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

// publish hands one segment to every subscriber without ever blocking. A
// subscriber over its byte budget has fallen too far behind: it is kicked (a
// clean disconnect) rather than dropped-from, because losing bytes mid-TS
// corrupts the stream for that viewer. The producer never waits.
func (b *TSBroadcaster) publish(seg []byte) {
	b.segments.Add(1)
	b.bytes.Add(int64(len(seg)))
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = seg
	for s := range b.subs {
		if s.queued.Load()+int64(len(seg)) > maxSubQueuedBytes {
			b.kickLocked(s) // too many bytes behind; drop this one, never block the rest
			continue
		}
		select {
		case s.data <- seg:
			s.queued.Add(int64(len(seg)))
		default:
			b.kickLocked(s) // slot count exhausted (pathologically small chunks)
		}
	}
}

// kickLocked disconnects a lagging subscriber and removes it from the fan-out,
// so no further (now gapped, hence corrupt) units are queued to it. Caller
// holds b.mu.
func (b *TSBroadcaster) kickLocked(s *tsSub) {
	select {
	case <-s.kicked:
	default:
		close(s.kicked)
	}
	delete(b.subs, s)
}

// End signals every subscriber that the stream has finished, so their handlers
// drain what is queued and close the HTTP response cleanly.
func (b *TSBroadcaster) End() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
}

// Ended reports whether the stream has finished (End was called). A finished
// broadcast has nothing live left to join.
func (b *TSBroadcaster) Ended() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// Viewers returns the current subscriber count.
func (b *TSBroadcaster) Viewers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// StatusLine implements the presentation status contract.
func (b *TSBroadcaster) StatusLine() string {
	return fmt.Sprintf("ts: %d viewers | %d segs | %s published",
		b.Viewers(), b.segments.Load(), fmtBytes(b.bytes.Load()))
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// ─── sink ────────────────────────────────────────────────────────────────────

// TSSink implements engine.Sink for continuous MPEG-TS output: it renumbers
// each segment's continuity counters into one seamless sequence, then publishes
// the whole segment to the broadcaster.
type TSSink struct {
	b  *TSBroadcaster
	cc ccRewriter
}

func NewTSSink(b *TSBroadcaster) *TSSink {
	return &TSSink{b: b, cc: ccRewriter{next: map[uint16]byte{}}}
}

// Init is never called for a TS stream (no init segment); satisfy the interface.
func (s *TSSink) Init(_ []byte) error { return nil }

func (s *TSSink) Segment(_ engine.SegmentInfo, data []byte) error {
	seg := append([]byte(nil), data...) // own it: the broadcaster holds it after we return
	s.cc.rewrite(seg)
	s.b.publish(seg)
	return nil
}

// ─── continuity counters ─────────────────────────────────────────────────────

const tsPacketSize = 188

// ccRewriter renumbers the 4-bit continuity counter of every payload-bearing TS
// packet into one continuous per-PID sequence across all segments. A player
// treats a CC jump as lost media and stalls to resync; independently-muxed
// source segments often restart their counters, so renumbering across the
// boundary is what makes concatenation seamless. State persists for the life of
// the stream — that is the whole point.
type ccRewriter struct {
	next map[uint16]byte
}

// rewrite patches seg in place. It processes whole 188-byte packets from the
// first sync byte; a segment that is not 188-aligned has its trailing bytes
// left untouched rather than corrupted.
func (c *ccRewriter) rewrite(seg []byte) {
	for off := 0; off+tsPacketSize <= len(seg); off += tsPacketSize {
		if seg[off] != 0x47 {
			return // alignment lost; don't guess past here
		}
		pid := uint16(seg[off+1]&0x1f)<<8 | uint16(seg[off+2])
		afc := (seg[off+3] >> 4) & 0x3
		// Null packets (0x1FFF) are stuffing; only payload-bearing packets
		// (adaptation-field control 1 or 3) carry a meaningful counter.
		if pid != 0x1fff && (afc == 1 || afc == 3) {
			cc := c.next[pid]
			seg[off+3] = (seg[off+3] & 0xf0) | (cc & 0x0f)
			c.next[pid] = (cc + 1) & 0x0f
		}
	}
}
