// Restream mode (-serve): download the selected streams and republish them as
// a live HLS presentation over HTTP, instead of writing a file. Video and audio
// tracks each feed one restream.Track through the normal engine pipeline; the
// packager renders the playlists and an HTTP server serves them.
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

// runRestream serves the selected streams as live HLS on o.serve until the
// source ends or the operator interrupts.
func runRestream(ctx context.Context, o options, client *httpx.Client, kind string,
	selected []*manifest.Stream, keys map[[16]byte][]byte, bbtsKey []byte,
	threadCeiling int, logv func(string, ...any)) error {

	// Video and audio restream cleanly by copy; subtitles need live WebVTT
	// segmentation that Phase 1 doesn't do, so skip them with a clear notice.
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

	// Part files are scratch: downloaded, decrypted, handed to the packager,
	// deleted. A private temp dir keeps them out of the working directory.
	tmpDir, err := os.MkdirTemp("", "m314dl-serve-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	pub := restream.NewPublisher()
	namer := newNamer()
	type job struct {
		st   *manifest.Stream
		sink engine.Sink
		tmp  string
	}
	var jobs []job
	for _, st := range streams {
		id := namer(st)
		tr := restream.TrackFromStream(id, st, st.Live)
		sink := pub.AddTrack(tr)
		jobs = append(jobs, job{st: st, sink: sink, tmp: filepath.Join(tmpDir, id+rawExt(st))})
		fmt.Fprintf(os.Stderr, "restream track %-10s %s\n", id, st)
	}

	srv := &http.Server{Addr: o.serve, Handler: restream.NewServer(pub).Handler()}
	ln, err := net.Listen("tcp", o.serve)
	if err != nil {
		return fmt.Errorf("restream: listen on %s: %w", o.serve, err)
	}
	go srv.Serve(ln)
	fmt.Fprintf(os.Stderr, "\nrestreaming live HLS at http://%s/live.m3u8\n\n", displayAddr(ln.Addr()))

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
	go restreamStatus(pub, statusStop)

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
				refresh = nil // finite source: publish it once, then ENDLIST
			}
			if err := engine.DownloadStream(gctx, cfg, j.st, j.tmp, refresh); err != nil {
				return fmt.Errorf("track %s: %w", j.st.ID, err)
			}
			return nil
		})
	}
	err = g.Wait()
	close(statusStop)
	pub.End()

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
	// VOD (playlists now carry EXT-X-ENDLIST) until the operator interrupts.
	if ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "source complete — serving VOD until Ctrl-C")
		<-ctx.Done()
	}
	shutdown()
	fmt.Fprintln(os.Stderr, "restream stopped")
	return nil
}

// restreamStatus prints a periodic liveness line so the operator can see it is
// working without opening a player.
func restreamStatus(pub *restream.Publisher, stop <-chan struct{}) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			var parts []string
			for _, s := range pub.Stats() {
				parts = append(parts, fmt.Sprintf("%s %d segs %s", s.ID, s.Published, fmtBitrate(s.Bitrate)))
			}
			if len(parts) > 0 {
				fmt.Fprintf(os.Stderr, "live: %s\n", strings.Join(parts, " | "))
			}
		}
	}
}

func fmtBitrate(bps int64) string {
	if bps <= 0 {
		return "?"
	}
	return fmt.Sprintf("%.1fMbps", float64(bps)/1e6)
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
