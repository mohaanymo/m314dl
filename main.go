// m314dl — HLS/DASH/MSS media downloader.
//
// Usage: m314dl [flags] <URL>
// URL may be a master/media playlist (.m3u8), a DASH manifest (.mpd), a Smooth
// Streaming manifest (.ism/Manifest), a web page (m314dl scrapes it for
// stream URLs), or a plain file (mp4, mkv, zip, iso, …), which is fetched in
// parallel byte ranges with resume and written as-is.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/hls"
	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/mux"
	"github.com/mohamed/m314dl/internal/pick"
	"github.com/mohamed/m314dl/internal/rpc"
	"github.com/mohamed/m314dl/internal/serve"
	"github.com/mohamed/m314dl/internal/source"
	"github.com/mohamed/m314dl/internal/subs"
	"github.com/mohamed/m314dl/internal/worker"
)

const version = "0.3.3"

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type options struct {
	output           string
	threads          int
	headers          multiFlag
	cookies          string
	proxy            string
	insecure         bool
	retries          int
	listOnly         bool
	sv, sa, ss       string
	adKeywords       multiFlag
	liveLimit        time.Duration
	liveFromStart    bool
	noMux            bool
	keepTemp         bool
	subFormat        string
	subExternal      bool
	ffmpegPath       string
	verbose          bool
	timeout          time.Duration
	progressInterval time.Duration
	keys             multiFlag
	bbtsKey          string
	rpc              string
	rpcSecret        string
	rpcMaxJobs       int
	rpcRetain        time.Duration
	serve            string
	serveFormat      string
	serveTranscode   string
	worker           string
	workerSecret     string
	workerMaxChan    int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "m314dl: error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.StringVar(&o.output, "o", "", "output file (extension selects container; default from URL, .mp4). Direct file: used as given, default the server's/URL's filename")
	flag.IntVar(&o.threads, "t", 0, "concurrent segment downloads per stream (direct file: parallel connections) — a fixed count, held (backs off only on rate limits, then climbs back). Omit to auto-tune (up to 64)")
	flag.Var(&o.headers, "H", "custom header 'Key: Value' (repeatable)")
	flag.StringVar(&o.cookies, "cookies", "", "Netscape cookies.txt file")
	flag.StringVar(&o.proxy, "proxy", "", "proxy URL (http://, socks5://, user:pass@ ok)")
	flag.BoolVar(&o.insecure, "insecure", false, "skip TLS certificate verification")
	flag.IntVar(&o.retries, "retries", 5, "retries per request (covers mid-body failures)")
	flag.BoolVar(&o.listOnly, "list", false, "list streams and exit")
	flag.StringVar(&o.sv, "sv", "best", "video select: best|worst|all|bestN|key=regex[:...] (keys: id,lang,name,codecs,res,bwmin,bwmax)")
	flag.StringVar(&o.sa, "sa", "", "audio select (default: best per language, video's group preferred)")
	flag.StringVar(&o.ss, "ss", "", "subtitle select (default: all)")
	flag.Var(&o.adKeywords, "ad-keyword", "regex; matching segment URLs are skipped as ads (repeatable, live too)")
	flag.DurationVar(&o.liveLimit, "live-duration", 0, "stop live recording after this duration (e.g. 1h30m)")
	flag.BoolVar(&o.liveFromStart, "live-from-start", false, "live: download the whole DVR window instead of starting at the live edge")
	flag.BoolVar(&o.noMux, "no-mux", false, "keep raw per-stream files, skip ffmpeg")
	flag.StringVar(&o.ffmpegPath, "ffmpeg", "", "path to the ffmpeg binary (default: next to m314dl, then PATH)")
	flag.BoolVar(&o.keepTemp, "keep", false, "keep temp stream files after mux")
	flag.Var(&o.keys, "key", "CENC content key 'KID:KEY' (hex; KID dashes optional) or bare 'KEY' (repeatable). Enables native in-process DRM decryption — no mp4decrypt needed")
	flag.StringVar(&o.bbtsKey, "bbts-key", "", "16-byte AES key (32 hex) for BBTS-encrypted MPEG-TS segments; the per-segment IV is read from the stream's SDT")
	flag.StringVar(&o.subFormat, "sub-format", "srt", "subtitle output: srt or vtt")
	flag.BoolVar(&o.subExternal, "sub-external", false, "write subtitles as sidecar files next to the output instead of muxing them in")
	flag.BoolVar(&o.verbose, "v", false, "verbose logging")
	flag.DurationVar(&o.timeout, "timeout", 0, "per-request timeout (default none; retries handle stalls)")
	flag.DurationVar(&o.progressInterval, "progress-interval", 0, "progress refresh interval, e.g. 500ms (default: 1s on a TTY, 5s when piped)")
	flag.StringVar(&o.rpc, "rpc", "", "run as an RPC server on this address (e.g. 127.0.0.1:8314) instead of downloading; see internal/rpc for the HTTP/JSON API")
	flag.StringVar(&o.rpcSecret, "rpc-secret", "", "bearer token for -rpc clients (required when binding a non-loopback address)")
	flag.IntVar(&o.rpcMaxJobs, "rpc-max-jobs", 64, "-rpc: max concurrent jobs; further /add requests get 503 until a slot frees (0 = unlimited)")
	flag.DurationVar(&o.rpcRetain, "rpc-retain", time.Hour, "-rpc: keep finished jobs queryable at least this long before reaping (bounds memory)")
	flag.StringVar(&o.serve, "serve", "", "restream: republish the selected streams live on this address (e.g. :8314) instead of downloading to a file")
	flag.StringVar(&o.serveFormat, "serve-format", "hls", "restream output: hls (/live.m3u8), ts (continuous MPEG-TS at /live.ts), or dash (/live.mpd; needs an fMP4 source)")
	flag.StringVar(&o.serveTranscode, "serve-transcode", "", "restream ts remux: ffmpeg output codec args to transcode instead of copy, e.g. '-c:v libx264 -preset veryfast -c:a aac' (implies the ffmpeg remux path)")
	flag.StringVar(&o.worker, "worker", "", "run as a multi-channel restream worker on this address (e.g. :7001); drive it via POST /api/channels, serve /{id}/live.m3u8. See internal/worker")
	flag.StringVar(&o.workerSecret, "worker-secret", "", "bearer token for -worker control API (required when binding a non-loopback address)")
	flag.IntVar(&o.workerMaxChan, "worker-max-channels", 32, "-worker: max concurrent channels; further starts get 503 (0 = unlimited)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "m314dl %s — HLS/DASH/MSS media downloader\n\nusage: m314dl [flags] <URL>\n\n", version)
		flag.PrintDefaults()
	}
	// Flags may appear in any order relative to the URL. Go's flag package stops
	// at the first non-flag arg, so parse in a loop: keep the positional it stops
	// on and resume parsing the rest. Flags before, after, or around the URL all
	// work (m314dl -o out.mp4 URL -t 16 is the same as m314dl URL -o out.mp4 -t 16).
	var positionals []string
	rest := os.Args[1:]
	for {
		flag.CommandLine.Parse(rest)
		rest = flag.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
	if *showVersion {
		fmt.Println("m314dl", version)
		return nil
	}
	if o.rpc != "" {
		if len(positionals) != 0 {
			return fmt.Errorf("-rpc takes no URL; submit jobs via POST /add")
		}
		return rpc.ServeRPC(o.rpc, o.rpcSecret, o.rpcMaxJobs, o.rpcRetain, version)
	}
	if o.worker != "" {
		if len(positionals) != 0 {
			return fmt.Errorf("-worker takes no URL; start channels via POST /api/channels")
		}
		logv := func(format string, args ...any) {
			if o.verbose {
				fmt.Fprintf(os.Stderr, "[v] "+format+"\n", args...)
			}
		}
		return worker.ServeWorker(o.worker, o.workerSecret, o.workerMaxChan, o.ffmpegPath, version, logv)
	}
	// Adaptive concurrency unless the user pinned -t.
	tPinned := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "t" {
			tPinned = true
		}
	})
	if len(positionals) != 1 {
		flag.Usage()
		return fmt.Errorf("exactly one URL required")
	}
	inputURL := positionals[0]

	logv := func(format string, args ...any) {
		if o.verbose {
			fmt.Fprintf(os.Stderr, "[v] "+format+"\n", args...)
		}
	}

	headers := map[string]string{}
	for _, h := range o.headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("bad header %q (want 'Key: Value')", h)
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	client, err := httpx.New(httpx.Options{
		Headers: headers, Proxy: o.proxy, CookieFile: o.cookies,
		Insecure: o.insecure, Timeout: o.timeout, Retries: o.retries,
	})
	if err != nil {
		return err
	}

	var adFilters []*regexp.Regexp
	for _, k := range o.adKeywords {
		re, err := regexp.Compile(k)
		if err != nil {
			return fmt.Errorf("bad -ad-keyword %q: %w", k, err)
		}
		adFilters = append(adFilters, re)
	}

	keys, err := source.ParseKeys(o.keys)
	if err != nil {
		return err
	}

	var bbtsKey []byte
	if o.bbtsKey != "" {
		bbtsKey, err = hex.DecodeString(strings.TrimSpace(o.bbtsKey))
		if err != nil || len(bbtsKey) != 16 {
			return fmt.Errorf("bad -bbts-key: must be 32 hex chars (16 bytes)")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopLive := make(chan struct{})

	// fetch + detect manifest (scraping web pages transparently)
	master, kind, err := source.LoadManifest(ctx, client, inputURL, logv)
	if err != nil {
		return err
	}
	threadCeiling := 0 // 0 = auto-tune (no -t given)
	if tPinned {
		threadCeiling = o.threads
	}
	if kind == source.FileKind {
		return downloadFile(ctx, cancel, &o, client, master.Streams[0], threadCeiling, logv)
	}

	ve, err := pick.ParseExpr(o.sv)
	if err != nil {
		return fmt.Errorf("-sv: %w", err)
	}
	var ae, se *pick.Expr
	if o.sa != "" {
		if ae, err = pick.ParseExpr(o.sa); err != nil {
			return fmt.Errorf("-sa: %w", err)
		}
	}
	if o.ss != "" {
		if se, err = pick.ParseExpr(o.ss); err != nil {
			return fmt.Errorf("-ss: %w", err)
		}
	}

	if o.listOnly {
		pick.Sort(master.Streams)
		for _, st := range master.Streams {
			fmt.Printf("%-4s %s\n", st.ID, st)
		}
		return nil
	}

	selected := pick.Select(master.Streams, ve, ae, se)
	if len(selected) == 0 {
		return fmt.Errorf("no streams matched the selection")
	}
	for _, st := range selected {
		fmt.Fprintf(os.Stderr, "selected: %-4s %s\n", st.ID, st)
	}

	// expand HLS media playlists for selected streams only
	live := master.Live
	for _, st := range selected {
		if kind == "hls" && !st.SegmentsFull {
			body, finalURL, err := client.FetchBytes(ctx, st.PlaylistURL, "")
			if err != nil {
				return fmt.Errorf("fetch media playlist for %s: %w", st.ID, err)
			}
			st.PlaylistURL = finalURL
			if err := hls.ParseMedia(body, finalURL, st); err != nil {
				return err
			}
		}
		if st.Live {
			live = true
		}
		if len(st.Segments) == 0 {
			return fmt.Errorf("stream %s has no segments", st.ID)
		}
		if k := st.Segments[0].Key; k != nil && k.Method == manifest.EncCENC && len(keys) == 0 {
			return fmt.Errorf("stream %s is CENC/DRM-protected; supply the content key with -key KID:KEY", st.ID)
		}
	}

	// Restream mode: republish live HLS over HTTP instead of downloading a file.
	if o.serve != "" {
		return serve.Run(ctx, serve.Options{Addr: o.serve, Format: o.serveFormat, Transcode: o.serveTranscode, FFmpegPath: o.ffmpegPath},
			client, kind, selected, keys, bbtsKey, threadCeiling, logv)
	}

	outPath := outputPath(o.output, inputURL, live)
	workDir := filepath.Dir(outPath)
	ffmpeg, ffErr := mux.FindFFmpeg(o.ffmpegPath)
	if !o.noMux && ffErr != nil {
		return fmt.Errorf("ffmpeg not found (needed for muxing; pass -ffmpeg <path>, or -no-mux to skip): %w", ffErr)
	}

	interruptHandler(live, stopLive, cancel)

	prog := engine.NewProgress(live, o.progressInterval)
	progStop := make(chan struct{})
	go prog.Render(progStop)

	cfg := engine.Config{
		Client: client, Threads: threadCeiling, Keys: keys, BBTSKey: bbtsKey, AdFilters: adFilters,
		LiveLimit: o.liveLimit, Progress: prog, Verbose: logv, Stop: stopLive,
		FromStart: o.liveFromStart,
	}

	type done struct {
		st     *manifest.Stream
		path   string
		failed bool // a subtitle whose download failed — skipped, not fatal
	}
	var results []done
	for _, st := range selected {
		results = append(results, done{st: st, path: engine.TempStreamPath(outPath, st, source.RawExt(st))})
	}
	// Stream scheduling. A stream's segments are latency-bound when small
	// (subtitles: thousands of ~byte-sized segments) and bandwidth-bound when
	// large (video). With only a few streams running at once, the many tiny
	// subtitle streams serialize into batches and their round-trip latency piles
	// up *after* the video finishes — a 5–6s tail on a 41-min asset. So run many
	// streams concurrently, but cap each subtitle stream to a few workers: a
	// handful hides its latency, and the cap keeps total in-flight requests
	// bounded (streamPar × workers) even with 25 subtitle tracks and a big -t.
	const subWorkerCap = 16
	streamPar := len(results)
	if streamPar < 4 {
		streamPar = 4
	} else if streamPar > 16 {
		streamPar = 16
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(streamPar)
	tDownload := time.Now()
	for i := range results {
		r := &results[i]
		stCfg := cfg // per-stream copy (shares Client/Progress pointers)
		if r.st.Type == manifest.Subtitles && (stCfg.Threads == 0 || stCfg.Threads > subWorkerCap) {
			stCfg.Threads = subWorkerCap
		}
		g.Go(func() error {
			refresh := source.RefreshFunc(client, kind, r.st, logv)
			if !r.st.Live {
				refresh = nil
			}
			if err := engine.DownloadStream(gctx, stCfg, r.st, r.path, refresh); err != nil {
				// Subtitles are optional: a broken track (Disney+ ships dummy
				// placeholder subtitle tracks, some CDNs 404 a language) must not
				// sink a finished video+audio download. Skip it with a warning.
				// Video/audio failures stay fatal — there's no output without them.
				if r.st.Type == manifest.Subtitles {
					r.failed = true
					fmt.Fprintf(os.Stderr, "warning: subtitle %s failed to download (%v); skipping it\n", r.st.ID, err)
					return nil
				}
				return fmt.Errorf("stream %s: %w", r.st.ID, err)
			}
			return nil
		})
	}
	err = g.Wait()
	logv("phase download: %s (%d streams)", time.Since(tDownload).Round(time.Millisecond), len(results))
	tFinalize := time.Now()
	close(progStop)
	time.Sleep(50 * time.Millisecond) // let renderer print the final line
	if err != nil {
		return err
	}

	// subtitles: normalize raw payloads into srt/vtt
	var muxInputs []mux.Input
	var sidecars []string             // -sub-external: files written beside output
	wentExternal := map[string]bool{} // raw paths whose subtitle became a sidecar
	subSidecars := map[string]bool{}  // sidecar paths already used (collision guard)
	for _, r := range results {
		if r.failed {
			continue // subtitle that failed to download — already warned
		}
		if r.st.Type != manifest.Subtitles {
			muxInputs = append(muxInputs, mux.Input{
				Path: r.path, Type: r.st.Type, Language: r.st.Language,
				Name: r.st.Name, Default: r.st.Default,
			})
			continue
		}
		subPath, err := convertSubtitle(r.path, o.subFormat, ffmpeg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: subtitle %s conversion failed (%v); keeping raw file %s\n", r.st.ID, err, r.path)
			continue
		}
		if o.subExternal {
			sidecar := sidecarSubPath(outPath, r.st, o.subFormat, subSidecars)
			subSidecars[sidecar] = true
			if err := os.Rename(subPath, sidecar); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not place sidecar subtitle (%v); keeping %s\n", err, subPath)
				continue
			}
			sidecars = append(sidecars, sidecar)
			wentExternal[r.path] = true
			if !o.keepTemp {
				os.Remove(r.path) // raw payload superseded by the sidecar
			}
			fmt.Fprintln(os.Stderr, "subtitle: "+sidecar)
			continue
		}
		muxInputs = append(muxInputs, mux.Input{
			Path: subPath, Type: manifest.Subtitles, Language: r.st.Language,
			Name: r.st.Name, Default: r.st.Default,
		})
	}
	logv("phase subtitle-convert: %s", time.Since(tFinalize).Round(time.Millisecond))

	if o.noMux {
		fmt.Fprintln(os.Stderr, "done (raw streams kept):")
		for _, r := range results {
			if !wentExternal[r.path] {
				fmt.Fprintln(os.Stderr, "  "+r.path)
			}
		}
		for _, s := range sidecars {
			fmt.Fprintln(os.Stderr, "  "+s)
		}
		return nil
	}

	// Nothing to mux (e.g. subtitles-only with -sub-external): the sidecars are
	// already written, so finish without invoking ffmpeg.
	if len(muxInputs) == 0 {
		if len(sidecars) == 0 {
			return fmt.Errorf("no streams selected to write")
		}
		fmt.Fprintln(os.Stderr, "done (subtitles only)")
		return nil
	}

	tMux := time.Now()
	if err := mux.Mux(ffmpeg, muxInputs, outPath); err != nil {
		fmt.Fprintf(os.Stderr, "mux failed; raw stream files kept in %s\n", workDir)
		return err
	}
	logv("phase mux: %s (%d inputs)", time.Since(tMux).Round(time.Millisecond), len(muxInputs))
	if !o.keepTemp {
		for _, in := range muxInputs {
			os.Remove(in.Path)
		}
		for _, r := range results { // raw subtitle payloads too
			os.Remove(r.path)
		}
	}
	fmt.Fprintln(os.Stderr, "done: "+outPath)
	return nil
}

// interruptHandler makes the first Ctrl-C/SIGTERM stop the download cleanly — a
// live recording finishes up (stopLive closed), a VOD cancels so the next run
// resumes — and a second one abort outright.
func interruptHandler(live bool, stopLive chan struct{}, cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		if live {
			// live: stop discovering, drain pipeline, mux what we have
			fmt.Fprintln(os.Stderr, "\ninterrupt: finishing recording (press again to abort)")
			close(stopLive)
		} else {
			// VOD: cancel now; resume state lets the next run continue
			fmt.Fprintln(os.Stderr, "\ninterrupt: stopping (rerun the same command to resume)")
			cancel()
		}
		<-sigCh
		fmt.Fprintln(os.Stderr, "aborted")
		cancel()
		os.Exit(130)
	}()
}

