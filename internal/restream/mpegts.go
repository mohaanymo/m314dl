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
// that falls a whole buffer behind is dropped, and no one else ever waits on it.
//
// Publishing whole segments (not arbitrary byte chunks) is deliberate: each HLS
// TS segment begins with PAT/PMT and is independently decodable, so a client
// that joins mid-stream always starts on a clean, playable boundary.

// tsSubBuffer is how many segments a subscriber may fall behind before it is
// dropped. At ~4-6s segments that is ~30-45s of slack — generous for a network
// hiccup, bounded so one stuck client can't grow memory without limit.
const tsSubBuffer = 8

// TSBroadcaster fans one ordered segment stream out to many HTTP subscribers.
type TSBroadcaster struct {
	mu   sync.RWMutex
	subs map[*tsSub]struct{}
	done chan struct{}

	segments atomic.Int64
	bytes    atomic.Int64
}

// tsSub is one connected client. data carries whole segments (shared, never
// mutated after publish); kicked closes when the client falls too far behind.
type tsSub struct {
	data   chan []byte
	kicked chan struct{}
}

func NewTSBroadcaster() *TSBroadcaster {
	return &TSBroadcaster{subs: map[*tsSub]struct{}{}, done: make(chan struct{})}
}

// Subscribe registers a client. The caller must Unsubscribe when it disconnects.
func (b *TSBroadcaster) Subscribe() *tsSub {
	s := &tsSub{data: make(chan []byte, tsSubBuffer), kicked: make(chan struct{})}
	b.mu.Lock()
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
// subscriber whose buffer is full has fallen too far behind: it is kicked (a
// clean disconnect) rather than dropped-from, because losing bytes mid-TS
// corrupts the stream for that viewer. The producer never waits.
func (b *TSBroadcaster) publish(seg []byte) {
	b.segments.Add(1)
	b.bytes.Add(int64(len(seg)))
	b.mu.RLock()
	subs := make([]*tsSub, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.RUnlock()
	for _, s := range subs {
		select {
		case s.data <- seg:
		default:
			b.kick(s) // buffer full → too slow; drop this one, never block the rest
		}
	}
}

func (b *TSBroadcaster) kick(s *tsSub) {
	select {
	case <-s.kicked:
	default:
		close(s.kicked)
	}
}

// End signals every subscriber that the stream has finished, so their handlers
// close the HTTP response cleanly.
func (b *TSBroadcaster) End() {
	select {
	case <-b.done:
	default:
		close(b.done)
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
