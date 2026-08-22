// Worker mode (-worker): a multi-channel restream agent a controller (e.g. the
// m314 panel) drives over HTTP. One process holds many channels; each channel
// downloads and decrypts its source and republishes it live, served at
// /{id}/live.m3u8 (or /live.ts, /live.mpd). Channels run in-process — the media
// is already in RAM in the packager, so there is no subprocess to babysit and no
// reverse proxy to a child's port.
//
// Control API (Bearer -worker-secret):
//
//	POST   /api/channels        {"id":"news","url":"...","format":"hls","keys":["KID:KEY"],"headers":{...}}
//	GET    /api/channels        -> [{"id":...,"state":"running",...}, ...]
//	GET    /api/channels/{id}   -> one channel's status
//	DELETE /api/channels/{id}   -> stop and remove
//	GET    /api/health          -> {"status":"ok","channels":3,"max":32,"version":..,cpu/ram/disk/net}
//
// Media (unauthenticated, like any origin — access control is the controller's
// job): GET /{id}/... routes to that channel's HLS/TS/DASH handler.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mohamed/m314dl/internal/hls"
	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/pick"
	"github.com/mohamed/m314dl/internal/serve"
	"github.com/mohamed/m314dl/internal/source"
)

type workerServer struct {
	secret      string
	version     string
	maxChannels int
	ffmpegPath  string
	logv        func(string, ...any)

	// spool is where channels stage segments. Each worker instance owns one and
	// clears it on startup: a channel removes its own directory when it stops,
	// but a killed or restarted process never gets to, and the leftovers are
	// invisible and unbounded. One restart-heavy day left 1430 directories and
	// 43GB behind, on the same disk the live segments were being written to.
	spool string

	mu       sync.RWMutex
	channels map[string]*channel
}

type channel struct {
	id        string
	url       string
	format    string
	mediaPath string // "/{id}/live.m3u8"
	handler   http.Handler
	pres      serve.Presentation
	cancel    context.CancelFunc
	tmpDir    string

	mu      sync.Mutex
	state   string // running | done | error
	errMsg  string
	live    bool
	started time.Time
}

var idOK = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

func newWorkerServer(secret string, maxChannels int, ffmpegPath, version string, logv func(string, ...any)) *workerServer {
	if logv == nil {
		logv = func(string, ...any) {}
	}
	return &workerServer{secret: secret, version: version, maxChannels: maxChannels, ffmpegPath: ffmpegPath,
		logv: logv, channels: map[string]*channel{}}
}

// useSpool claims a staging directory for this worker and clears whatever a
// previous run left in it.
//
// The directory is named for the address the worker listens on, so two workers
// on one host never share or delete each other's staging area, and a restart of
// either reclaims exactly its own leftovers.
func (w *workerServer) useSpool(addr string) error {
	name := "m314dl-worker-" + strings.NewReplacer(":", "_", "/", "_", ".", "_").Replace(addr)
	dir := filepath.Join(os.TempDir(), name)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear spool %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create spool %s: %w", dir, err)
	}
	w.spool = dir
	return nil
}

// spoolDir is where a channel stages its segments.
func (w *workerServer) spoolDir() string {
	if w.spool != "" {
		return w.spool
	}
	return "" // os.MkdirTemp falls back to the system temp dir
}

func ServeWorker(addr, secret string, maxChannels int, ffmpegPath, version string, logv func(string, ...any)) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-worker %q: %w", addr, err)
	}
	if secret == "" && host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("-worker on non-loopback %q requires -worker-secret", addr)
		}
	}
	w := newWorkerServer(secret, maxChannels, ffmpegPath, version, logv)
	if err := w.useSpool(addr); err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: w.handler(), ReadHeaderTimeout: 10 * time.Second}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nworker: shutting down, stopping channels")
		w.stopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	cap := "unlimited"
	if maxChannels > 0 {
		cap = fmt.Sprint(maxChannels)
	}
	fmt.Fprintf(os.Stderr, "m314dl %s worker on %s (max channels: %s)\n", version, addr, cap)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (w *workerServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/channels", httpx.BearerAuth(w.secret, w.apiStart))
	mux.HandleFunc("GET /api/channels", httpx.BearerAuth(w.secret, w.apiList))
	mux.HandleFunc("GET /api/channels/{id}", httpx.BearerAuth(w.secret, w.apiStatus))
	mux.HandleFunc("DELETE /api/channels/{id}", httpx.BearerAuth(w.secret, w.apiStop))
	mux.HandleFunc("GET /api/health", httpx.BearerAuth(w.secret, w.apiHealth))
	mux.HandleFunc("/{id}/", w.media) // media: unauthenticated, routed to the channel
	return mux
}