// downloadFile writes a direct file straight to its output path: no stream
// selection, no temp file, no mux — the bytes are the deliverable. -o is used
// as given (no container extension is forced); otherwise the server's or the
// URL's filename, extension and all.
func downloadFile(ctx context.Context, cancel context.CancelFunc, o *options, client *httpx.Client, st *manifest.Stream, threads int, logv func(string, ...any)) error {
	outPath := o.output
	if outPath == "" {
		outPath = st.Name
	}
	size := "size unknown"
	if n := source.FileSize(st); n >= 0 {
		size = fmt.Sprintf("%d bytes", n)
	}
	if o.listOnly {
		fmt.Printf("file %s (%s)\n", st.Name, size)
		return nil
	}
	fmt.Fprintf(os.Stderr, "file: %s (%s)\n", outPath, size)
	interruptHandler(false, nil, cancel)
	prog := engine.NewProgress(false, o.progressInterval)
	progStop := make(chan struct{})
	go prog.Render(progStop)
	err := engine.DownloadFile(ctx, engine.Config{Client: client, Threads: threads, Progress: prog, Verbose: logv}, st, outPath)
	close(progStop)
	time.Sleep(50 * time.Millisecond) // let renderer print the final line
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "done: "+outPath)
	return nil
}

// sidecarSubPath builds "<output-basename>.<lang>.<fmt>" next to the output,
// disambiguating with the stream name/ID when a language repeats.
func sidecarSubPath(outPath string, st *manifest.Stream, format string, used map[string]bool) string {
	base := strings.TrimSuffix(outPath, filepath.Ext(outPath))
	tag := st.Language
	if tag == "" {
		tag = "sub"
	}
	cand := fmt.Sprintf("%s.%s.%s", base, tag, format)
	if used[cand] {
		extra := st.Name
		if extra == "" {
			extra = st.ID
		}
		cand = fmt.Sprintf("%s.%s.%s.%s", base, tag, source.SanitizeTag(extra), format)
	}
	return cand
}

