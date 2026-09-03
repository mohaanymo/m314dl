package mss_test

// End-to-end tests against a real Smooth Streaming presentation. ffmpeg's
// smoothstreaming muxer writes the genuine article — Manifest, PIFF fragments
// with tfxd/tfrf and no tfdt — so nothing here is hand-rolled except the
// PlayReady header (ffmpeg encrypts with plain CENC; the PlayReady framing is
// the parser's job and is unit-tested separately). Skipped without ffmpeg.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/mp4"
	"github.com/mohamed/m314dl/internal/mux"
	"github.com/mohamed/m314dl/internal/pick"
	"github.com/mohamed/m314dl/internal/serve"
	"github.com/mohamed/m314dl/internal/source"
)

const (
	cencKID = "00112233445566778899aabbccddeeff"
	cencKey = "0123456789abcdef0123456789abcdef"
)

func requireTools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	var err error
	if ffmpeg, err = exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if ffprobe, err = exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	return ffmpeg, ffprobe
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, out)
	}
	return string(out)
}

// x264 settings shared by every encode so the SPS/PPS (and hence the manifest's
// CodecPrivateData) are identical across fixtures.
var videoArgs = []string{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25", "-t", "4",
	"-c:v", "libx264", "-preset", "ultrafast", "-threads", "1", "-g", "25", "-keyint_min", "25",
	"-sc_threshold", "0", "-b:v", "200k"}

// genSmooth writes a one-track Smooth presentation under dir and returns its
// StreamIndex element. Video and audio are packaged separately: ffmpeg shifts
// every stream by the AAC priming delay when they share a muxer, which puts the
// first video fragment at a nonzero time its Manifest doesn't declare.
func genSmooth(t *testing.T, ffmpeg, dir, kind string) string {
	t.Helper()
	args := append([]string{"-hide_banner", "-loglevel", "error"}, videoArgs...)
	if kind == "audio" {
		args = []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-t", "4", "-c:a", "aac", "-b:a", "64k", "-ac", "2"}
	}
	args = append(args, "-f", "smoothstreaming", "-min_frag_duration", "1000000", "-window_size", "0", "-extra_window_size", "0", dir)
	run(t, ffmpeg, args...)
	m, err := os.ReadFile(filepath.Join(dir, "Manifest"))
	if err != nil {
		t.Fatal(err)
	}
	i, j := strings.Index(string(m), "<StreamIndex"), strings.Index(string(m), "</StreamIndex>")
	if i < 0 || j < 0 {
		t.Fatalf("no StreamIndex in %s", m)
	}
	return string(m[i : j+len("</StreamIndex>")])
}

