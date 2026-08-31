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
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestDownloadStreamResume simulates an interrupted run (checkpoint on disk)
// and verifies the rerun skips already-done segments, appends byte-exact, and
// seeds progress from the checkpoint instead of restarting at 0.
func TestDownloadStreamResume(t *testing.T) {
	var requested sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.Path, true)
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 5; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	// a prior run wrote the first two segments and checkpointed
	prefix := "[/seg0][/seg1]"
	os.WriteFile(out, []byte(prefix), 0o644)
	os.WriteFile(out+".m314dl-state", []byte(fmt.Sprintf(`{"next_idx":2,"offset":%d}`, len(prefix))), 0o644)

	prog := NewProgress(false, 0)
	client, _ := httpx.New(httpx.Options{Retries: 2})
	if err := DownloadStream(context.Background(), Config{Client: client, Threads: 2, Progress: prog}, st, out, nil); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(out)
	if string(b) != "[/seg0][/seg1][/seg2][/seg3][/seg4]" {
		t.Fatalf("resumed output = %q", b)
	}
	if _, ok := requested.Load("/seg0"); ok {
		t.Fatal("seg0 re-downloaded on resume")
	}
	if _, ok := requested.Load("/seg1"); ok {
		t.Fatal("seg1 re-downloaded on resume")
	}
	if d := prog.done.Load(); d != 5 {
		t.Fatalf("progress done = %d, want 5 (2 seeded + 3 new)", d)
	}
	if _, err := os.Stat(out + ".m314dl-state"); !os.IsNotExist(err) {
		t.Fatal("state file should be removed on success")
	}
}

// TestIntraSegmentResume proves byte-range resume within a single segment: a
// partial part file on disk is completed with a Range request, not re-fetched
// from scratch.
func TestIntraSegmentResume(t *testing.T) {
	content := func(i int) []byte { return bytes.Repeat([]byte{byte('A' + i)}, 100) }
	var ranges sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var i int
		fmt.Sscanf(r.URL.Path, "/seg%d", &i)
		full := content(i)
		if rh := r.Header.Get("Range"); rh != "" {
			ranges.Store(r.URL.Path, rh)
			var start int
			fmt.Sscanf(rh, "bytes=%d-", &start)
			if start >= len(full) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(full)-1, len(full)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[start:])
			return
		}
		w.Write(full)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 3; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	// a prior run got 30 of seg1's 100 bytes before being killed
	if err := os.WriteFile(partPath(out, 1), content(1)[:30], 0o644); err != nil {
		t.Fatal(err)
	}

	client, _ := httpx.New(httpx.Options{Retries: 3})
	if err := DownloadStream(context.Background(), Config{Client: client, Threads: 2}, st, out, nil); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(out)
	want := append(append(content(0), content(1)...), content(2)...)
	if !bytes.Equal(b, want) {
		t.Fatalf("output mismatch: got %d bytes, want %d", len(b), len(want))
	}
	// seg1 resumed from byte 30; seg0/seg2 fetched fresh (no Range)
	if rh, ok := ranges.Load("/seg1"); !ok || rh.(string) != "bytes=30-" {
		t.Fatalf("seg1 should resume with Range bytes=30-, got %v", rh)
	}
	if _, ok := ranges.Load("/seg0"); ok {
		t.Fatal("seg0 should not be range-requested")
	}
}

// TestProgressTotalUpfront proves the segment total is published before
// downloads complete, so the percentage denominator is final from the first
// tick instead of inflating as segments trickle out (which slid the bar back).
func TestProgressTotalUpfront(t *testing.T) {
	gate := make(chan struct{})
	reached := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		<-gate // hold every segment until the test releases
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < 5; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: 1,
		})
	}
	out := filepath.Join(t.TempDir(), "out.ts")
	prog := NewProgress(false, 0)
	client, _ := httpx.New(httpx.Options{Retries: 1})
	done := make(chan error, 1)
	go func() {
		done <- DownloadStream(context.Background(), Config{Client: client, Threads: 2, Progress: prog}, st, out, nil)
	}()

	<-reached // a worker is fetching, so feed has already published the total
	if tot := prog.total.Load(); tot != 5 {
		t.Fatalf("total = %d before any segment completed, want 5 upfront", tot)
	}
	if d := prog.done.Load(); d != 0 {
		t.Fatalf("done = %d while segments are blocked, want 0", d)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if prog.total.Load() != 5 || prog.done.Load() != 5 {
		t.Fatalf("final total/done = %d/%d, want 5/5", prog.total.Load(), prog.done.Load())
	}
}

type countingSink struct{ n atomic.Int64 }

func (s *countingSink) Init([]byte) error { return nil }
func (s *countingSink) Segment(SegmentInfo, []byte) error {
	s.n.Add(1)
	return nil
}

// PaceRealtime holds a finite restream to the source's real duration after a
// short prime burst, so a VOD can't flood the live output. A lower-bound timing
// check: the paced segments cannot finish faster than their own durations.
func TestSinkWriterPaceRealtime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	defer srv.Close()

	const n = 8
	const dur = 0.05 // seconds/segment
	st := &manifest.Stream{ID: "t", Type: manifest.Video}
	for i := 0; i < n; i++ {
		st.Segments = append(st.Segments, manifest.Segment{
			URL: fmt.Sprintf("%s/seg%d", srv.URL, i), Seq: int64(i), Duration: dur,
		})
	}
	client, _ := httpx.New(httpx.Options{})
	sink := &countingSink{}

	start := time.Now()
	err := DownloadStream(context.Background(),
		Config{Client: client, Threads: 4, Sink: sink, PaceRealtime: true},
		st, filepath.Join(t.TempDir(), "o"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sink.n.Load() != n {
		t.Fatalf("emitted %d segments, want %d", sink.n.Load(), n)
	}
	// The prime burst is free; the rest are paced to dur each.
	wantMin := time.Duration(float64(n-pacePrimeSegments) * dur * float64(time.Second))
	if got := time.Since(start); got < wantMin {
		t.Fatalf("paced run took %v, want at least %v", got, wantMin)
	}
}
