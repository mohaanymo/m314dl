package source

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
)

// A plain file (mp4, mkv, zip, iso, …) is downloaded by the same pipeline as a
// stream: one "stream" whose segments are consecutive byte ranges of the
// object, fetched in parallel and concatenated in order.

const (
	// fileChunk is the byte-range slice size. 4 MiB amortizes the per-request
	// cost (TCP ramp-up, headers, a part file, a checkpoint write) to well under
	// 1% at CDN speeds, keeps the ordered writer's peak memory at one slice, and
	// lets a 256 MiB file already occupy all 64 adaptive workers. Files that
	// would need more than fileMaxChunks slices get proportionally bigger ones
	// so the slice count — and the progress "segs" figure — stays readable.
	fileChunk     = 4 << 20
	fileMaxChunks = 4096
	// fileMaxSniff is how much textual body LoadManifest is willing to read to
	// look for a manifest or scrape a page; a text body bigger than any
	// playlist or web page is a file.
	fileMaxSniff = 32 << 20
	// FileKind is the kind LoadManifest returns for a direct file.
	FileKind = "file"
)

// planChunks splits size bytes into consecutive inclusive ranges covering
// [0, size).
func planChunks(size int64) []manifest.ByteRange {
	c := int64(fileChunk)
	if n := (size + c - 1) / c; n > fileMaxChunks {
		c = (size + fileMaxChunks - 1) / fileMaxChunks
	}
	var out []manifest.ByteRange
	for start := int64(0); start < size; start += c {
		end := start + c - 1
		if end >= size {
			end = size - 1
		}
		out = append(out, manifest.ByteRange{Start: start, End: end})
	}
	return out
}

// isDirectFile decides from the response headers and the first bytes of the
// body whether the input is a file to download as-is, rather than a manifest
// or a web page to scrape. A manifest is always text, so a binary body can't
// be one; a text body stays on the manifest/scrape path (a JSON or plain
// endpoint may well carry stream URLs) unless it's served as an attachment or
// is far too big to be a playlist or page.
func isDirectFile(resp *http.Response, head []byte) bool {
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Disposition")), "attachment") {
		return true
	}
	if isHTML(resp.Header.Get("Content-Type")) {
		return false
	}
	if resp.ContentLength > fileMaxSniff {
		return true
	}
	return !strings.HasPrefix(http.DetectContentType(head), "text/")
}

func isHTML(contentType string) bool {
	mt, _, _ := mime.ParseMediaType(contentType)
	return mt == "text/html" || mt == "application/xhtml+xml"
}

// fileName picks the output filename: the Content-Disposition filename, else
// the final URL's basename; the real extension is kept. Reduced to a bare
// basename so a hostile header can't point outside the output directory.
func fileName(resp *http.Response, finalURL string) string {
	name := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		name = params["filename"]
	}
	if name == "" {
		if u, err := url.Parse(finalURL); err == nil {
			name = path.Base(u.Path)
		}
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == ".." || name == string(filepath.Separator) || name == "" {
		return "m314dl-output"
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		name = name[:200-len(ext)] + ext
	}
	return name
}

// fileMaster builds the single-stream master for a direct file. When the
// length is known and a byte-range probe is honored, the stream is the object
// sliced into ranges; otherwise (unknown/chunked length, ranges ignored, or a
// file no bigger than one slice) it is one whole-object segment, which the
// engine streams on a single connection with byte-offset resume.
func fileMaster(ctx context.Context, client *httpx.Client, resp *http.Response, finalURL string, logv func(string, ...any)) *manifest.Master {
	st := &manifest.Stream{Type: manifest.Unknown, ID: "file", Name: fileName(resp, finalURL), PlaylistURL: finalURL}
	size := resp.ContentLength
	var chunks []manifest.ByteRange
	if size > fileChunk && rangesHonored(ctx, client, finalURL, size) {
		chunks = planChunks(size)
	}
	if len(chunks) < 2 {
		st.Segments = []manifest.Segment{{URL: finalURL}}
		logv("file: %s, %d bytes, single connection", st.Name, size)
	} else {
		for i := range chunks {
			st.Segments = append(st.Segments, manifest.Segment{URL: finalURL, Range: &chunks[i], Seq: int64(i)})
		}
		logv("file: %s, %d bytes in %d ranges of %d", st.Name, size, len(chunks), chunks[0].End+1)
	}
	return &manifest.Master{URL: finalURL, Streams: []*manifest.Stream{st}}
}

// rangesHonored asks for the object's first byte and checks the server slices:
// a 206 whose Content-Range total matches the length. Trusting Accept-Ranges
// alone is not enough — servers advertise it and then answer a ranged request
// with the whole object, and others honor ranges without advertising.
func rangesHonored(ctx context.Context, client *httpx.Client, u string, size int64) bool {
	resp, err := client.Open(ctx, u, "bytes=0-0")
	if err != nil {
		return false
	}
	resp.Body.Close()
	var start, end, total int64
	_, err = fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
	return resp.StatusCode == http.StatusPartialContent && err == nil && start == 0 && end == 0 && total == size
}

// FileSize is a direct-file stream's length when its ranges make it known, -1
// otherwise (single-connection stream).
func FileSize(st *manifest.Stream) int64 {
	if n := len(st.Segments); n > 0 && st.Segments[n-1].Range != nil {
		return st.Segments[n-1].Range.End + 1
	}
	return -1
}
