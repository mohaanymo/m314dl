package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRPCAuthAndValidation(t *testing.T) {
	s := &rpcServer{exe: "/bin/echo", secret: "tok"}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	post := func(path, body, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := post("/add", `{"url":"http://x/a.m3u8"}`, ""); rec.Code != 401 {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	if rec := post("/add", `{}`, "tok"); rec.Code != 400 {
		t.Fatalf("missing url: got %d, want 400", rec.Code)
	}
	if rec := post("/add", `{"url":"http://x","args":["-rpc",":1"]}`, "tok"); rec.Code != 400 {
		t.Fatalf("-rpc in args: got %d, want 400", rec.Code)
	}

	rec := post("/add", `{"url":"http://x/a.m3u8","args":["-list"]}`, "tok")
	if rec.Code != 200 {
		t.Fatalf("add: got %d: %s", rec.Code, rec.Body)
	}
	var added map[string]int
	json.Unmarshal(rec.Body.Bytes(), &added)
	if added["id"] != 1 {
		t.Fatalf("id = %d, want 1", added["id"])
	}

	// /bin/echo exits immediately; wait for the reaper to mark it done
	deadline := time.Now().Add(5 * time.Second)
	for {
		req := httptest.NewRequest("GET", "/jobs/1", nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, req)
		var j rpcJob
		json.Unmarshal(rec.Body.Bytes(), &j)
		if j.State == "done" {
			if !strings.Contains(j.Log, "a.m3u8") {
				t.Fatalf("log missing echoed args: %q", j.Log)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished: state=%q", j.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRPCRequiresSecretOffLoopback(t *testing.T) {
	if err := serveRPC("0.0.0.0:0", ""); err == nil || !strings.Contains(err.Error(), "rpc-secret") {
		t.Fatalf("got %v, want rpc-secret error", err)
	}
}
