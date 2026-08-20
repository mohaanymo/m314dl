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
//	GET    /api/health          -> {"channels":3,"max":32}
//
// Media (unauthenticated, like any origin — access control is the controller's
// job): GET /{id}/... routes to that channel's HLS/TS/DASH handler.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/hls"
	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/pick"
)

type workerServer struct {
	secret      string
	maxChannels int
	ffmpegPath  string
	logv        func(string, ...any)

	mu       sync.RWMutex
	channels map[string]*channel
}

type channel struct {
	id        string
	url       string
	format    string
	mediaPath string // "/{id}/live.m3u8"
	handler   http.Handler
	pres      presentation
	cancel    context.CancelFunc
	tmpDir    string

	mu      sync.Mutex
	state   string // running | done | error
	errMsg  string
	live    bool
	started time.Time
}

var idOK = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

func newWorkerServer(secret string, maxChannels int, ffmpegPath string, logv func(string, ...any)) *workerServer {
	if logv == nil {
		logv = func(string, ...any) {}
	}
	return &workerServer{secret: secret, maxChannels: maxChannels, ffmpegPath: ffmpegPath, logv: logv, channels: map[string]*channel{}}
}

func serveWorker(addr, secret string, maxChannels int, ffmpegPath string, logv func(string, ...any)) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-worker %q: %w", addr, err)
	}
	if secret == "" && host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("-worker on non-loopback %q requires -worker-secret", addr)
		}
	}
	w := newWorkerServer(secret, maxChannels, ffmpegPath, logv)
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
	mux.HandleFunc("POST /api/channels", bearerAuth(w.secret, w.apiStart))
	mux.HandleFunc("GET /api/channels", bearerAuth(w.secret, w.apiList))
	mux.HandleFunc("GET /api/channels/{id}", bearerAuth(w.secret, w.apiStatus))
	mux.HandleFunc("DELETE /api/channels/{id}", bearerAuth(w.secret, w.apiStop))
	mux.HandleFunc("GET /api/health", bearerAuth(w.secret, w.apiHealth))
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
	writeJSON(rw, ch.view())
}

func (w *workerServer) apiList(rw http.ResponseWriter, r *http.Request) {
	w.mu.RLock()
	views := make([]channelView, 0, len(w.channels))
	for _, ch := range w.channels {
		views = append(views, ch.view())
	}
	w.mu.RUnlock()
	writeJSON(rw, views)
}

func (w *workerServer) apiStatus(rw http.ResponseWriter, r *http.Request) {
	ch := w.channel(r.PathValue("id"))
	if ch == nil {
		http.Error(rw, "no such channel", http.StatusNotFound)
		return
	}
	writeJSON(rw, ch.view())
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
	writeJSON(rw, map[string]string{"status": "stopped", "id": id})
}

func (w *workerServer) apiHealth(rw http.ResponseWriter, r *http.Request) {
	w.mu.RLock()
	n := len(w.channels)
	w.mu.RUnlock()
	writeJSON(rw, map[string]any{"status": "ok", "channels": n, "max": w.maxChannels})
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
	keys, err := parseKeys(req.Keys)
	if err != nil {
		return nil, err
	}
	ve, ae, se, err := parseSelection(req.SV, req.SA, req.SS)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	master, kind, err := loadManifest(ctx, client, req.URL, w.logv)
	if err != nil {
		cancel()
		return nil, err
	}
	selected, err := selectAndExpand(ctx, client, kind, master, ve, ae, se, keys)
	if err != nil {
		cancel()
		return nil, err
	}

	tmpDir, err := os.MkdirTemp("", "m314dl-ch-"+req.ID+"-*")
	if err != nil {
		cancel()
		return nil, err
	}
	o := options{serveFormat: req.Format, serveTranscode: req.Transcode, ffmpegPath: w.ffmpegPath}
	pres, handler, path, jobs, err := buildOutputs(o, selected, tmpDir, w.logv)
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
func (w *workerServer) runChannel(ctx context.Context, ch *channel, client *httpx.Client, kind string, keys map[[16]byte][]byte, jobs []job) {
	// End the presentation on cancel too, so an ffmpeg-remux writer blocked on a
	// stalled pipe unblocks with EPIPE and the errgroup can return.
	go func() {
		<-ctx.Done()
		ch.pres.End()
	}()

	g, gctx := errgroup.WithContext(ctx)
	for _, j := range jobs {
		j := j
		g.Go(func() error {
			cfg := engine.Config{Client: client, Keys: keys, Verbose: w.logv, Sink: j.sink}
			refresh := refreshFunc(client, kind, j.st, w.logv)
			if !j.st.Live {
				refresh = nil
			}
			if err := engine.DownloadStream(gctx, cfg, j.st, j.tmp, refresh); err != nil {
				return fmt.Errorf("track %s: %w", j.st.ID, err)
			}
			return nil
		})
	}
	err := g.Wait()
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
