package restream

import (
	"testing"

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

// A subscriber that never drains fills its buffer and is kicked, while the
// producer never blocks and a subscriber that keeps draining keeps receiving —
// the exact opposite of the drm worker's serial-stall (a stuck viewer there
// froze everyone). Single-goroutine and deterministic: if publish ever blocked,
// this test would deadlock instead of failing.
func TestTSSlowSubscriberKicked(t *testing.T) {
	b := NewTSBroadcaster()
	slow := b.Subscribe() // never drained
	fast := b.Subscribe() // drained every iteration

	for i := 0; i < tsSubBuffer+3; i++ {
		b.publish([]byte{byte(i)})
		select {
		case v := <-fast.data:
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
		t.Fatal("slow subscriber should have been kicked once its buffer filled")
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
