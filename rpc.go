// RPC server mode (-rpc): a small HTTP/JSON API for submitting and watching
// download jobs remotely. Each job runs as a child m314dl process with the
// client-supplied CLI args, so every feature (selection, keys, resume,
// graceful-stop signal semantics) works identically to local runs.
//
//	POST /add            {"url":"...","args":["-o","x.mp4"]} -> {"id":1}
//	GET  /jobs           -> [{"id":1,"state":"running","progress":"42% | ..."},...]
//	GET  /jobs/{id}      -> job detail incl. captured log
//	POST /jobs/{id}/stop -> SIGINT (graceful; repeat to abort, like ^C ^C)
//	GET  /health         -> {"status":"ok","running":3,"total":57,"max":64}
//
// Built to run unattended in production: bounded memory (finished jobs are
// reaped, per-job logs are capped), no lock contention (each job's output is
// buffered under its own lock, never the server's), an admission cap so a flood
// of /add can't fork-bomb the host, graceful shutdown that stops children, and
// crash-safety so children die with the parent (Linux).
//
// Auth: "Authorization: Bearer <secret>" when -rpc-secret is set; a secret is
// mandatory on non-loopback binds.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	rpcLogCap     = 256 << 10 // keep the last 256KB of each job's output
	rpcMaxBody    = 64 << 10  // cap the /add request body
	rpcMaxRetain  = 5000      // hard ceiling on retained (incl. finished) jobs
	rpcReapPeriod = 30 * time.Second
)

// rpcJob is one download job. Server-owned identity/timing fields are set once
// and then read-only; the live fields (state, progress, log) are guarded by the
// job's own mutex, so a chatty child never contends on the server lock.
type rpcJob struct {
	id      int64
	url     string
	args    []string
	started time.Time
	cmd     *exec.Cmd

	endedNano atomic.Int64 // 0 while running; set once on exit (lock-free reaping)

	mu       sync.Mutex
	state    string // running | done | error
	progress string
	errMsg   string
	log      []byte
}

type rpcServer struct {
	exe     string // binary to spawn for jobs (self)
	secret  string
	maxJobs int           // 0 = unlimited
	retain  time.Duration // keep finished jobs at least this long

	nextID atomic.Int64

	mu      sync.RWMutex // guards the jobs map and running count
	jobs    map[int64]*rpcJob
	running int
}

func newRPCServer(exe, secret string, maxJobs int, retain time.Duration) *rpcServer {
	if retain <= 0 {
		retain = time.Hour
	}
	return &rpcServer{exe: exe, secret: secret, maxJobs: maxJobs, retain: retain, jobs: map[int64]*rpcJob{}}
}

func serveRPC(addr, secret string, maxJobs int, retain time.Duration) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-rpc %q: %w", addr, err)
	}
	if secret == "" && host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("-rpc on non-loopback %q requires -rpc-secret", addr)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	s := newRPCServer(exe, secret, maxJobs, retain)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second, // slowloris guard
	}

	stopReaper := make(chan struct{})
	go s.reapLoop(stopReaper)

	// Graceful shutdown: stop accepting, stop children, then exit.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nrpc: shutting down, stopping jobs")
		close(stopReaper)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		s.stopAll()
	}()

	cap := "unlimited"
	if maxJobs > 0 {
		cap = strconv.Itoa(maxJobs)
	}
	fmt.Fprintf(os.Stderr, "m314dl %s RPC server on %s (max jobs: %s)\n", version, addr, cap)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *rpcServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /add", s.auth(s.add))
	mux.HandleFunc("GET /jobs", s.auth(s.list))
	mux.HandleFunc("GET /jobs/{id}", s.auth(s.detail))
	mux.HandleFunc("POST /jobs/{id}/stop", s.auth(s.stop))
	mux.HandleFunc("GET /health", s.auth(s.health))
	return mux
}

func (s *rpcServer) auth(h http.HandlerFunc) http.HandlerFunc { return bearerAuth(s.secret, h) }

// bearerAuth wraps a handler with constant-time "Authorization: Bearer <secret>"
// checking. An empty secret disables auth (loopback-only convenience).
func bearerAuth(secret string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if secret != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	}
}

