package mux

import "testing"

func TestFindFFmpegExplicit(t *testing.T) {
	// an explicit executable name on PATH resolves (use "sh", always present)
	if p, err := FindFFmpeg("sh"); err != nil || p == "" {
		t.Fatalf("explicit name on PATH should resolve, got %q err=%v", p, err)
	}
	// a bogus explicit path is rejected, not silently ignored
	if _, err := FindFFmpeg("/no/such/ffmpeg-xyz"); err == nil {
		t.Fatal("bogus explicit ffmpeg path should error")
	}
}
