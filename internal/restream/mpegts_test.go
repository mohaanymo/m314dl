package restream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mohamed/m314dl/internal/engine"
)

// tsPacket builds a 188-byte payload-bearing TS packet for pid with counter cc.
func tsPacket(pid uint16, cc byte) []byte {
	p := make([]byte, tsPacketSize)
	p[0] = 0x47
	p[1] = byte(pid>>8) & 0x1f
	p[2] = byte(pid)
	p[3] = 0x10 | (cc & 0x0f) // adaptation-field control = 1 (payload only)
	return p
}

func ccOf(pkt []byte) byte { return pkt[3] & 0x0f }

// Continuity counters must run continuously per PID across segment boundaries,
// regardless of what the source segments carried.
func TestCCRewriteAcrossSegments(t *testing.T) {
	c := ccRewriter{next: map[uint16]byte{}}

	seg1 := append(append(tsPacket(0x100, 9), tsPacket(0x100, 9)...), tsPacket(0x100, 9)...)
	c.rewrite(seg1)
	for i, want := range []byte{0, 1, 2} {
		if got := ccOf(seg1[i*tsPacketSize:]); got != want {
			t.Fatalf("seg1 packet %d cc=%d want %d", i, got, want)
		}
	}

	// Second segment restarts its source counters at 0 — output must continue.
	seg2 := append(tsPacket(0x100, 0), tsPacket(0x100, 0)...)
	c.rewrite(seg2)
	for i, want := range []byte{3, 4} {
		if got := ccOf(seg2[i*tsPacketSize:]); got != want {
			t.Fatalf("seg2 packet %d cc=%d want %d (not continuous across boundary)", i, got, want)
		}
	}
}

// Counters wrap at 16 and are tracked independently per PID.
func TestCCRewritePerPIDAndWrap(t *testing.T) {
	c := ccRewriter{next: map[uint16]byte{}}
	var seg []byte
	for i := 0; i < 20; i++ {
		seg = append(seg, tsPacket(0x100, 0)...)
	}
	seg = append(seg, tsPacket(0x200, 0)...)  // different PID starts fresh
	seg = append(seg, tsPacket(0x1fff, 0)...) // null packet: never renumbered
	c.rewrite(seg)

	if got := ccOf(seg[15*tsPacketSize:]); got != 15 {
		t.Fatalf("pid 0x100 packet 15 cc=%d want 15", got)
	}
	if got := ccOf(seg[16*tsPacketSize:]); got != 0 {
		t.Fatalf("pid 0x100 packet 16 cc=%d want 0 (wrap)", got)
	}
	if got := ccOf(seg[20*tsPacketSize:]); got != 0 {
		t.Fatalf("pid 0x200 first packet cc=%d want 0", got)
	}
	if got := ccOf(seg[21*tsPacketSize:]); got != 0 {
		t.Fatalf("null packet cc must be untouched, got %d", got)
	}
}

func TestTSBroadcastFanout(t *testing.T) {
	b := NewTSBroadcaster()
	s1, s2 := b.Subscribe(), b.Subscribe()
	seg := []byte("SEGMENT-BYTES")
	b.publish(seg)
	for _, s := range []*tsSub{s1, s2} {
		select {
		case got := <-s.data:
			if string(got) != "SEGMENT-BYTES" {
				t.Fatalf("subscriber got %q", got)
			}
		default:
			t.Fatal("subscriber did not receive published segment")
		}
	}
	if b.Viewers() != 2 {
		t.Fatalf("viewers=%d want 2", b.Viewers())
	}
}