// fixture builds a video+audio presentation served over HTTP and returns the
// server, the root directory, and the manifest text (video index first).
func fixture(t *testing.T, ffmpeg string) (*httptest.Server, string, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	v := genSmooth(t, ffmpeg, filepath.Join(tmp, "v"), "video")
	a := genSmooth(t, ffmpeg, filepath.Join(tmp, "a"), "audio")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"v", "a"} {
		entries, _ := os.ReadDir(filepath.Join(tmp, d))
		for _, e := range entries {
			if e.IsDir() {
				if err := os.Rename(filepath.Join(tmp, d, e.Name()), filepath.Join(root, e.Name())); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	manifest := `<?xml version="1.0" encoding="utf-8"?>
<SmoothStreamingMedia MajorVersion="2" MinorVersion="0" Duration="40000000">
` + v + "\n" + a + "\n</SmoothStreamingMedia>"
	if err := os.WriteFile(filepath.Join(root, "Manifest"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)
	return srv, root, manifest
}

func newClient(t *testing.T) *httpx.Client {
	t.Helper()
	c, err := httpx.New(httpx.Options{Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func logv(format string, args ...any) {}

// load goes through the shared dispatcher so the kind sniffing is covered too.
func load(t *testing.T, client *httpx.Client, u string) []*manifest.Stream {
	t.Helper()
	master, kind, err := source.LoadManifest(context.Background(), client, u, logv)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "mss" {
		t.Fatalf("kind %q, want mss", kind)
	}
	best, _ := pick.ParseExpr("best")
	selected := pick.Select(master.Streams, best, nil, nil)
	if len(selected) == 0 {
		t.Fatal("nothing selected")
	}
	return selected
}

// probe returns the first line of an ffprobe csv query (MPEG-TS lists a stream
// under its program as well as at the top level, so the line can repeat).
func probe(t *testing.T, ffprobe, path string, sel string, entries string) string {
	t.Helper()
	out := strings.TrimSpace(run(t, ffprobe, "-v", "error", "-select_streams", sel, "-show_entries", entries, "-of", "csv=p=0", path))
	line, _, _ := strings.Cut(out, "\n")
	return line
}

func TestE2EClearDownloadAndMux(t *testing.T) {
	ffmpeg, ffprobe := requireTools(t)
	srv, _, _ := fixture(t, ffmpeg)
	client := newClient(t)
	selected := load(t, client, srv.URL+"/Manifest")
	if len(selected) != 2 {
		t.Fatalf("selected %d streams, want video+audio", len(selected))
	}

	out := filepath.Join(t.TempDir(), "out.mp4")
	var inputs []mux.Input
	for _, st := range selected {
		raw := engine.TempStreamPath(out, st, source.RawExt(st))
		if !strings.HasSuffix(raw, ".mp4") {
			t.Fatalf("raw ext for %s: %s", st.ID, raw)
		}
		if err := engine.DownloadStream(context.Background(), engine.Config{Client: client}, st, raw, nil); err != nil {
			t.Fatalf("download %s: %v", st.ID, err)
		}
		// the raw file is a valid fMP4 on its own: synthesized init + fragments
		b, _ := os.ReadFile(raw)
		if string(b[4:8]) != "ftyp" || !bytes.Contains(b, []byte("tfdt")) {
			t.Fatalf("raw %s: no ftyp/tfdt", st.ID)
		}
		inputs = append(inputs, mux.Input{Path: raw, Type: st.Type, Language: st.Language})
	}
	if err := mux.Mux(ffmpeg, inputs, out); err != nil {
		t.Fatal(err)
	}
	if got := probe(t, ffprobe, out, "v:0", "stream=codec_name,width,height"); got != "h264,320,180" {
		t.Fatalf("video: %q", got)
	}
	if got := probe(t, ffprobe, out, "a:0", "stream=codec_name,sample_rate,channels"); got != "aac,48000,2" {
		t.Fatalf("audio: %q", got)
	}
	if got := strings.TrimSpace(run(t, ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", out)); got != "100" {
		t.Fatalf("decoded %s video frames, want 100", got)
	}
	if errs := run(t, ffmpeg, "-v", "error", "-i", out, "-f", "null", "-"); strings.TrimSpace(errs) != "" {
		t.Fatalf("decode errors:\n%s", errs)
	}
}

func TestE2ERestream(t *testing.T) {
	ffmpeg, ffprobe := requireTools(t)
	srv, _, _ := fixture(t, ffmpeg)
	client := newClient(t)

	for _, format := range []string{"hls", "dash"} {
		t.Run(format, func(t *testing.T) {
			selected := load(t, client, srv.URL+"/Manifest")
			tmp := t.TempDir()
			pres, handler, path, jobs, err := serve.BuildOutputs(serve.Options{Format: format}, selected, tmp, logv)
			if err != nil {
				t.Fatal(err)
			}
			if err := serve.RunJobs(context.Background(), client, "mss", jobs, nil, nil, 0, logv); err != nil {
				t.Fatal(err)
			}
			pres.End()
			hs := httptest.NewServer(handler)
			defer hs.Close()
			get := func(p string) []byte {
				resp, err := http.Get(hs.URL + p)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != 200 {
					t.Fatalf("GET %s: %d %s", p, resp.StatusCode, b)
				}
				return b
			}
			top := string(get(path))
			var segName string
			if format == "dash" {
				if !strings.Contains(top, "<MPD") || !strings.Contains(top, `codecs="avc1.42c00d"`) || !strings.Contains(top, `codecs="mp4a.40.2"`) {
					t.Fatalf("mpd:\n%s", top)
				}
				m := regexp.MustCompile(`startNumber="(\d+)"`).FindStringSubmatch(top)
				if m == nil {
					t.Fatalf("no startNumber in mpd:\n%s", top)
				}
				n, _ := strconv.Atoi(m[1])
				segName = fmt.Sprintf("%06d.m4s", n)
			} else {
				if !strings.Contains(top, "#EXT-X-STREAM-INF") || !strings.Contains(top, "avc1.42c00d") || !strings.Contains(top, "RESOLUTION=320x180") {
					t.Fatalf("master playlist:\n%s", top)
				}
				media := string(get("/video/index.m3u8"))
				if !strings.Contains(media, "#EXT-X-MAP:URI=\"init.mp4\"") || !strings.Contains(media, "#EXT-X-ENDLIST") {
					t.Fatalf("media playlist:\n%s", media)
				}
				segName = firstSegment(t, media)
			}
			// the first published video segment plays against the published init
			init := get("/video/init.mp4")
			seg := get("/video/" + segName)
			if mp4.SoleTrackID(init) != 1 || !bytes.Contains(seg, []byte("tfdt")) {
				t.Fatal("published init/segment not normalized")
			}
			f := filepath.Join(tmp, "check.mp4")
			os.WriteFile(f, append(init, seg...), 0o644)
			if got := probe(t, ffprobe, f, "v:0", "stream=codec_name,width,height"); got != "h264,320,180" {
				t.Fatalf("published segment: %q", got)
			}
		})
	}

	// MPEG-TS output: the fMP4 tracks are remuxed by a long-lived ffmpeg into
	// one transport stream fanned out to viewers.
	t.Run("ts", func(t *testing.T) {
		selected := load(t, client, srv.URL+"/Manifest")
		tmp := t.TempDir()
		pres, handler, path, jobs, err := serve.BuildOutputs(serve.Options{Format: "ts", FFmpegPath: ffmpeg}, selected, tmp, logv)
		if err != nil {
			t.Fatal(err)
		}
		hs := httptest.NewServer(handler)
		defer hs.Close()
		// A viewer joins the broadcast and reads until it ends — which is when
		// the sources are done and the presentation is ended, as the run loop does.
		viewer := make(chan []byte, 1)
		go func() {
			for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
				resp, err := http.Get(hs.URL + path)
				if err != nil {
					continue
				}
				if resp.StatusCode == 200 {
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					viewer <- b
					return
				}
				resp.Body.Close()
			}
			viewer <- nil
		}()
		if err := serve.RunJobs(context.Background(), client, "mss", jobs, nil, nil, 0, logv); err != nil {
			t.Fatal(err)
		}
		pres.End()
		body := <-viewer
		if len(body) == 0 || body[0] != 0x47 {
			t.Fatalf("no transport stream received (%d bytes)", len(body))
		}
		f := filepath.Join(tmp, "live.ts")
		os.WriteFile(f, body, 0o644)
		if got := probe(t, ffprobe, f, "v:0", "stream=codec_name,width,height"); got != "h264,320,180" {
			t.Fatalf("ts video: %q", got)
		}
		if got := probe(t, ffprobe, f, "a:0", "stream=codec_name"); got != "aac" {
			t.Fatalf("ts audio: %q", got)
		}
	})
}

var segRe = regexp.MustCompile(`(?m)^[^#\s]+\.m4s$`)

func firstSegment(t *testing.T, playlist string) string {
	t.Helper()
	m := segRe.FindString(playlist)
	if m == "" {
		t.Fatalf("no segment in playlist:\n%s", playlist)
	}
	return m
}

// TestE2ECENC encrypts the same video with ffmpeg's CENC (cenc-aes-ctr, 8-byte
// IVs, subsample-encrypted H.264 — the PIFF/PlayReady layout minus the uuid
// box, which the mp4 package tests cover), serves the fragments under a
// PlayReady-protected manifest, and checks the native decrypt matches ffmpeg's
// own decryption of the source bit for bit.
func TestE2ECENC(t *testing.T) {
	ffmpeg, _ := requireTools(t)
	srv, root, manifestText := fixture(t, ffmpeg)
	client := newClient(t)

	// encrypted ismv → one fragment file per moof/mdat pair, named by its tfxd time
	ismv := filepath.Join(t.TempDir(), "enc.ismv")
	args := append([]string{"-hide_banner", "-loglevel", "error"}, videoArgs...)
	args = append(args, "-encryption_scheme", "cenc-aes-ctr", "-encryption_key", cencKey, "-encryption_kid", cencKID,
		"-movflags", "+frag_keyframe", "-f", "ismv", ismv)
	run(t, ffmpeg, args...)
	b, err := os.ReadFile(ismv)
	if err != nil {
		t.Fatal(err)
	}
	var moof []byte
	n := 0
	for off := 0; off+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[off:]))
		typ := string(b[off+4 : off+8])
		if size < 8 || off+size > len(b) {
			t.Fatalf("bad box at %d", off)
		}
		switch typ {
		case "moof":
			moof = b[off : off+size]
		case "mdat":
			frag := append(append([]byte(nil), moof...), b[off:off+size]...)
			name := "Fragments(video=" + tfxdTime(t, moof) + ")"
			if err := os.WriteFile(filepath.Join(root, "QualityLevels(200000)", name), frag, 0o644); err != nil {
				t.Fatal(err)
			}
			n++
		}
		off += size
	}
	if n != 4 {
		t.Fatalf("split %d fragments, want 4 (1s GOPs over 4s)", n)
	}
	var kid [16]byte
	hex.Decode(kid[:], []byte(cencKID))
	key, _ := hex.DecodeString(cencKey)
	prot := `<Protection><ProtectionHeader SystemID="9A04F079-9840-4286-AB92-E65BE0885F95">` +
		playReadyHeader(kid, true) + `</ProtectionHeader></Protection>`
	manifestText = strings.Replace(manifestText, "\n<StreamIndex", "\n"+prot+"\n<StreamIndex", 1)
	if err := os.WriteFile(filepath.Join(root, "Manifest"), []byte(manifestText), 0o644); err != nil {
		t.Fatal(err)
	}

	selected := load(t, client, srv.URL+"/Manifest")
	var video *manifest.Stream
	for _, st := range selected {
		if st.Type == manifest.Video {
			video = st
		}
	}
	if video == nil || video.Segments[0].Key == nil || video.Segments[0].Key.KID != kid {
		t.Fatalf("video not marked CENC with the PlayReady KID: %+v", video)
	}

	raw := filepath.Join(t.TempDir(), "video.mp4")
	// no key → refused up front, nothing downloaded
	if err := engine.DownloadStream(context.Background(), engine.Config{Client: client}, video, raw, nil); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Fatalf("keyless download: %v", err)
	}
	cfg := engine.Config{Client: client, Keys: map[[16]byte][]byte{kid: key}}
	if err := engine.DownloadStream(context.Background(), cfg, video, raw, nil); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(raw)
	if info, _ := mp4.ParseInit(out); info != nil {
		t.Fatal("output init still flagged encrypted")
	}
	if bytes.Contains(out, []byte("senc")) || bytes.Contains(out, []byte("saiz")) {
		t.Fatal("output fragments still carry sample encryption boxes")
	}
	// decoded frames identical to ffmpeg's own decryption of the source
	mine := run(t, ffmpeg, "-v", "error", "-i", raw, "-f", "md5", "-")
	ref := run(t, ffmpeg, "-v", "error", "-decryption_key", cencKey, "-i", ismv, "-f", "md5", "-")
	if mine != ref || !strings.HasPrefix(mine, "MD5=") {
		t.Fatalf("decoded frames differ:\n mine %s\n  ref %s", mine, ref)
	}
}

// tfxdTime reads the PIFF fragment time out of a moof's traf.
func tfxdTime(t *testing.T, moof []byte) string {
	t.Helper()
	ext := []byte{0x6d, 0x1d, 0x9b, 0x05, 0x42, 0xd5, 0x44, 0xe6, 0x80, 0xe2, 0x14, 0x1d, 0xaf, 0xf7, 0x57, 0xb2}
	i := bytes.Index(moof, append([]byte("uuid"), ext...))
	if i < 0 {
		t.Fatal("no tfxd in fragment")
	}
	p := moof[i+4+16:]
	if p[0] == 1 {
		return strconv.FormatUint(binary.BigEndian.Uint64(p[4:]), 10)
	}
	return strconv.FormatUint(uint64(binary.BigEndian.Uint32(p[4:])), 10)
}
