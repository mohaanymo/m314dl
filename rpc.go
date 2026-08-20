// RPC server mode (-rpc): a small HTTP/JSON API for submitting and watching
// download jobs remotely. Each job runs as a child m314dl process with the
// client-supplied CLI args, so every feature (selection, keys, resume,
// graceful-stop signal semantics) works identically to local runs.
//
//	POST /add            {"url":"...","args":["-o","x.mp4"]} -> {"id":1}
//	GET  /jobs           -> [{"id":1,"state":"running","progress":"42% | ..."},...]
//	GET  /jobs/{id}      -> job detail incl. captured log
//	POST /jobs/{id}/stop -> SIGINT (graceful; repeat to abort, like ^C ^C)
//
// Auth: "Authorization: Bearer <secret>" when -rpc-secret is set; a secret is
// mandatory on non-loopback binds.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const rpcLogCap = 256 << 10 // keep the last 256KB of each job's output

type rpcJob struct {
	ID       int        `json:"id"`
	URL      string     `json:"url"`
	Args     []string   `json:"args,omitempty"`
	State    string     `json:"state"` // running | done | error
	Progress string     `json:"progress,omitempty"`
	Error    string     `json:"error,omitempty"`
	Started  time.Time  `json:"started"`
	Ended    *time.Time `json:"ended,omitempty"`
	Log      string     `json:"log,omitempty"` // detail view only

	log []byte
	cmd *exec.Cmd
}

type rpcServer struct {
	exe    string // binary to spawn for jobs (self)
	secret string

	mu   sync.Mutex // ponytail: one global lock; fine for a handful of jobs
	jobs []*rpcJob
}

func serveRPC(addr, secret string) error {
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
	s := &rpcServer{exe: exe, secret: secret}
	fmt.Fprintf(os.Stderr, "m314dl %s RPC server on %s\n", version, addr)
	return http.ListenAndServe(addr, s.handler())
}

func (s *rpcServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /add", s.auth(s.add))
	mux.HandleFunc("GET /jobs", s.auth(s.list))
	mux.HandleFunc("GET /jobs/{id}", s.auth(s.detail))
	mux.HandleFunc("POST /jobs/{id}/stop", s.auth(s.stop))
	return mux
}

func (s *rpcServer) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.secret != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.secret)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	}
}

func (s *rpcServer) add(w http.ResponseWriter, r *http.Request) {
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
	cmd := exec.Command(s.exe, append(append([]string{}, req.Args...), req.URL)...)
	j := &rpcJob{URL: req.URL, Args: req.Args, State: "running", Started: time.Now(), cmd: cmd}
	out := &jobWriter{s: s, j: j}
	cmd.Stdout = out
	cmd.Stderr = out

	s.mu.Lock()
	j.ID = len(s.jobs) + 1
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		http.Error(w, "spawn: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.jobs = append(s.jobs, j)
	s.mu.Unlock()

	go func() {
		err := cmd.Wait()
		now := time.Now()
		s.mu.Lock()
		j.Ended = &now
		if err != nil {
			j.State, j.Error = "error", err.Error()
		} else {
			j.State = "done"
		}
		s.mu.Unlock()
	}()
	writeJSON(w, map[string]int{"id": j.ID})
}

func (s *rpcServer) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]rpcJob, len(s.jobs))
	for i, j := range s.jobs {
		out[i] = *j // shallow copy; Log deliberately empty in list view
	}
	s.mu.Unlock()
	writeJSON(w, out)
}

func (s *rpcServer) detail(w http.ResponseWriter, r *http.Request) {
	j := s.byID(r)
	if j == nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	v := *j
	v.Log = string(j.log)
	s.mu.Unlock()
	writeJSON(w, v)
}

func (s *rpcServer) stop(w http.ResponseWriter, r *http.Request) {
	j := s.byID(r)
	if j == nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	running := j.State == "running"
	cmd := j.cmd
	s.mu.Unlock()
	if !running {
		http.Error(w, "job not running", http.StatusConflict)
		return
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		cmd.Process.Kill() // Windows can't deliver SIGINT; hard-stop instead
	}
	writeJSON(w, map[string]string{"status": "stopping"})
}

func (s *rpcServer) byID(r *http.Request) *rpcJob {
	id, err := strconv.Atoi(r.PathValue("id"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || id < 1 || id > len(s.jobs) {
		return nil
	}
	return s.jobs[id-1]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// jobWriter captures a child's combined output, keeping a bounded log and the
// latest progress line (progress lines contain " segs | " — a stable contract,
// see engine.Progress.Line).
type jobWriter struct {
	s *rpcServer
	j *rpcJob
}

func (jw *jobWriter) Write(p []byte) (int, error) {
	jw.s.mu.Lock()
	defer jw.s.mu.Unlock()
	jw.j.log = append(jw.j.log, p...)
	if len(jw.j.log) > rpcLogCap {
		jw.j.log = append([]byte(nil), jw.j.log[len(jw.j.log)-rpcLogCap/2:]...)
	}
	for _, line := range strings.Split(string(p), "\n") {
		if strings.Contains(line, " segs | ") {
			jw.j.Progress = strings.TrimSpace(line)
		}
	}
	return len(p), nil
}