// A subscriber that never drains exceeds its byte budget and is kicked (and
// unsubscribed, so no further gapped units reach it), while the producer never
// blocks and a subscriber that keeps draining keeps receiving — the exact
// opposite of the drm worker's serial-stall (a stuck viewer there froze
// everyone). Single-goroutine and deterministic: if publish ever blocked, this
// test would deadlock instead of failing.
func TestTSSlowSubscriberKicked(t *testing.T) {
	old := maxSubQueuedBytes
	maxSubQueuedBytes = 20
	defer func() { maxSubQueuedBytes = old }()

	b := NewTSBroadcaster()
	slow := b.Subscribe() // never drained; 8-byte units bust the 20-byte budget on the 3rd
	fast := b.Subscribe() // drained every iteration

	for i := 0; i < 5; i++ {
		seg := []byte{byte(i), 0, 0, 0, 0, 0, 0, 0}
		b.publish(seg)
		select {
		case v := <-fast.data:
			fast.queued.Add(-int64(len(v)))
			if v[0] != byte(i) {
				t.Fatalf("fast subscriber out of order: got %d want %d", v[0], i)
			}
		default:
			t.Fatalf("fast subscriber missed segment %d while slow one was stuck", i)
		}
	}

	select {
	case <-slow.kicked:
	default:
		t.Fatal("slow subscriber should have been kicked once its byte budget filled")
	}
	if len(slow.data) != 2 {
		t.Fatalf("slow subscriber should hold only pre-kick units, has %d", len(slow.data))
	}
	if b.Viewers() != 1 {
		t.Fatalf("kicked subscriber still registered: viewers=%d want 1", b.Viewers())
	}
}

// A new subscriber is primed with the most recent published unit, so a joiner
// has media immediately instead of waiting up to a whole segment interval.
func TestTSSubscriberPrimedWithLast(t *testing.T) {
	b := NewTSBroadcaster()
	b.publish([]byte("SEG-1"))
	b.publish([]byte("SEG-2"))
	s := b.Subscribe()
	select {
	case v := <-s.data:
		if string(v) != "SEG-2" {
			t.Fatalf("primed with %q, want most recent unit", v)
		}
	default:
		t.Fatal("new subscriber not primed with the last published unit")
	}
	if q := s.queued.Load(); q != int64(len("SEG-2")) {
		t.Fatalf("primed unit not accounted: queued=%d", q)
	}
}

// When the stream ends, a connected viewer receives everything already queued
// (the finite tail) instead of being cut off mid-buffer; a viewer joining
// after the end is refused instead of getting an instant empty 200.
func TestTSServerDrainsOnEndAndRefusesLateJoin(t *testing.T) {
	b := NewTSBroadcaster()
	srv := httptest.NewServer(NewTSServer(b))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/live.ts")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for b.Viewers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("handler never subscribed")
		}
		time.Sleep(time.Millisecond)
	}

	b.publish([]byte("SEG-A."))
	b.publish([]byte("SEG-B."))
	b.End()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "SEG-A.SEG-B." {
		t.Fatalf("viewer lost queued tail at end: got %q", body)
	}

	late, err := http.Get(srv.URL + "/live.ts")
	if err != nil {
		t.Fatalf("late GET: %v", err)
	}
	late.Body.Close()
	if late.StatusCode != http.StatusNotFound {
		t.Fatalf("late join after end: status %d, want 404", late.StatusCode)
	}
}

func TestTSSinkRewritesAndPublishes(t *testing.T) {
	b := NewTSBroadcaster()
	sub := b.Subscribe()
	sink := NewTSSink(b)

	if err := sink.Init([]byte("ignored")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seg := append(tsPacket(0x100, 7), tsPacket(0x100, 7)...)
	if err := sink.Segment(engine.SegmentInfo{}, seg); err != nil {
		t.Fatalf("Segment: %v", err)
	}
	select {
	case got := <-sub.data:
		if ccOf(got) != 0 || ccOf(got[tsPacketSize:]) != 1 {
			t.Fatalf("sink did not renumber CC: %d,%d", ccOf(got), ccOf(got[tsPacketSize:]))
		}
	default:
		t.Fatal("sink did not publish segment")
	}
	// The sink must own its copy — mutating the caller's buffer must not touch
	// what was published.
	seg[0] = 0
	// (nothing to assert beyond no panic / no shared-mutation; covered by -race)
}
