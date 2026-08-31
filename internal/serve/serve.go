// Restream mode (-serve): download the selected streams and republish them live
// over HTTP instead of writing a file. Two output shapes share one run loop:
//
//   - HLS (-serve-format hls, default): a multivariant playlist plus a rolling
//     window of segments, one media playlist per track.
//   - MPEG-TS (-serve-format ts): a single continuous transport stream fanned
//     out to every viewer. Requires one muxed TS source track.
package serve

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/mux"
	"github.com/mohamed/m314dl/internal/restream"
	"github.com/mohamed/m314dl/internal/source"
)

// Options is the subset of the CLI flags that shapes a restream: where to
// listen and what to publish.
type Options struct {
	Addr       string // listen address (-serve)
	Format     string // hls | ts | dash (-serve-format)
	Transcode  string // ffmpeg output args for the ts remux path (-serve-transcode)
	FFmpegPath string // -ffmpeg
}

// Presentation is the output-format-agnostic surface the run loop needs: how to
// finish the stream and how to describe its liveness.
type Presentation interface {
	End()
	StatusLine() string
}

// job is one source stream wired to the sink that publishes it.
type Job struct {
	st   *manifest.Stream
	sink engine.Sink
	tmp  string
}

// runRestream builds the output for the selected streams and hands off to the
// blocking run loop (the one-shot `-serve` mode).
func Run(ctx context.Context, o Options, client *httpx.Client, kind string,
	selected []*manifest.Stream, keys map[[16]byte][]byte, bbtsKey []byte,
	threadCeiling int, logv func(string, ...any)) error {

	tmpDir, err := os.MkdirTemp("", "m314dl-serve-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	pres, handler, path, jobs, err := BuildOutputs(o, selected, tmpDir, logv)
	if err != nil {
		return fmt.Errorf("restream: %w", err)
	}
	for _, j := range jobs {
		fmt.Fprintf(os.Stderr, "restream track %s\n", j.st)
	}
	return serve(ctx, o, client, kind, jobs, pres, handler, path, keys, bbtsKey, threadCeiling, logv)
}

// buildOutputs turns selected streams into a ready-to-run output: the
// Presentation (End/StatusLine), the HTTP handler that serves it, the media
// path, and the per-stream download jobs (part files under tmpDir). It is the
// single place that maps -serve-format onto a packager, shared by the one-shot
// `-serve` mode and the multi-channel worker. Subtitles are skipped (no live
// WebVTT segmentation yet).
func BuildOutputs(o Options, selected []*manifest.Stream, tmpDir string, logv func(string, ...any)) (Presentation, http.Handler, string, []Job, error) {
	var streams []*manifest.Stream
	for _, st := range selected {
		if st.Type == manifest.Subtitles {
			logv("restream: skipping subtitle track %s (not supported yet)", st.ID)
			continue
		}
		streams = append(streams, st)
	}
	if len(streams) == 0 {
		return nil, nil, "", nil, fmt.Errorf("no video/audio streams selected")
	}

	switch o.Format {
	case "ts", "mpegts":
		if pure := singleMuxedTS(streams); pure != nil && o.Transcode == "" {
			// Source is already a muxed TS: copy segments straight out, no ffmpeg.
			b := restream.NewTSBroadcaster()
			jobs := []Job{{st: pure, sink: restream.NewTSSink(b), tmp: filepath.Join(tmpDir, "ts"+source.RawExt(pure))}}
			return b, restream.NewTSServer(b), "/live.ts", jobs, nil
		}
		// fMP4 source, separate audio, or a transcode was asked for: remux with
		// one long-lived ffmpeg feeding the broadcaster.
		ffmpeg, err := mux.FindFFmpeg(o.FFmpegPath)
		if err != nil {
			return nil, nil, "", nil, fmt.Errorf("ts output needs remuxing but ffmpeg not found (pass -ffmpeg <path>): %w", err)
		}
		var targs []string
		if o.Transcode != "" {
			targs = strings.Fields(o.Transcode)
		}
		b := restream.NewTSBroadcaster()
		rem, err := restream.NewRemuxTS(ffmpeg, len(streams), targs, b, logv)
		if err != nil {
			return nil, nil, "", nil, err
		}
		var jobs []Job
		for i, st := range streams {
			jobs = append(jobs, Job{st: st, sink: rem.Sink(i), tmp: filepath.Join(tmpDir, fmt.Sprintf("in%d", i)+source.RawExt(st))})
		}
		return rem, restream.NewTSServer(b), "/live.ts", jobs, nil
	case "dash", "mpd":
		if err := requireFMP4(streams); err != nil {
			return nil, nil, "", nil, err
		}
		pub, jobs := buildPublisherJobs(streams, tmpDir)
		return pub, restream.NewServer(pub).DASHHandler(), "/live.mpd", jobs, nil
	case "", "hls":
		pub, jobs := buildPublisherJobs(streams, tmpDir)
		return pub, restream.NewServer(pub).Handler(), "/live.m3u8", jobs, nil
	default:
		return nil, nil, "", nil, fmt.Errorf("-serve-format %q: want hls, ts, or dash", o.Format)
	}
}

// buildPublisherJobs wires each stream to a track in a new Publisher (shared by
// the HLS and DASH paths, which differ only in the handler).
func buildPublisherJobs(streams []*manifest.Stream, tmpDir string) (*restream.Publisher, []Job) {
	pub := restream.NewPublisher()
	namer := newNamer()
	var jobs []Job
	for _, st := range streams {
		id := namer(st)
		sink := pub.AddTrack(restream.TrackFromStream(id, st, st.Live))
		jobs = append(jobs, Job{st: st, sink: sink, tmp: filepath.Join(tmpDir, id+source.RawExt(st))})
	}
	return pub, jobs
}

// serve runs the download jobs into their sinks while an HTTP server publishes
// the result, until the source ends or the operator interrupts.
func serve(ctx context.Context, o Options, client *httpx.Client, kind string,
	jobs []Job, pres Presentation, handler http.Handler, path string,
	keys map[[16]byte][]byte, bbtsKey []byte, threadCeiling int, logv func(string, ...any)) error {

	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return fmt.Errorf("restream: listen on %s: %w", o.Addr, err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	fmt.Fprintf(os.Stderr, "\nrestreaming live at http://%s%s\n\n", displayAddr(ln.Addr()), path)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\ninterrupt: stopping restream")
		cancel()
		<-sigCh
		os.Exit(130)
	}()

	statusStop := make(chan struct{})
	go statusLoop(pres, statusStop)

	// On cancel, end the presentation immediately. For the ffmpeg remux path this
	// closes the input pipes, so a download goroutine blocked writing to a stalled
	// ffmpeg unblocks with EPIPE and g.Wait can return. End is idempotent, so the
	// post-Wait End below is harmless.
	go func() {
		<-ctx.Done()
		pres.End()
	}()

	err = RunJobs(ctx, client, kind, jobs, keys, bbtsKey, threadCeiling, logv)
	close(statusStop)
	pres.End()

	shutdown := func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		srv.Shutdown(shutCtx)
	}

	// A real download error (not an interrupt) is fatal.
	if err != nil && ctx.Err() == nil {
		shutdown()
		return err
	}
	// A finite source finished publishing on its own: keep serving the completed
	// stream (HLS now carries EXT-X-ENDLIST) until the operator interrupts.
	if ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "source complete — serving until Ctrl-C")
		<-ctx.Done()
	}
	shutdown()
	fmt.Fprintln(os.Stderr, "restream stopped")
	return nil
}