func outputPath(out, inputURL string, live bool) string {
	if out != "" {
		if filepath.Ext(out) == "" {
			out += ".mp4"
		}
		return out
	}
	name := "m314dl-output"
	if u, err := url.Parse(inputURL); err == nil {
		base := filepath.Base(u.Path)
		base = strings.TrimSuffix(base, filepath.Ext(base))
		if base != "" && base != "/" && base != "." {
			name = base
		}
	}
	if len(name) > 120 {
		name = name[:120]
	}
	if live {
		name += "-" + time.Now().Format("20060102-150405")
	}
	return name + ".mp4"
}

// convertSubtitle normalizes a raw subtitle stream file to srt/vtt.
func convertSubtitle(rawPath, format, ffmpeg string) (string, error) {
	b, err := os.ReadFile(rawPath)
	if err != nil {
		return "", err
	}
	outPath := strings.TrimSuffix(rawPath, ".rawsub") + "." + format
	kind := subs.Sniff(b)
	if kind == subs.KindFMP4 {
		// stpp: TTML documents live in mdat — extract natively (ffmpeg has
		// no TTML decoder)
		if mdat := subs.ExtractMdat(b); subs.IsTTMLPayload(mdat) {
			b = mdat
			kind = subs.KindTTML
		}
	}
	if kind == subs.KindFMP4 {
		if ffmpeg == "" {
			return "", errors.New("fMP4 subtitles need ffmpeg")
		}
		// ffmpeg reads mp4-wrapped wvtt/stpp fine; rename so it sniffs mp4
		mp4Path := strings.TrimSuffix(rawPath, ".rawsub") + ".sub.mp4"
		if err := os.Rename(rawPath, mp4Path); err != nil {
			return "", err
		}
		defer os.Rename(mp4Path, rawPath)
		if err := mux.ExtractSubtitle(ffmpeg, mp4Path, outPath); err != nil {
			return "", err
		}
		return outPath, nil
	}
	cues, err := subs.Parse(b, kind)
	if err != nil {
		return "", err
	}
	if len(cues) == 0 {
		return "", errors.New("no cues found")
	}
	cues = subs.Rebase(cues)
	if format == "vtt" {
		return outPath, subs.WriteVTT(outPath, cues)
	}
	return outPath, subs.WriteSRT(outPath, cues)
}
