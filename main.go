// m314dl — HLS/DASH media downloader.
//
// Usage: m314dl [flags] <URL>
// URL may be a master/media playlist (.m3u8), a DASH manifest (.mpd), or a
// web page (m314dl scrapes it for stream URLs).
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

	"github.com/mohamed/m314dl/internal/dash"
	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/hls"
	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/mux"
	"github.com/mohamed/m314dl/internal/pick"
	"github.com/mohamed/m314dl/internal/scrape"
	"github.com/mohamed/m314dl/internal/subs"
)

const version = "0.2.0"

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
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "m314dl: error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.StringVar(&o.output, "o", "", "output file (extension selects container; default from URL, .mp4)")
	flag.IntVar(&o.threads, "t", 0, "concurrent segment downloads per stream — a fixed count, held (backs off only on rate limits, then climbs back). Omit to auto-tune (up to 64)")
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
	flag.StringVar(&o.rpc, "rpc", "", "run as an RPC server on this address (e.g. 127.0.0.1:8314) instead of downloading; see rpc.go for the HTTP/JSON API")
	flag.StringVar(&o.rpcSecret, "rpc-secret", "", "bearer token for -rpc clients (required when binding a non-loopback address)")
	flag.IntVar(&o.rpcMaxJobs, "rpc-max-jobs", 64, "-rpc: max concurrent jobs; further /add requests get 503 until a slot frees (0 = unlimited)")
	flag.DurationVar(&o.rpcRetain, "rpc-retain", time.Hour, "-rpc: keep finished jobs queryable at least this long before reaping (bounds memory)")
	flag.StringVar(&o.serve, "serve", "", "restream: republish the selected streams live on this address (e.g. :8314) instead of downloading to a file")
	flag.StringVar(&o.serveFormat, "serve-format", "hls", "restream output: hls (/live.m3u8), ts (continuous MPEG-TS at /live.ts), or dash (/live.mpd; needs an fMP4 source)")
	flag.StringVar(&o.serveTranscode, "serve-transcode", "", "restream ts remux: ffmpeg output codec args to transcode instead of copy, e.g. '-c:v libx264 -preset veryfast -c:a aac' (implies the ffmpeg remux path)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "m314dl %s — HLS/DASH media downloader\n\nusage: m314dl [flags] <URL>\n\n", version)
		flag.PrintDefaults()
	}
	// Accept the input as the first arg (N_m3u8DL-RE style) as well as last:
	// Go's flag package stops at the first non-flag, so rotate a leading
	// non-flag token to the end before parsing.
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string{}, args[1:]...), args[0])
	}
	flag.CommandLine.Parse(args)
	if *showVersion {
		fmt.Println("m314dl", version)
		return nil
	}
	if o.rpc != "" {
		if flag.NArg() != 0 {
			return fmt.Errorf("-rpc takes no URL; submit jobs via POST /add")
		}
		return serveRPC(o.rpc, o.rpcSecret, o.rpcMaxJobs, o.rpcRetain)
	}
	// Adaptive concurrency unless the user pinned -t.
	tPinned := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "t" {
			tPinned = true
		}
	})
	if flag.NArg() != 1 {
		flag.Usage()
		return fmt.Errorf("exactly one URL required")
	}
	inputURL := flag.Arg(0)

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

	keys, err := parseKeys(o.keys)
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
	master, kind, err := loadManifest(ctx, client, inputURL, logv)
	if err != nil {
		return err
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
		threadCeiling := 0
		if tPinned {
			threadCeiling = o.threads
		}
		return runRestream(ctx, o, client, kind, selected, keys, bbtsKey, threadCeiling, logv)
	}

	outPath := outputPath(o.output, inputURL, live)
	workDir := filepath.Dir(outPath)
	ffmpeg, ffErr := mux.FindFFmpeg(o.ffmpegPath)
	if !o.noMux && ffErr != nil {
		return fmt.Errorf("ffmpeg not found (needed for muxing; pass -ffmpeg <path>, or -no-mux to skip): %w", ffErr)
	}

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

	prog := engine.NewProgress(live, o.progressInterval)
	progStop := make(chan struct{})
	go prog.Render(progStop)

	threadCeiling := 0 // 0 = auto-tune (no -t given)
	if tPinned {
		threadCeiling = o.threads
	}
	cfg := engine.Config{
		Client: client, Threads: threadCeiling, Keys: keys, BBTSKey: bbtsKey, AdFilters: adFilters,
		LiveLimit: o.liveLimit, Progress: prog, Verbose: logv, Stop: stopLive,
		FromStart: o.liveFromStart,
	}

	type done struct {
		st   *manifest.Stream
		path string
	}
	var results []done
	for _, st := range selected {
		results = append(results, done{st: st, path: engine.TempStreamPath(outPath, st, rawExt(st))})
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
			refresh := refreshFunc(client, kind, r.st, logv)
			if !r.st.Live {
				refresh = nil
			}
			if err := engine.DownloadStream(gctx, stCfg, r.st, r.path, refresh); err != nil {
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
		cand = fmt.Sprintf("%s.%s.%s.%s", base, tag, sanitizeTag(extra), format)
	}
	return cand
}

var tagBad = regexp.MustCompile(`[^\w.\-]+`)

func sanitizeTag(s string) string { return tagBad.ReplaceAllString(s, "_") }

