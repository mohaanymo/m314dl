package subs

import (
	"encoding/binary"
	"testing"
)

const vttConcat = `WEBVTT

00:00:01.000 --> 00:00:03.000
hello

00:00:03.000 --> 00:00:05.000
world

WEBVTT

00:00:03.000 --> 00:00:05.000
world

00:00:05.000 --> 00:00:07.000
again
`

func TestVTTConcatDedupe(t *testing.T) {
	cues := parseVTT(vttConcat)
	if len(cues) != 3 {
		t.Fatalf("cues = %d, want 3 (overlap deduped): %+v", len(cues), cues)
	}
	if cues[0].Text != "hello" || cues[2].Text != "again" {
		t.Fatalf("cues = %+v", cues)
	}
}

const ttml = `<?xml version="1.0"?>
<tt xmlns="http://www.w3.org/ns/ttml"><body><div>
<p begin="00:00:01.500" end="00:00:03.000">line one<br/>line two</p>
<p begin="4.5s" end="6s"><span>styled</span> text &amp; more</p>
</div></body></tt>`

func TestTTML(t *testing.T) {
	cues := parseTTML(ttml)
	if len(cues) != 2 {
		t.Fatalf("cues = %d", len(cues))
	}
	if cues[0].Start != 1.5 || cues[0].Text != "line one\nline two" {
		t.Fatalf("c0 = %+v", cues[0])
	}
	if cues[1].Start != 4.5 || cues[1].Text != "styled text & more" {
		t.Fatalf("c1 = %+v", cues[1])
	}
}

// broken XML (unclosed tags) must still yield cues — lenient by design
func TestTTMLLenient(t *testing.T) {
	broken := `<tt><body><p begin="1s" end="2s">ok</p><p begin="3s">no end attr` // malformed tail
	cues := parseTTML(broken)
	if len(cues) != 1 || cues[0].Text != "ok" {
		t.Fatalf("cues = %+v", cues)
	}
}

func TestSniff(t *testing.T) {
	if Sniff([]byte("WEBVTT\n\n00:00:01.000 --> 2")) != KindVTT {
		t.Fatal("vtt")
	}
	if Sniff([]byte(`<?xml version="1.0"?><tt>`)) != KindTTML {
		t.Fatal("ttml")
	}
	box := make([]byte, 16)
	binary.BigEndian.PutUint32(box, 16)
	copy(box[4:8], "styp")
	if Sniff(box) != KindFMP4 {
		t.Fatal("fmp4")
	}
}

func TestExtractMdat(t *testing.T) {
	payload := []byte("<tt><p begin=\"1s\" end=\"2s\">x</p></tt>")
	box := make([]byte, 8+len(payload)+8)
	binary.BigEndian.PutUint32(box, 8)
	copy(box[4:8], "styp")
	binary.BigEndian.PutUint32(box[8:], uint32(8+len(payload)))
	copy(box[12:16], "mdat")
	copy(box[16:], payload)
	got := ExtractMdat(box[:8+8+len(payload)])
	if string(got) != string(payload) {
		t.Fatalf("mdat = %q", got)
	}
	if !IsTTMLPayload(got) {
		t.Fatal("should look like TTML")
	}
}

func TestSRTFormat(t *testing.T) {
	if s := fmtSRTTime(3723.5); s != "01:02:03,500" {
		t.Fatalf("srt time = %s", s)
	}
}
