package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
)

// rangedStream slices a body of n bytes into chunk-sized byte-range segments,
// the shape source.fileMaster produces for a direct file.
func rangedStream(u string, n, chunk int) *manifest.Stream {
	st := &manifest.Stream{ID: "file", Type: manifest.Unknown, Name: "f.bin"}
	for i, start := 0, 0; start < n; i, start = i+1, start+chunk {
		end := start + chunk - 1
		if end >= n {
			end = n - 1
		}
		st.Segments = append(st.Segments, manifest.Segment{URL: u, Range: &manifest.ByteRange{Start: int64(start), End: int64(end)}, Seq: int64(i)})
	}
	return st
}

// fileBody opens with a PNG magic and has TS sync bytes 188 apart: the
// fake-image-header strip would cut it if the file path weren't verbatim.
func fileBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13 ^ i>>7)
	}
	copy(b, "\x89PNG\r\n\x1a\n")
	for _, i := range []int{50, 238, 426} {
		b[i] = 0x47
	}
	return b
}

func assertFile(t *testing.T, out string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("output mismatch: %d bytes (err %v), want %d", len(got), err, len(want))
	}
	if left, _ := filepath.Glob(out + ".*"); len(left) != 0 {
		t.Fatalf("leftover files: %v", left)
	}
}

// Slicing server: parallel ranged chunks assemble byte-exact, verbatim.
func TestDownloadFileRanged(t *testing.T) {
	body := fileBody(5000)
	var ranges sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges.Store(r.Header.Get("Range"), true)
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	out := filepath.Join(t.TempDir(), "f.bin")
	if err := DownloadFile(context.Background(), Config{Client: client, Threads: 3}, rangedStream(srv.URL, 5000, 1000), out); err != nil {
		t.Fatal(err)
	}
	assertFile(t, out, body)
	for _, want := range []string{"bytes=0-999", "bytes=4000-4999"} {
		if _, ok := ranges.Load(want); !ok {
			t.Fatalf("chunk %s never requested", want)
		}
	}
}

// A server that passed the probe but answers chunks with the whole object:
// the ranged run refuses to corrupt, and the fallback completes the file on
// one connection from whatever prefix was committed.
func TestDownloadFileRangeIgnoredFallsBack(t *testing.T) {
	body := fileBody(5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body) // 200, Range ignored
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	st := rangedStream(srv.URL, 5000, 1000)
	out := filepath.Join(t.TempDir(), "f.bin")
	err := DownloadStream(context.Background(), Config{Client: client, Threads: 2, Verbatim: true}, st, out, nil)
	if !errors.Is(err, ErrRangeIgnored) {
		t.Fatalf("ranged run should fail with ErrRangeIgnored, got %v", err)
	}
	if got, _ := os.ReadFile(out); len(got) > 1000 || !bytes.Equal(got, body[:len(got)]) {
		t.Fatalf("ranged run wrote %d bytes that aren't the file's prefix", len(got))
	}
	if err := DownloadFile(context.Background(), Config{Client: client, Threads: 2}, st, out); err != nil {
		t.Fatal(err)
	}
	assertFile(t, out, body)
}

// Resume with ranged chunks: committed prefix + checkpoint + a partial part
// file complete byte-exact, fetching only what is missing.
func TestDownloadFileRangedResume(t *testing.T) {
	body := fileBody(5000)
	var ranges sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges.Store(r.Header.Get("Range"), true)
		http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "f.bin")
	// prior run: chunks 0-1 committed, 300 bytes of chunk 2 in its part file
	os.WriteFile(out, body[:2000], 0o644)
	os.WriteFile(out+".m314dl-state", []byte(`{"next_idx":2,"offset":2000}`), 0o644)
	os.WriteFile(partPath(out, 2), body[2000:2300], 0o644)

	client, _ := httpx.New(httpx.Options{Retries: 1})
	if err := DownloadFile(context.Background(), Config{Client: client, Threads: 2}, rangedStream(srv.URL, 5000, 1000), out); err != nil {
		t.Fatal(err)
	}
	assertFile(t, out, body)
	if _, ok := ranges.Load("bytes=2300-2999"); !ok {
		t.Fatal("chunk 2 should resume from its partial part file")
	}
	for _, done := range []string{"bytes=0-999", "bytes=1000-1999"} {
		if _, ok := ranges.Load(done); ok {
			t.Fatalf("committed chunk %s re-downloaded", done)
		}
	}
}

// A 206 shorter than the slice asked for is not the whole slice: the worker
// asks again for the rest instead of committing a short chunk.
func TestDownloadFileShortRange(t *testing.T) {
	body := fileBody(3000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b)
		if b-a+1 > 600 {
			b = a + 599 // cap every range at 600 bytes
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[a : b+1])
	}))
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 3})
	out := filepath.Join(t.TempDir(), "f.bin")
	if err := DownloadFile(context.Background(), Config{Client: client, Threads: 3}, rangedStream(srv.URL, 3000, 1000), out); err != nil {
		t.Fatal(err)
	}
	assertFile(t, out, body)
}