// ─── control API ─────────────────────────────────────────────────────────────

type channelReq struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Format    string            `json:"format"` // hls | ts | dash (default hls)
	Keys      []string          `json:"keys"`   // CENC "KID:KEY" (hex)
	Headers   map[string]string `json:"headers"`
	Proxy     string            `json:"proxy"`
	Insecure  bool              `json:"insecure"`
	SV        string            `json:"sv"` // video selection (default best)
	SA        string            `json:"sa"`
	SS        string            `json:"ss"`
	Transcode string            `json:"transcode"` // ts remux: ffmpeg output args
}

func (w *workerServer) apiStart(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, 64<<10)
	var req channelReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" || req.ID == "" {
		http.Error(rw, `bad request: want {"id":"...","url":"...","format":"hls|ts|dash"}`, http.StatusBadRequest)
		return
	}
	if !idOK.MatchString(req.ID) || req.ID == "api" {
		http.Error(rw, "bad channel id (want [a-zA-Z0-9._-], not 'api')", http.StatusBadRequest)
		return
	}

	w.mu.RLock()
	_, exists := w.channels[req.ID]
	atCap := w.maxChannels > 0 && len(w.channels) >= w.maxChannels
	w.mu.RUnlock()
	if exists {
		http.Error(rw, "channel already exists: "+req.ID, http.StatusConflict)
		return
	}
	if atCap {
		http.Error(rw, fmt.Sprintf("at capacity (%d channels)", w.maxChannels), http.StatusServiceUnavailable)
		return
	}

	ch, err := w.startChannel(req)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	httpx.WriteJSON(rw, ch.view())
}

func (w *workerServer) apiList(rw http.ResponseWriter, r *http.Request) {
	w.mu.RLock()
	views := make([]channelView, 0, len(w.channels))
	for _, ch := range w.channels {
		views = append(views, ch.view())
	}
	w.mu.RUnlock()
	httpx.WriteJSON(rw, views)
}

func (w *workerServer) apiStatus(rw http.ResponseWriter, r *http.Request) {
	ch := w.channel(r.PathValue("id"))
	if ch == nil {
		http.Error(rw, "no such channel", http.StatusNotFound)
		return
	}
	httpx.WriteJSON(rw, ch.view())
}

func (w *workerServer) apiStop(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.mu.Lock()
	ch := w.channels[id]
	if ch != nil {
		delete(w.channels, id)
	}
	w.mu.Unlock()
	if ch == nil {
		http.Error(rw, "no such channel", http.StatusNotFound)
		return
	}
	ch.cancel() // downloads stop; the run goroutine ends the presentation and cleans up
	httpx.WriteJSON(rw, map[string]string{"status": "stopped", "id": id})
}

func (w *workerServer) apiHealth(rw http.ResponseWriter, r *http.Request) {
	w.mu.RLock()
	n := len(w.channels)
	w.mu.RUnlock()
	httpx.WriteJSON(rw, healthOut{
		Status: "ok", Channels: n, Max: w.maxChannels,
		// The version is what tells a panel the worker is deployed at all: with
		// none reported, its server list showed every node as "not deployed"
		// however healthy the node actually was.
		Version: w.version,
		sysStat: readSysStat(),
	})
}

// healthOut is what GET /api/health answers. sysStat is embedded, so its
// fields sit alongside these at the top level of the object.
type healthOut struct {
	Status   string `json:"status"`
	Channels int    `json:"channels"`
	Max      int    `json:"max"`
	Version  string `json:"version"`
	sysStat
}

// ─── media routing ───────────────────────────────────────────────────────────

func (w *workerServer) media(rw http.ResponseWriter, r *http.Request) {
	ch := w.channel(r.PathValue("id"))
	if ch == nil {
		http.NotFound(rw, r)
		return
	}
	// Strip the /{id} prefix so the channel's own handler sees /live.m3u8 etc.
	http.StripPrefix("/"+ch.id, ch.handler).ServeHTTP(rw, r)
}