// RunJobs downloads every job's stream into its sink until the sources end or
// ctx is cancelled. It is the shared run loop of the one-shot `-serve` mode and
// the worker's per-channel goroutine.
func RunJobs(ctx context.Context, client *httpx.Client, kind string, jobs []Job,
	keys map[[16]byte][]byte, bbtsKey []byte, threadCeiling int, logv func(string, ...any)) error {

	g, gctx := errgroup.WithContext(ctx)
	for _, j := range jobs {
		j := j
		g.Go(func() error {
			cfg := engine.Config{
				Client: client, Threads: threadCeiling, Keys: keys, BBTSKey: bbtsKey,
				Verbose: logv, Sink: j.sink,
				// A finite source downloads flat-out; pace it to real time so it
				// doesn't flood the live output (TS viewer buffers, packager window).
				PaceRealtime: !j.st.Live,
			}
			refresh := source.RefreshFunc(client, kind, j.st, logv)
			if !j.st.Live {
				refresh = nil // finite source: publish it once, then finish
			}
			if err := engine.DownloadStream(gctx, cfg, j.st, j.tmp, refresh); err != nil {
				return fmt.Errorf("track %s: %w", j.st.ID, err)
			}
			return nil
		})
	}
	return g.Wait()
}

func statusLoop(pres Presentation, stop <-chan struct{}) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if line := pres.StatusLine(); line != "" {
				fmt.Fprintf(os.Stderr, "live: %s\n", line)
			}
		}
	}
}

// singleMuxedTS returns the one muxed TS video track that the pure-Go path can
// copy straight out, or nil when the selection needs FFmpeg remuxing (an fMP4
// source, or a separate audio track to mux in).
func singleMuxedTS(streams []*manifest.Stream) *manifest.Stream {
	if len(streams) != 1 {
		return nil // separate audio (or more) → must be muxed together → remux
	}
	st := streams[0]
	if st.Type != manifest.Video {
		return nil
	}
	if st.Init != nil || segmentIsFMP4TS(st) {
		return nil // fMP4 → remux to TS
	}
	return st
}

// requireFMP4 rejects a DASH-output selection that contains a TS track: DASH
// segments must be fMP4, and remuxing TS→fMP4 is a later phase.
func requireFMP4(streams []*manifest.Stream) error {
	for _, st := range streams {
		if st.Init == nil && !segmentIsFMP4TS(st) {
			return fmt.Errorf("restream -serve-format dash: track %s is MPEG-TS, not fMP4; DASH output needs an fMP4 source (TS→fMP4 remux is a later phase)", st.ID)
		}
	}
	return nil
}

func segmentIsFMP4TS(st *manifest.Stream) bool {
	if len(st.Segments) == 0 {
		return false
	}
	u := st.Segments[0].URL
	return strings.Contains(u, ".m4s") || strings.Contains(u, ".cmf")
}

// newNamer assigns short, URL-safe, unique track ids: "video", "video2",
// "audio", "audio-fr", …. Readable ids make the playlist URLs self-describing.
func newNamer() func(*manifest.Stream) string {
	used := map[string]int{}
	return func(st *manifest.Stream) string {
		base := st.Type.String()
		if base == "subtitles" {
			base = "sub"
		}
		if st.Type != manifest.Video && st.Language != "" {
			base += "-" + source.SanitizeTag(st.Language)
		}
		used[base]++
		if used[base] == 1 {
			return base
		}
		return fmt.Sprintf("%s%d", base, used[base])
	}
}

// displayAddr turns a listener address into something clickable, replacing a
// wildcard/empty host with localhost.
func displayAddr(addr net.Addr) string {
	s := addr.String()
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return s
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}
