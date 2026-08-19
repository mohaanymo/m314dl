package bbts

import (
	"bytes"
	"testing"
)

// A TS with no "mdcm|" SDT never goes "ready", so Decrypt must pass every packet
// through byte-for-byte. This exercises the packet loop + PSI push without
// needing a real encrypted sample.
func TestClearStreamPassesThrough(t *testing.T) {
	// 3 sync-prefixed packets: a PAT, an SDT (no mdcm), and a video packet.
	pkt := func(pid uint16, pusi bool) []byte {
		p := make([]byte, tsPkt)
		p[0] = sync
		p[1] = byte(pid >> 8)
		if pusi {
			p[1] |= 0x40
		}
		p[2] = byte(pid & 0xFF)
		p[3] = 0x10 // AFC=1 (payload only)
		for i := 4; i < tsPkt; i++ {
			p[i] = byte(i)
		}
		return p
	}
	in := bytes.Join([][]byte{
		pkt(pidPAT, true),
		pkt(pidSDT, true),
		pkt(videoPID, true),
	}, nil)

	out, err := Decrypt(in, make([]byte, 16))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("clear stream altered: in %d bytes, out %d bytes", len(in), len(out))
	}
}

func TestBadKeyLength(t *testing.T) {
	if _, err := Decrypt(nil, make([]byte, 8)); err == nil {
		t.Fatal("expected error for 8-byte key")
	}
}

// splitPipe/hexBytes underpin IV extraction; check the exact "mdcm|..." shape.
func TestHexBytesAndSplit(t *testing.T) {
	parts := splitPipe("mdcm|prov|svc|Xdeadbeefdeadbeefdeadbeefdeadbeef")
	if len(parts) != 4 || parts[0] != "mdcm" {
		t.Fatalf("splitPipe: %#v", parts)
	}
	b, err := hexBytes(parts[3][1:], 16) // drop leading marker char
	if err != nil || len(b) != 16 || b[0] != 0xde {
		t.Fatalf("hexBytes: %v %x", err, b)
	}
}
