package engine

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
)

func TestStripFakeImageHeader(t *testing.T) {
	ts := make([]byte, 188*3)
	for i := 0; i < 3; i++ {
		ts[i*188] = 0x47
	}
	fake := append([]byte{0x89, 'P', 'N', 'G', 0, 0, 0, 0, 1, 2, 3}, ts...)
	got := stripFakeImageHeader(fake)
	if !bytes.Equal(got, ts) {
		t.Fatalf("PNG header not stripped: %d vs %d bytes", len(got), len(ts))
	}
	// clean data untouched
	if !bytes.Equal(stripFakeImageHeader(ts), ts) {
		t.Fatal("clean TS modified")
	}
}

func TestDecryptAES128(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := make([]byte, 16)
	plain := []byte("hello segment data")
	// pad + encrypt
	pad := 16 - len(plain)%16
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, _ := aes.NewCipher(key)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, padded)

	got, err := decryptAES128CBC(enc, key, iv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("got %q want %q", got, plain)
	}
}

// End-to-end: segments served over HTTP, one flaky segment (fails twice then
// succeeds), verify ordered output and resume-state cleanup.
func TestDownloadStreamOrderAndRetry(t *testing.T) {
	var flakes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seg2" && flakes.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 5; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	client, _ := httpx.New(httpx.Options{Retries: 5})
	out := filepath.Join(t.TempDir(), "out.ts")
	err := DownloadStream(context.Background(), Config{Client: client, Threads: 3}, st, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	want := "[/seg0][/seg1][/seg2][/seg3][/seg4]"
	if string(b) != want {
		t.Fatalf("out = %q want %q", b, want)
	}
	if _, err := os.Stat(out + ".m314dl-state"); !os.IsNotExist(err) {
		t.Fatal("state file should be removed on success")
	}
}

func TestDownloadStreamVODFailNoHole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seg1" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 3; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	client, _ := httpx.New(httpx.Options{Retries: 1})
	out := filepath.Join(t.TempDir(), "out.ts")
	err := DownloadStream(context.Background(), Config{Client: client, Threads: 2}, st, out, nil)
	if err == nil {
		t.Fatal("want error for 404 segment")
	}
	// only the contiguous prefix before the failure may be committed
	b, _ := os.ReadFile(out)
	if string(b) != "[/seg0]" {
		t.Fatalf("committed %q, want only seg0", b)
	}
}

func TestDRMRefused(t *testing.T) {
	st := &manifest.Stream{ID: "drm", Segments: []manifest.Segment{
		{URL: "http://x/seg0", Key: &manifest.Key{Method: manifest.EncCENC}},
	}}
	client, _ := httpx.New(httpx.Options{})
	err := DownloadStream(context.Background(), Config{Client: client}, st, filepath.Join(t.TempDir(), "o"), nil)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("DRM")) {
		t.Fatalf("want DRM refusal, got %v", err)
	}
}

func TestTempStreamPathIsolation(t *testing.T) {
	st := &manifest.Stream{Type: manifest.Video, ID: "video=3000000"}
	// two episodes in the same season directory, same manifest stream id
	a := TempStreamPath("/lib/Show/Season 01/E01.mkv", st, ".mp4")
	b := TempStreamPath("/lib/Show/Season 01/E02.mkv", st, ".mp4")
	if a == b {
		t.Fatalf("concurrent episodes must not share a temp path: both %q", a)
	}
	// stable across reruns of the same command (resume relies on this)
	if a2 := TempStreamPath("/lib/Show/Season 01/E01.mkv", st, ".mp4"); a2 != a {
		t.Fatalf("temp path not stable: %q vs %q", a, a2)
	}
	// lives beside the output, not in some scratch dir
	if got := filepath.Dir(a); got != "/lib/Show/Season 01" {
		t.Fatalf("temp not beside output: %q", got)
	}
}
