// Restream mode (-serve): download the selected streams and republish them live
// over HTTP instead of writing a file. Two output shapes share one run loop:
//
//   - HLS (-serve-format hls, default): a multivariant playlist plus a rolling
//     window of segments, one media playlist per track.
//   - MPEG-TS (-serve-format ts): a single continuous transport stream fanned
//     out to every viewer. Requires one muxed TS source track.
package main

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
	"github.com/mohamed/m314dl/internal/restream"
)

// presentation is the output-format-agnostic surface the run loop needs: how to
// finish the stream and how to describe its liveness.
type presentation interface {
	End()
	StatusLine() string
}

// job is one source stream wired to the sink that publishes it.
type job struct {
	st   *manifest.Stream
	sink engine.Sink
	tmp  string
}

// runRestream selects the output format and hands off to the shared run loop.
func runRestream(ctx context.Context, o options, client *httpx.Client, kind string,
	selected []*manifest.Stream, keys map[[16]byte][]byte, bbtsKey []byte,
	threadCeiling int, logv func(string, ...any)) error {

	// Video and audio restream cleanly by copy; subtitles need live WebVTT
	// segmentation that isn't wired up yet, so skip them with a clear notice.
	var streams []*manifest.Stream
	for _, st := range selected {
		if st.Type == manifest.Subtitles {
			fmt.Fprintf(os.Stderr, "restream: skipping subtitle track %s (not supported by -serve yet)\n", st.ID)
			continue
		}
		streams = append(streams, st)
	}
	if len(streams) == 0 {
		return fmt.Errorf("restream: no video/audio streams selected")
	}

	tmpDir, err := os.MkdirTemp("", "m314dl-serve-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	var pres presentation
	var jobs []job
	var handler http.Handler
	var path string

	switch o.serveFormat {
	case "ts", "mpegts":
		st, err := singleMuxedTS(streams)
		if err != nil {
			return err
		}
		b := restream.NewTSBroadcaster()
		pres, handler, path = b, restream.NewTSServer(b), "/live.ts"
		jobs = []job{{st: st, sink: restream.NewTSSink(b), tmp: filepath.Join(tmpDir, "ts"+rawExt(st))}}
		fmt.Fprintf(os.Stderr, "restream track %-10s %s\n", "ts", st)
	case "dash", "mpd":
		if err := requireFMP4(streams); err != nil {
			return err
		}
		pub := restream.NewPublisher()
		namer := newNamer()
		for _, st := range streams {
			id := namer(st)
			sink := pub.AddTrack(restream.TrackFromStream(id, st, st.Live))
			jobs = append(jobs, job{st: st, sink: sink, tmp: filepath.Join(tmpDir, id+rawExt(st))})
			fmt.Fprintf(os.Stderr, "restream track %-10s %s\n", id, st)
		}
		pres, handler, path = pub, restream.NewServer(pub).DASHHandler(), "/live.mpd"
	case "", "hls":
		pub := restream.NewPublisher()
		namer := newNamer()
		for _, st := range streams {
			id := namer(st)
			sink := pub.AddTrack(restream.TrackFromStream(id, st, st.Live))
			jobs = append(jobs, job{st: st, sink: sink, tmp: filepath.Join(tmpDir, id+rawExt(st))})
			fmt.Fprintf(os.Stderr, "restream track %-10s %s\n", id, st)
		}
		pres, handler, path = pub, restream.NewServer(pub).Handler(), "/live.m3u8"
	default:
		return fmt.Errorf("-serve-format %q: want hls, ts, or dash", o.serveFormat)
	}

	return serve(ctx, o, client, kind, jobs, pres, handler, path, keys, bbtsKey, threadCeiling, logv)
}

// serve runs the download jobs into their sinks while an HTTP server publishes
// the result, until the source ends or the operator interrupts.
func serve(ctx context.Context, o options, client *httpx.Client, kind string,
	jobs []job, pres presentation, handler http.Handler, path string,
	keys map[[16]byte][]byte, bbtsKey []byte, threadCeiling int, logv func(string, ...any)) error {

	ln, err := net.Listen("tcp", o.serve)
	if err != nil {
		return fmt.Errorf("restream: listen on %s: %w", o.serve, err)
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

	g, gctx := errgroup.WithContext(ctx)
	for _, j := range jobs {
		j := j
		g.Go(func() error {
			cfg := engine.Config{
				Client: client, Threads: threadCeiling, Keys: keys, BBTSKey: bbtsKey,
				Verbose: logv, Sink: j.sink,
			}
			refresh := refreshFunc(client, kind, j.st, logv)
			if !j.st.Live {
				refresh = nil // finite source: publish it once, then finish
			}
			if err := engine.DownloadStream(gctx, cfg, j.st, j.tmp, refresh); err != nil {
				return fmt.Errorf("track %s: %w", j.st.ID, err)
			}
			return nil
		})
	}
	err = g.Wait()
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

func statusLoop(pres presentation, stop <-chan struct{}) {
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

// singleMuxedTS returns the one muxed TS track for continuous MPEG-TS output,
// or an error explaining why the selection can't produce one. Muxing separate
// elementary streams into TS, or remuxing fMP4 to TS, is a later phase.
func singleMuxedTS(streams []*manifest.Stream) (*manifest.Stream, error) {
	var video []*manifest.Stream
	for _, st := range streams {
		if st.Type == manifest.Video {
			video = append(video, st)
		}
	}
	if len(video) == 0 {
		return nil, fmt.Errorf("restream -serve-format ts: no video track selected")
	}
	st := video[0]
	if st.Init != nil || segmentIsFMP4TS(st) {
		return nil, fmt.Errorf("restream -serve-format ts: source is fMP4, not MPEG-TS; use -serve-format hls (fMP4→TS remux is a later phase)")
	}
	if len(streams) > 1 {
		fmt.Fprintln(os.Stderr, "restream: MPEG-TS output carries only the muxed video track; other selected tracks are ignored (separate-audio muxing is a later phase)")
	}
	return st, nil
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
			base += "-" + sanitizeTag(st.Language)
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
