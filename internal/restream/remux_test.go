package restream

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/mux"
)

// Feed a real fMP4 rendition through the FFmpeg remux and confirm valid MPEG-TS
// comes out. Exercises the pipe/fd wiring end to end; skips where ffmpeg or the
// fixtures aren't present.
func TestRemuxFMP4ToTS(t *testing.T) {
	ffmpeg, err := mux.FindFFmpeg("")
	if err != nil {
		t.Skip("ffmpeg not found")
	}
	fxDir := "../../bench/fixtures/hls-fmp4"
	initData, err := os.ReadFile(filepath.Join(fxDir, "init.mp4"))
	if err != nil {
		t.Skip("fMP4 fixtures not present")
	}

	b := NewTSBroadcaster()
	sub := b.Subscribe()
	rem, err := NewRemuxTS(ffmpeg, 1, nil, b, nil)
	if err != nil {
		t.Fatalf("NewRemuxTS: %v", err)
	}
	sink := rem.Sink(0)
	if err := sink.Init(initData); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for i := 0; i < 4; i++ {
		seg, err := os.ReadFile(filepath.Join(fxDir, fmt.Sprintf("seg%04d.m4s", i)))
		if err != nil {
			break
		}
		if err := sink.Segment(engine.SegmentInfo{}, seg); err != nil {
			t.Fatalf("Segment %d: %v", i, err)
		}
	}
	rem.End() // close inputs → ffmpeg flushes → pump publishes remaining output

	// Collect whatever FFmpeg produced (poll up to a few seconds).
	var out []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(out) < 1000 {
		select {
		case chunk := <-sub.data:
			out = append(out, chunk...)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(out) == 0 {
		t.Fatal("remux produced no output")
	}
	if out[0] != 0x47 {
		t.Fatalf("remux output is not MPEG-TS (first byte %#x)", out[0])
	}
	// Sanity: 188-byte packet alignment on the first few packets.
	for _, off := range []int{188, 376} {
		if off < len(out) && out[off] != 0x47 {
			t.Fatalf("TS packet misaligned at offset %d (%#x)", off, out[off])
		}
	}
}