func (w *workerServer) channel(id string) *channel {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.channels[id]
}

// ─── channel lifecycle ───────────────────────────────────────────────────────

// startChannel resolves the source, selects streams, builds the output, and
// launches the downloads in the background. It blocks only for the manifest
// fetch + selection, so start errors (bad URL, DRM without a key) surface in the
// response; a running channel is then served immediately.
func (w *workerServer) startChannel(req channelReq) (*channel, error) {
	headers := req.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	client, err := httpx.New(httpx.Options{Headers: headers, Proxy: req.Proxy, Insecure: req.Insecure, Retries: 5})
	if err != nil {
		return nil, err
	}
	keys, err := source.ParseKeys(req.Keys)
	if err != nil {
		return nil, err
	}
	ve, ae, se, err := parseSelection(req.SV, req.SA, req.SS)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	master, kind, err := source.LoadManifest(ctx, client, req.URL, w.logv)
	if err != nil {
		cancel()
		return nil, err
	}
	selected, err := selectAndExpand(ctx, client, kind, master, ve, ae, se, keys)
	if err != nil {
		cancel()
		return nil, err
	}

	tmpDir, err := os.MkdirTemp(w.spoolDir(), "m314dl-ch-"+req.ID+"-*")
	if err != nil {
		cancel()
		return nil, err
	}
	o := serve.Options{Format: req.Format, Transcode: req.Transcode, FFmpegPath: w.ffmpegPath}
	pres, handler, path, jobs, err := serve.BuildOutputs(o, selected, tmpDir, w.logv)
	if err != nil {
		cancel()
		os.RemoveAll(tmpDir)
		return nil, err
	}

	live := false
	for _, st := range selected {
		if st.Live {
			live = true
		}
	}
	ch := &channel{
		id: req.ID, url: req.URL, format: normFormat(req.Format),
		mediaPath: "/" + req.ID + path, handler: handler, pres: pres,
		cancel: cancel, tmpDir: tmpDir, state: "running", live: live, started: time.Now(),
	}

	// Register, guarding against a racing start of the same id / a full table.
	w.mu.Lock()
	if _, exists := w.channels[req.ID]; exists {
		w.mu.Unlock()
		cancel()
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("channel already exists: %s", req.ID)
	}
	if w.maxChannels > 0 && len(w.channels) >= w.maxChannels {
		w.mu.Unlock()
		cancel()
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("at capacity (%d channels)", w.maxChannels)
	}
	w.channels[req.ID] = ch
	w.mu.Unlock()

	go w.runChannel(ctx, ch, client, kind, keys, jobs)
	return ch, nil
}

// runChannel downloads every track into its sink until the source ends or the
// channel is stopped, then finalizes state and cleans up.
func (w *workerServer) runChannel(ctx context.Context, ch *channel, client *httpx.Client, kind string, keys map[[16]byte][]byte, jobs []serve.Job) {
	// End the presentation on cancel too, so an ffmpeg-remux writer blocked on a
	// stalled pipe unblocks with EPIPE and the errgroup can return.
	go func() {
		<-ctx.Done()
		ch.pres.End()
	}()

	err := serve.RunJobs(ctx, client, kind, jobs, keys, nil, 0, w.logv)
	ch.pres.End()
	os.RemoveAll(ch.tmpDir)

	ch.mu.Lock()
	switch {
	case ctx.Err() != nil:
		ch.state = "stopped"
	case err != nil:
		ch.state, ch.errMsg = "error", err.Error()
	default:
		ch.state = "done" // finite source finished; still served (ENDLIST)
	}
	ch.mu.Unlock()
}

func (w *workerServer) stopAll() {
	w.mu.Lock()
	chans := make([]*channel, 0, len(w.channels))
	for _, ch := range w.channels {
		chans = append(chans, ch)
	}
	w.channels = map[string]*channel{}
	w.mu.Unlock()
	for _, ch := range chans {
		ch.cancel()
	}
}

// ─── views ───────────────────────────────────────────────────────────────────

