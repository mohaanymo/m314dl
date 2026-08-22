package main

import (
	"testing"

	"github.com/mohamed/m314dl/internal/manifest"
)

func TestSidecarSubPath(t *testing.T) {
	used := map[string]bool{}
	en := &manifest.Stream{Language: "en", ID: "s1"}
	got := sidecarSubPath("out.mkv", en, "srt", used)
	if got != "out.en.srt" {
		t.Fatalf("got %q, want out.en.srt", got)
	}
	used[got] = true
	// second English track must not collide with the first
	en2 := &manifest.Stream{Language: "en", ID: "s2", Name: "forced"}
	got2 := sidecarSubPath("out.mkv", en2, "srt", used)
	if got2 == got {
		t.Fatalf("second en track collided: %q", got2)
	}
	if got2 != "out.en.forced.srt" {
		t.Fatalf("got %q, want out.en.forced.srt", got2)
	}
	// no language falls back to "sub"
	if p := sidecarSubPath("v.mp4", &manifest.Stream{ID: "x"}, "vtt", map[string]bool{}); p != "v.sub.vtt" {
		t.Fatalf("got %q, want v.sub.vtt", p)
	}
}
