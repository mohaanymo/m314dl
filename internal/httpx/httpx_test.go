package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("5"); d != 5*time.Second {
		t.Fatalf("seconds: got %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Fatalf("empty: got %v", d)
	}
	if d := parseRetryAfter("-3"); d != 0 {
		t.Fatalf("negative: got %v", d)
	}
	// HTTP-date in the future yields a positive, bounded duration
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d <= 0 || d > 11*time.Second {
		t.Fatalf("http-date: got %v", d)
	}
	// past date → 0
	if d := parseRetryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)); d != 0 {
		t.Fatalf("past date: got %v", d)
	}
}

func TestFetchBytesExPressure(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 { // first two attempts get rate-limited
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("payload"))
	}))
	defer srv.Close()

	c, err := New(Options{Retries: 5})
	if err != nil {
		t.Fatal(err)
	}
	body, _, pressure, err := c.FetchBytesEx(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("should recover after 429s: %v", err)
	}
	if string(body) != "payload" {
		t.Fatalf("body = %q", body)
	}
	if pressure != 2 {
		t.Fatalf("pressure = %d, want 2 (two 429s before success)", pressure)
	}
}

func TestFetchBytesPermanentNoRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound) // 404 is permanent
	}))
	defer srv.Close()
	c, _ := New(Options{Retries: 5})
	if _, _, err := c.FetchBytes(context.Background(), srv.URL, ""); err == nil {
		t.Fatal("404 should error")
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("404 should not retry: %d requests", n)
	}
}
