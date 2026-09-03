package source

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/httpx"
)

func TestPlanChunks(t *testing.T) {
	check := func(size int64, wantN int) {
		t.Helper()
		got := planChunks(size)
		if len(got) != wantN {
			t.Fatalf("size %d: %d chunks, want %d", size, len(got), wantN)
		}
		var next int64
		for i, r := range got {
			if r.Start != next || r.End < r.Start {
				t.Fatalf("size %d: chunk %d = %+v, want start %d", size, i, r, next)
			}
			next = r.End + 1
		}
		if wantN > 0 && next != size {
			t.Fatalf("size %d: chunks end at %d", size, next)
		}
	}
	check(0, 0)
	check(1, 1)
	check(fileChunk, 1)     // exactly one slice
	check(fileChunk+1, 2)   // one byte of remainder
	check(3*fileChunk-1, 3) // short last slice
	if got := planChunks(3*fileChunk + 1); got[3].End-got[3].Start != 0 {
		t.Fatalf("last chunk should be the 1-byte remainder: %+v", got[3])
	}
	// huge: slice count capped, slices grown to cover
	huge := int64(fileChunk) * fileMaxChunks * 10
	if got := planChunks(huge); len(got) > fileMaxChunks || got[len(got)-1].End != huge-1 {
		t.Fatalf("huge: %d chunks, last end %d", len(got), got[len(got)-1].End)
	}
}

func TestFileName(t *testing.T) {
	cases := []struct{ cd, url, want string }{
		{"", "http://h/dl/movie.mkv?token=1", "movie.mkv"},
		{"", "http://h/dl/image.iso", "image.iso"},
		{`attachment; filename="archive.zip"`, "http://h/download?id=5", "archive.zip"},
		{`attachment; filename*=UTF-8''na%C3%AFve.mp4`, "http://h/x", "naïve.mp4"},
		{`attachment; filename="../../etc/passwd"`, "http://h/x", "passwd"},
		{"", "http://h/", "m314dl-output"},
		{"", "http://h", "m314dl-output"},
		{`attachment; filename=".."`, "http://h/", "m314dl-output"},
	}
	for _, c := range cases {
		resp := &http.Response{Header: http.Header{}}
		if c.cd != "" {
			resp.Header.Set("Content-Disposition", c.cd)
		}
		if got := fileName(resp, c.url); got != c.want {
			t.Errorf("fileName(%q, %q) = %q, want %q", c.cd, c.url, got, c.want)
		}
	}
	long := &http.Response{Header: http.Header{"Content-Disposition": {`attachment; filename="` + strings.Repeat("a", 300) + `.tar.gz"`}}}
	if got := fileName(long, ""); len(got) > 200 || !strings.HasSuffix(got, ".gz") {
		t.Fatalf("long name not capped with extension kept: %d %q", len(got), got[len(got)-10:])
	}
}

// blob is a deterministic multi-slice file body. It opens with a PNG magic and
// carries TS sync bytes 188 apart so the engine's fake-image-header strip would
// mangle it if the file path weren't verbatim.
func blob(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 ^ i>>8)
	}
	copy(b, "\x89PNG\r\n\x1a\n")
	for _, i := range []int{100, 288, 476} {
		b[i] = 0x47
	}
	return b
}

// fileServer serves the detection/download fixtures.
func fileServer(t *testing.T, big []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nv.m3u8\n"
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream") // CDNs do this
		fmt.Fprint(w, master)
	})
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><video src="/playlist.m3u8"></video></html>`)
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"src":"http://%s/playlist.m3u8"}`, r.Host)
	})
	mux.HandleFunc("/empty.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><p>nothing here</p></html>`)
	})
	mux.HandleFunc("/blob.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "blob.bin", time.Time{}, bytes.NewReader(big))
	})
	mux.HandleFunc("/redir", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/blob.bin", http.StatusFound)
	})
	mux.HandleFunc("/small.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "small.bin", time.Time{}, bytes.NewReader(big[:100]))
	})
	mux.HandleFunc("/notes.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Disposition", `attachment; filename="notes.txt"`)
		fmt.Fprint(w, "just some text\n")
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.(http.Flusher).Flush() // no Content-Length: chunked transfer
		w.Write(big)
	})
	// advertises ranges, ignores them
	mux.HandleFunc("/liar", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(len(big)))
		w.Write(big)
	})
	// honors ranges without advertising them
	mux.HandleFunc("/quiet", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		var a, b int
		if n, _ := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); n == 2 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(big)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(big[a : b+1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(big)))
		w.Write(big)
	})
	return httptest.NewServer(mux)
}