func (s *rpcServer) add(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, rpcMaxBody)
	var req struct {
		URL  string   `json:"url"`
		Args []string `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(w, `bad request: want {"url":"...","args":["-o","x.mp4"]}`, http.StatusBadRequest)
		return
	}
	for _, a := range req.Args {
		if a == "-rpc" || a == "--rpc" {
			http.Error(w, "-rpc not allowed in job args", http.StatusBadRequest)
			return
		}
	}

	// Admission: reserve a running slot before spawning so a flood of /add can't
	// fork-bomb the host. Reserve and register under one lock so the cap is exact.
	cmd := exec.Command(s.exe, append(append([]string{}, req.Args...), req.URL)...)
	hardenCmd(cmd) // die with the parent (Linux); own process group
	j := &rpcJob{url: req.URL, args: req.Args, started: time.Now(), cmd: cmd, state: "running"}
	out := &jobWriter{j: j}
	cmd.Stdout = out
	cmd.Stderr = out

	s.mu.Lock()
	if s.maxJobs > 0 && s.running >= s.maxJobs {
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("at capacity (%d jobs running); retry later", s.maxJobs), http.StatusServiceUnavailable)
		return
	}
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		http.Error(w, "spawn: "+err.Error(), http.StatusInternalServerError)
		return
	}
	j.id = s.nextID.Add(1)
	s.jobs[j.id] = j
	s.running++
	s.mu.Unlock()

	go func() {
		err := cmd.Wait()
		j.finish(err)
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}()
	writeJSON(w, map[string]int64{"id": j.id})
}

func (s *rpcServer) list(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	snapshot := make([]*rpcJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		snapshot = append(snapshot, j)
	}
	s.mu.RUnlock()
	// Copy each job's live fields under its own lock — never the server lock.
	out := make([]jobView, len(snapshot))
	for i, j := range snapshot {
		out[i] = j.view(false)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	writeJSON(w, out)
}

func (s *rpcServer) detail(w http.ResponseWriter, r *http.Request) {
	j := s.byID(r)
	if j == nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	writeJSON(w, j.view(true))
}

func (s *rpcServer) stop(w http.ResponseWriter, r *http.Request) {
	j := s.byID(r)
	if j == nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	j.mu.Lock()
	running := j.state == "running"
	j.mu.Unlock()
	if !running {
		http.Error(w, "job not running", http.StatusConflict)
		return
	}
	if err := j.cmd.Process.Signal(os.Interrupt); err != nil {
		j.cmd.Process.Kill() // Windows can't deliver SIGINT; hard-stop instead
	}
	writeJSON(w, map[string]string{"status": "stopping"})
}

func (s *rpcServer) health(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	running, total := s.running, len(s.jobs)
	s.mu.RUnlock()
	writeJSON(w, map[string]any{"status": "ok", "running": running, "total": total, "max": s.maxJobs})
}

func (s *rpcServer) byID(r *http.Request) *rpcJob {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobs[id]
}

// reapLoop bounds memory: it drops jobs that finished longer than retain ago,
// and if the map still exceeds the hard ceiling, drops the oldest finished ones.
func (s *rpcServer) reapLoop(stop <-chan struct{}) {
	t := time.NewTicker(rpcReapPeriod)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.reap(time.Now())
		}
	}
}

func (s *rpcServer) reap(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-s.retain).UnixNano()
	for id, j := range s.jobs {
		if e := j.endedNano.Load(); e > 0 && e < cutoff {
			delete(s.jobs, id)
		}
	}
	if len(s.jobs) <= rpcMaxRetain {
		return
	}
	// Over the hard ceiling: drop the oldest finished jobs first.
	finished := make([]*rpcJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.endedNano.Load() > 0 {
			finished = append(finished, j)
		}
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].endedNano.Load() < finished[b].endedNano.Load() })
	for _, j := range finished {
		if len(s.jobs) <= rpcMaxRetain {
			break
		}
		delete(s.jobs, j.id)
	}
}

// stopAll signals every running child to stop, for graceful shutdown.
func (s *rpcServer) stopAll() {
	s.mu.RLock()
	jobs := make([]*rpcJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.RUnlock()
	for _, j := range jobs {
		if j.endedNano.Load() == 0 && j.cmd.Process != nil {
			j.cmd.Process.Signal(os.Interrupt)
		}
	}
}

// ---- job helpers (all under the job's own lock) ----

func (j *rpcJob) finish(err error) {
	j.mu.Lock()
	if err != nil {
		j.state, j.errMsg = "error", err.Error()
	} else {
		j.state = "done"
	}
	j.mu.Unlock()
	j.endedNano.Store(time.Now().UnixNano())
}

type jobView struct {
	ID       int64      `json:"id"`
	URL      string     `json:"url"`
	Args     []string   `json:"args,omitempty"`
	State    string     `json:"state"`
	Progress string     `json:"progress,omitempty"`
	Error    string     `json:"error,omitempty"`
	Started  time.Time  `json:"started"`
	Ended    *time.Time `json:"ended,omitempty"`
	Log      string     `json:"log,omitempty"`
}

func (j *rpcJob) view(includeLog bool) jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := jobView{
		ID: j.id, URL: j.url, Args: j.args, State: j.state,
		Progress: j.progress, Error: j.errMsg, Started: j.started,
	}
	if e := j.endedNano.Load(); e > 0 {
		t := time.Unix(0, e)
		v.Ended = &t
	}
	if includeLog {
		v.Log = string(j.log)
	}
	return v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// jobWriter captures a child's combined output into its job, under the job's own
// lock (never the server lock), keeping a bounded log and the latest progress
// line (progress lines contain " segs | " — a stable contract, see
// engine.Progress.Line).
type jobWriter struct{ j *rpcJob }

func (jw *jobWriter) Write(p []byte) (int, error) {
	j := jw.j
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, p...)
	if len(j.log) > rpcLogCap {
		j.log = append([]byte(nil), j.log[len(j.log)-rpcLogCap/2:]...)
	}
	for _, line := range strings.Split(string(p), "\n") {
		if strings.Contains(line, " segs | ") {
			j.progress = strings.TrimSpace(line)
		}
	}
	return len(p), nil
}