type channelView struct {
	ID      string    `json:"id"`
	URL     string    `json:"url"`
	Format  string    `json:"format"`
	State   string    `json:"state"`
	Error   string    `json:"error,omitempty"`
	Live    bool      `json:"live"`
	Media   string    `json:"media"`
	Status  string    `json:"status,omitempty"`
	Started time.Time `json:"started"`
}

func (ch *channel) view() channelView {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return channelView{
		ID: ch.id, URL: ch.url, Format: ch.format, State: ch.state, Error: ch.errMsg,
		Live: ch.live, Media: ch.mediaPath, Status: ch.pres.StatusLine(), Started: ch.started,
	}
}

func normFormat(f string) string {
	switch f {
	case "ts", "mpegts":
		return "ts"
	case "dash", "mpd":
		return "dash"
	default:
		return "hls"
	}
}

// parseSelection parses the three selector expressions (empty sv defaults to best).
func parseSelection(sv, sa, ss string) (ve, ae, se *pick.Expr, err error) {
	if sv == "" {
		sv = "best"
	}
	if ve, err = pick.ParseExpr(sv); err != nil {
		return nil, nil, nil, fmt.Errorf("sv: %w", err)
	}
	if sa != "" {
		if ae, err = pick.ParseExpr(sa); err != nil {
			return nil, nil, nil, fmt.Errorf("sa: %w", err)
		}
	}
	if ss != "" {
		if se, err = pick.ParseExpr(ss); err != nil {
			return nil, nil, nil, fmt.Errorf("ss: %w", err)
		}
	}
	return ve, ae, se, nil
}

// selectAndExpand selects the requested streams and, for HLS, fetches each
// media playlist (mirrors run()'s expansion). It fails fast on a CENC track
// with no key so a misconfigured channel reports the reason at start.
func selectAndExpand(ctx context.Context, client *httpx.Client, kind string, master *manifest.Master, ve, ae, se *pick.Expr, keys map[[16]byte][]byte) ([]*manifest.Stream, error) {
	selected := pick.Select(master.Streams, ve, ae, se)
	if len(selected) == 0 {
		return nil, fmt.Errorf("no streams matched the selection")
	}
	// A video selection that matches nothing must not quietly leave an
	// audio-only channel. It runs, every metric reads healthy, and the first
	// person to notice is a viewer looking at a black screen — which is exactly
	// how a quality preference of "720" against a source publishing 768x432
	// took the picture off a live channel.
	if wantsVideo(ve) && !hasKind(selected, manifest.Video) && hasKind(master.Streams, manifest.Video) {
		return nil, fmt.Errorf("no video stream matched the selection; the source publishes %s "+
			"(use -sv none for an audio-only channel)", videoSummary(master.Streams))
	}
	for _, st := range selected {
		if kind == "hls" && !st.SegmentsFull {
			body, finalURL, err := client.FetchBytes(ctx, st.PlaylistURL, "")
			if err != nil {
				return nil, fmt.Errorf("fetch media playlist for %s: %w", st.ID, err)
			}
			st.PlaylistURL = finalURL
			if err := hls.ParseMedia(body, finalURL, st); err != nil {
				return nil, err
			}
		}
		if len(st.Segments) == 0 {
			return nil, fmt.Errorf("stream %s has no segments", st.ID)
		}
		if k := st.Segments[0].Key; k != nil && k.Method == manifest.EncCENC && len(keys) == 0 {
			return nil, fmt.Errorf("stream %s is CENC/DRM-protected; supply the content key", st.ID)
		}
	}
	return selected, nil
}

// wantsVideo reports whether a video selection asks for a picture at all.
// A nil expression means "best", which does.
func wantsVideo(ve *pick.Expr) bool { return ve == nil || !ve.IsNone() }

func hasKind(streams []*manifest.Stream, k manifest.MediaType) bool {
	for _, st := range streams {
		if st.Type == k {
			return true
		}
	}
	return false
}

// videoSummary lists the resolutions a source does publish, so the error names
// the alternatives instead of only rejecting what was asked for.
func videoSummary(streams []*manifest.Stream) string {
	var out []string
	seen := map[string]bool{}
	for _, st := range streams {
		if st.Type != manifest.Video {
			continue
		}
		r := st.Resolution()
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return "no video at all"
	}
	return strings.Join(out, ", ")
}
