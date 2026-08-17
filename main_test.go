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

func TestParseKeys(t *testing.T) {
	keys, err := parseKeys([]string{"00112233445566778899aabbccddeeff:0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	var kid [16]byte
	copy(kid[:], []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
	if len(keys[kid]) != 16 {
		t.Fatalf("key not stored under KID: %v", keys)
	}
	// dashes in KID tolerated
	if _, err := parseKeys([]string{"00112233-4455-6677-8899-aabbccddeeff:0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("dashed KID should parse: %v", err)
	}
	// bare key → zero KID
	bare, err := parseKeys([]string{"0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bare[[16]byte{}]) != 16 {
		t.Fatal("bare key should be stored under the zero KID")
	}
	// bad lengths rejected
	if _, err := parseKeys([]string{"tooshort:0123456789abcdef0123456789abcdef"}); err == nil {
		t.Fatal("short KID should error")
	}
	if _, err := parseKeys([]string{"abcd"}); err == nil {
		t.Fatal("short key should error")
	}
}