// localManifestPath returns the filesystem path when input is a local manifest
// (a file:// URL, or a schemeless string that names an existing file) and ok.
func localManifestPath(input string) (string, bool) {
	if strings.HasPrefix(input, "file://") {
		return strings.TrimPrefix(input, "file://"), true
	}
	if u, err := url.Parse(input); err == nil && u.Scheme != "" {
		return "", false // has a real URL scheme (http/https/…)
	}
	if _, err := os.Stat(input); err == nil {
		return input, true
	}
	return "", false
}

// localBaseURL is the base against which relative segment URLs in a local
// manifest resolve. Absolute segment URLs (the common case for signed
// manifests) ignore it.
func localBaseURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file://" + filepath.Dir(abs) + "/"
}

// parseKeys parses -key values into a KID→key map. Each value is "KID:KEY"
// (hex, KID dashes optional) or a bare "KEY" (stored under the zero KID, used
// when exactly one key is given and the KID does not match).
func parseKeys(vals []string) (map[[16]byte][]byte, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := map[[16]byte][]byte{}
	for _, v := range vals {
		kidStr, keyStr, hasKID := strings.Cut(v, ":")
		if !hasKID {
			kidStr, keyStr = "", v
		}
		key, err := hex.DecodeString(strings.TrimSpace(keyStr))
		if err != nil || len(key) != 16 {
			return nil, fmt.Errorf("bad -key %q: key must be 32 hex chars (16 bytes)", v)
		}
		var kid [16]byte
		if hasKID {
			k, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(kidStr), "-", ""))
			if err != nil || len(k) != 16 {
				return nil, fmt.Errorf("bad -key %q: KID must be 32 hex chars (16 bytes)", v)
			}
			copy(kid[:], k)
		}
		out[kid] = key
	}
	return out, nil
}

// loadManifest fetches the input, sniffs HLS/DASH, and falls back to
// scraping when the input is a web page.
func loadManifest(ctx context.Context, client *httpx.Client, inputURL string, logv func(string, ...any)) (*manifest.Master, string, error) {
	// Local manifest file (path or file://): some providers sign the playlist
	// per request and hand it over as text rather than a URL.
	if path, ok := localManifestPath(inputURL); ok {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("read manifest %s: %w", path, err)
		}
		base := localBaseURL(path)
		logv("local manifest %s (base %s)", path, base)
		switch {
		case hls.IsHLS(body):
			m, err := hls.ParseMaster(body, base)
			return m, "hls", err
		case dash.IsDASH(body):
			m, err := dash.Parse(body, base)
			return m, "dash", err
		}
		return nil, "", fmt.Errorf("local file %s is not an HLS/DASH manifest", path)
	}
	body, finalURL, err := client.FetchBytes(ctx, inputURL, "")
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", inputURL, err)
	}
	if hls.IsHLS(body) {
		m, err := hls.ParseMaster(body, finalURL)
		return m, "hls", err
	}
	if dash.IsDASH(body) {
		m, err := dash.Parse(body, finalURL)
		return m, "dash", err
	}
	// probably a web page: scrape it
	logv("input is not a manifest; scraping page for stream URLs")
	candidates, err := scrape.Find(ctx, client, inputURL)
	if err != nil {
		return nil, "", err
	}
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("no HLS/DASH manifest found at %s (not a playlist, and page scan found no stream URLs)", inputURL)
	}
	for _, c := range candidates {
		fmt.Fprintln(os.Stderr, "found stream: "+c)
	}
	first := candidates[0]
	logv("using first candidate: %s", first)
	body, finalURL, err = client.FetchBytes(ctx, first, "")
	if err != nil {
		return nil, "", fmt.Errorf("fetch scraped %s: %w", first, err)
	}
	switch {
	case hls.IsHLS(body):
		m, err := hls.ParseMaster(body, finalURL)
		return m, "hls", err
	case dash.IsDASH(body):
		m, err := dash.Parse(body, finalURL)
		return m, "dash", err
	}
	return nil, "", fmt.Errorf("scraped URL %s is not a recognizable manifest", first)
}

func refreshFunc(client *httpx.Client, kind string, st *manifest.Stream, logv func(string, ...any)) engine.RefreshFunc {
	return func(ctx context.Context) (*manifest.Stream, error) {
		body, finalURL, err := client.FetchBytes(ctx, st.PlaylistURL, "")
		if err != nil {
			return nil, err
		}
		if kind == "hls" {
			fresh := *st
			fresh.Segments = nil
			if err := hls.ParseMedia(body, finalURL, &fresh); err != nil {
				return nil, err
			}
			return &fresh, nil
		}
		m, err := dash.Parse(body, finalURL)
		if err != nil {
			return nil, err
		}
		for _, cand := range m.Streams {
			if cand.ID == st.ID {
				return cand, nil
			}
		}
		logv("live: stream %s missing from refreshed MPD", st.ID)
		return nil, fmt.Errorf("stream %s no longer in MPD", st.ID)
	}
}

func rawExt(st *manifest.Stream) string {
	if st.Type == manifest.Subtitles {
		return ".rawsub"
	}
	u := ""
	if len(st.Segments) > 0 {
		u = st.Segments[0].URL
	}
	if st.Init != nil || strings.Contains(u, ".m4s") || strings.Contains(u, ".mp4") {
		return ".mp4"
	}
	return ".ts"
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