func TestLoadManifestDetect(t *testing.T) {
	big := blob(2*fileChunk + 12345) // three slices
	srv := fileServer(t, big)
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 1})
	logv := func(string, ...any) {}

	cases := []struct {
		path, kind string
		segs       int    // file: expected segment count
		name       string // file: expected filename
	}{
		{"/playlist.m3u8", "hls", 0, ""},
		{"/page.html", "hls", 0, ""},
		{"/json", "hls", 0, ""}, // textual non-HTML still scrapes
		{"/blob.bin", FileKind, 3, "blob.bin"},
		{"/redir", FileKind, 3, "blob.bin"}, // final URL names the file
		{"/small.bin", FileKind, 1, "small.bin"},
		{"/notes.txt", FileKind, 1, "notes.txt"}, // attachment beats text
		{"/chunked", FileKind, 1, "chunked"},     // unknown length
		{"/liar", FileKind, 1, "liar"},           // ranges advertised, not honored
		{"/quiet", FileKind, 3, "quiet"},         // ranges honored, not advertised
	}
	for _, c := range cases {
		m, kind, err := LoadManifest(context.Background(), client, srv.URL+c.path, logv)
		if err != nil || kind != c.kind {
			t.Errorf("%s: kind %q err %v, want %q", c.path, kind, err, c.kind)
			continue
		}
		if kind != FileKind {
			continue
		}
		st := m.Streams[0]
		if len(st.Segments) != c.segs || st.Name != c.name {
			t.Errorf("%s: %d segments name %q, want %d %q", c.path, len(st.Segments), st.Name, c.segs, c.name)
		}
		if c.segs > 1 && (st.Segments[0].Range == nil || FileSize(st) != int64(len(big))) {
			t.Errorf("%s: ranges not planned over %d bytes: %+v", c.path, len(big), st.Segments[0])
		}
		if c.segs == 1 && st.Segments[0].Range != nil {
			t.Errorf("%s: single segment should carry no range", c.path)
		}
		if c.path == "/redir" && !strings.HasSuffix(st.Segments[0].URL, "/blob.bin") {
			t.Errorf("redirect: segments should use the final URL, got %s", st.Segments[0].URL)
		}
	}
	if _, _, err := LoadManifest(context.Background(), client, srv.URL+"/empty.html", logv); err == nil {
		t.Fatal("a page with no streams should still error")
	}
}

// End to end: detection → plan → engine → byte-exact file, on a server that
// slices, one that ignores ranges, and one with no length at all.
func TestDownloadFileEndToEnd(t *testing.T) {
	big := blob(2*fileChunk + 12345)
	srv := fileServer(t, big)
	defer srv.Close()
	client, _ := httpx.New(httpx.Options{Retries: 2})
	logv := func(string, ...any) {}
	for _, p := range []string{"/blob.bin", "/liar", "/chunked", "/quiet"} {
		m, kind, err := LoadManifest(context.Background(), client, srv.URL+p, logv)
		if err != nil || kind != FileKind {
			t.Fatalf("%s: %q %v", p, kind, err)
		}
		out := filepath.Join(t.TempDir(), m.Streams[0].Name)
		if err := engine.DownloadFile(context.Background(), engine.Config{Client: client, Threads: 3}, m.Streams[0], out); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		got, _ := os.ReadFile(out)
		if !bytes.Equal(got, big) {
			t.Fatalf("%s: output differs (%d vs %d bytes)", p, len(got), len(big))
		}
		if left, _ := filepath.Glob(out + ".*"); len(left) != 0 {
			t.Fatalf("%s: leftovers %v", p, left)
		}
	}
}
