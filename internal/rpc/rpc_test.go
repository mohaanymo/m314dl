package rpc

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func rpcPost(t *testing.T, s *rpcServer, path, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func rpcGet(t *testing.T, s *rpcServer, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestRPCAuthAndValidation(t *testing.T) {
	s := newRPCServer("/bin/echo", "tok", 64, time.Hour)

	if rec := rpcPost(t, s, "/add", `{"url":"http://x/a.m3u8"}`, ""); rec.Code != 401 {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	if rec := rpcPost(t, s, "/add", `{}`, "tok"); rec.Code != 400 {
		t.Fatalf("missing url: got %d, want 400", rec.Code)
	}
	if rec := rpcPost(t, s, "/add", `{"url":"http://x","args":["-rpc",":1"]}`, "tok"); rec.Code != 400 {
		t.Fatalf("-rpc in args: got %d, want 400", rec.Code)
	}

	rec := rpcPost(t, s, "/add", `{"url":"http://x/a.m3u8","args":["-list"]}`, "tok")
	if rec.Code != 200 {
		t.Fatalf("add: got %d: %s", rec.Code, rec.Body)
	}
	var added map[string]int64
	json.Unmarshal(rec.Body.Bytes(), &added)
	if added["id"] != 1 {
		t.Fatalf("id = %d, want 1", added["id"])
	}

	// /bin/echo exits immediately; wait for the reaper goroutine to mark it done.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var v jobView
		json.Unmarshal(rpcGet(t, s, "/jobs/1", "tok").Body.Bytes(), &v)
		if v.State == "done" {
			if !strings.Contains(v.Log, "a.m3u8") {
				t.Fatalf("log missing echoed args: %q", v.Log)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished: state=%q", v.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The admission cap rejects /add once maxJobs are running, so a flood can't
// fork-bomb the host.
func TestRPCAdmissionCap(t *testing.T) {
	s := newRPCServer("/bin/echo", "", 1, time.Hour)
	s.mu.Lock()
	s.running = 1 // simulate one job already running
	s.mu.Unlock()
	rec := rpcPost(t, s, "/add", `{"url":"http://x/a.m3u8"}`, "")
	if rec.Code != 503 {
		t.Fatalf("at capacity should 503, got %d: %s", rec.Code, rec.Body)
	}
}

// Finished jobs are reaped after the retention window, and IDs never reuse a
// reaped id (monotonic), so a caller can't accidentally address a recycled job.
func TestRPCReapAndMonotonicIDs(t *testing.T) {
	s := newRPCServer("/bin/echo", "", 64, time.Hour)

	var first map[string]int64
	json.Unmarshal(rpcPost(t, s, "/add", `{"url":"http://x/1"}`, "").Body.Bytes(), &first)
	if first["id"] != 1 {
		t.Fatalf("first id = %d, want 1", first["id"])
	}
	// Wait for it to finish (endedNano set).
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		j := s.jobs[1]
		s.mu.RUnlock()
		if j != nil && j.endedNano.Load() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job 1 never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Reap with a clock far past the retention window: job 1 must be dropped.
	s.reap(time.Now().Add(2 * time.Hour))
	if rec := rpcGet(t, s, "/jobs/1", ""); rec.Code != 404 {
		t.Fatalf("reaped job should 404, got %d", rec.Code)
	}

	// A new job gets id 2 — the reaped id 1 is never reused.
	var second map[string]int64
	json.Unmarshal(rpcPost(t, s, "/add", `{"url":"http://x/2"}`, "").Body.Bytes(), &second)
	if second["id"] != 2 {
		t.Fatalf("second id = %d, want 2 (ids must not recycle after reap)", second["id"])
	}
}

// stopAll signals every running child, so graceful shutdown actually stops
// in-flight jobs instead of orphaning them.
func TestRPCStopAll(t *testing.T) {
	s := newRPCServer("/bin/sh", "", 4, time.Hour)
	// A job that becomes `sleep 30`; a signal terminates it (no orphan).
	if rec := rpcPost(t, s, "/add", `{"url":"x","args":["-c","exec sleep 30"]}`, ""); rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	s.mu.RLock()
	j := s.jobs[1]
	s.mu.RUnlock()
	if j == nil {
		t.Fatal("job not registered")
	}

	s.stopAll()

	deadline := time.Now().Add(5 * time.Second)
	for j.endedNano.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("stopAll did not stop the running job")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRPCHealth(t *testing.T) {
	s := newRPCServer("/bin/echo", "", 32, time.Hour)
	var h map[string]any
	json.Unmarshal(rpcGet(t, s, "/health", "").Body.Bytes(), &h)
	if h["status"] != "ok" || h["max"].(float64) != 32 {
		t.Fatalf("health: %v", h)
	}
}

func TestRPCRequiresSecretOffLoopback(t *testing.T) {
	if err := ServeRPC("0.0.0.0:0", "", 64, time.Hour, "test"); err == nil || !strings.Contains(err.Error(), "rpc-secret") {
		t.Fatalf("got %v, want rpc-secret error", err)
	}
}
