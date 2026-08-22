package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRedirectSendsNoReferer is a regression test for a live failure against a
// real CDN, not a style preference.
//
// Go's http.Client adds `Referer: <previous url>` to every request it makes
// while following a redirect. Akamai's tokenized edge URLs reject a request
// carrying one: the same live DASH manifest answered 200 fetched directly and
// 403 fetched through Go's redirect, from the same address, the same second,
// with otherwise identical headers.
//
// The symptom looked nothing like the cause. The 403 body parsed as a manifest
// containing no PSSH, so a DRM channel silently never found a key.
func TestRedirectSendsNoReferer(t *testing.T) {
	var sawReferer string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/signed", http.StatusFound)
		case "/signed":
			sawReferer = r.Header.Get("Referer")
			if sawReferer != "" {
				http.Error(w, "Access Denied", http.StatusForbidden)
				return
			}
			// Headers the caller set must still survive the redirect.
			if r.Header.Get("X-Caller") != "kept" {
				http.Error(w, "caller header lost across the redirect", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("<MPD>pssh</MPD>"))
		}
	}))
	defer srv.Close()

	c, err := New(Options{Retries: 1, Headers: map[string]string{"X-Caller": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := c.FetchBytes(context.Background(), srv.URL+"/start", "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if sawReferer != "" {
		t.Fatalf("a Referer reached the redirect target: %q", sawReferer)
	}
	if !strings.Contains(string(body), "pssh") {
		t.Fatalf("body: %q", body)
	}
}

// TestRedirectLoopStops keeps the bound net/http applies by default, which
// replacing CheckRedirect would otherwise silently remove.
func TestRedirectLoopStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer srv.Close()

	c, err := New(Options{Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.FetchBytes(context.Background(), srv.URL+"/start", ""); err == nil {
		t.Fatal("an endless redirect loop was followed forever")
	}
}
