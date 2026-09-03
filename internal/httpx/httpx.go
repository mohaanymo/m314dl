// Package httpx is the shared HTTP layer: default headers, cookies, proxy,
// and body-read-covering retries with exponential backoff.
package httpx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

type Options struct {
	Headers    map[string]string
	Proxy      string // http://, https://, socks5:// (user:pass@ supported)
	CookieFile string // Netscape cookies.txt
	Insecure   bool   // skip TLS verify — explicit opt-in only
	Timeout    time.Duration
	Retries    int // per-request attempts beyond the first
}

type Client struct {
	hc      *http.Client
	headers map[string]string
	retries int
}

func New(o Options) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxConnsPerHost = 0
	// Keep many connections warm in the idle pool so sustained high concurrency
	// reuses TCP+TLS instead of re-handshaking per segment. Sized well above the
	// usual -t so several concurrent streams (video+audio+subs) don't churn.
	tr.MaxIdleConns = 512
	tr.MaxIdleConnsPerHost = 256
	// Deliberately DO NOT use HTTP/2. A CDN that offers h2 makes Go pool every
	// concurrent segment request onto ONE TCP connection (h2 multiplexing). One
	// TCP flow is throughput-limited by RTT and packet loss (≈ MSS/(RTT·√loss)),
	// so on a real CDN it can't fill the pipe — invisible on localhost (RTT≈0),
	// crippling over the network. HTTP/1.1 opens one connection per in-flight
	// request instead, giving N independent flows (what N_m3u8DL/aria2/yt-dlp do)
	// and ~N× the aggregate throughput.
	//
	// Disabling h2 takes BOTH steps: empty (non-nil) TLSNextProto stops Go's
	// automatic h2 upgrade, AND the ALPN offer must advertise only http/1.1 —
	// otherwise the server still selects h2 over TLS and answers a request Go
	// parses as HTTP/1.1 with HTTP/2 frames ("malformed HTTP response \x00\x00..").
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.TLSClientConfig = &tls.Config{
		NextProtos:         []string{"http/1.1"},
		InsecureSkipVerify: o.Insecure,
	}
	if o.Proxy != "" {
		pu, err := url.Parse(o.Proxy)
		if err != nil {
			return nil, fmt.Errorf("bad proxy url: %w", err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	// A data: URL answers from its own payload: a manifest can carry an inline
	// init segment (Smooth Streaming synthesizes one) and the engine fetches it
	// through the same client, retries and all, with no special case.
	tr.RegisterProtocol("data", dataTransport{})
	hc := &http.Client{Transport: tr, Timeout: o.Timeout, CheckRedirect: noReferer}
	if o.CookieFile != "" {
		jar, err := loadNetscapeCookies(o.CookieFile)
		if err != nil {
			return nil, err
		}
		hc.Jar = jar
	} else {
		// In-memory jar so a Set-Cookie on the manifest is carried to the
		// segment requests. Disney+ hands out an `hdntl` auth cookie on the
		// manifest response and every segment 403s without it — the manifest
		// is fetched first through this same client, so the cookie is already
		// stored by the time segments download.
		hc.Jar, _ = cookiejar.New(nil)
	}
	headers := map[string]string{"User-Agent": DefaultUA}
	for k, v := range o.Headers {
		headers[http.CanonicalHeaderKey(k)] = v
	}
	r := o.Retries
	if r <= 0 {
		r = 5
	}
	return &Client{hc: hc, headers: headers, retries: r}, nil
}

// dataTransport serves data:[<mediatype>][;base64],<payload> URLs (RFC 2397)
// as a 200 response. Range headers are ignored: the payload is tiny and the
// caller already copes with a server that answers a ranged request in full.
type dataTransport struct{}

func (dataTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	meta, payload, ok := strings.Cut(req.URL.Opaque, ",")
	if !ok {
		return nil, fmt.Errorf("data: URL has no payload")
	}
	var body []byte
	if strings.HasSuffix(meta, ";base64") {
		b, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, fmt.Errorf("data: URL: %w", err)
		}
		body, meta = b, strings.TrimSuffix(meta, ";base64")
	} else {
		s, err := url.PathUnescape(payload)
		if err != nil {
			return nil, fmt.Errorf("data: URL: %w", err)
		}
		body = []byte(s)
	}
	if meta == "" {
		meta = "text/plain"
	}
	return &http.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {meta}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// maxRedirects matches net/http's own default, which replacing CheckRedirect
// would otherwise silently remove.
const maxRedirects = 10

// noReferer follows redirects without the Referer header net/http adds by itself.
//
// Go sets `Referer: <previous url>` on every request it makes while following a
// redirect. Tokenized CDN URLs reject a request carrying one: a live DASH
// manifest behind Akamai answers 200 when fetched directly and 403 through Go's
// redirect, from the same address, the same second, with otherwise identical
// headers. The failure looks nothing like its cause — the 403 body parses as a
// manifest with no PSSH, so a DRM source simply never finds a key, and a clear
// one reports the source as unavailable.
//
// A downloader has no business inventing a Referer the caller did not set, so
// this is unconditional. Any header the caller did set is still carried through
// the redirect.
func noReferer(req *http.Request, via []*http.Request) error {
	req.Header.Del("Referer")
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

// Do sends a request with default headers applied.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	for k, v := range c.headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	return c.hc.Do(req)
}

// Retries is the configured per-request retry count.
func (c *Client) Retries() int { return c.retries }

// Retriable reports whether a status is worth retrying (exported for streaming
// callers that run their own retry loop).
func Retriable(status int) bool { return retriable(status) }

// ParseRetryAfter parses a Retry-After header value (exported).
func ParseRetryAfter(v string) time.Duration { return parseRetryAfter(v) }

// RangeGet issues a single GET with the given Range header (default headers,
// cookies and proxy applied) and returns the response for streaming. The caller
// owns resp.Body and runs its own retry loop. No redirects-vs-headers subtlety
// beyond the shared client's.
func (c *Client) RangeGet(ctx context.Context, rawURL, rangeHdr string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	return c.Do(req)
}

// retriable reports whether an HTTP status is worth retrying.
func retriable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

type StatusError struct {
	Code       int
	URL        string
	RetryAfter time.Duration // parsed from the Retry-After header (0 if absent)
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.URL) }

// FetchBytes GETs a URL (optionally with a Range header) and returns the full
// body plus the final post-redirect URL.
func (c *Client) FetchBytes(ctx context.Context, rawURL, rangeHdr string) ([]byte, string, error) {
	body, finalURL, _, err := c.FetchBytesEx(ctx, rawURL, rangeHdr)
	return body, finalURL, err
}

// FetchBytesEx is FetchBytes plus a pressure count: how many times the server
// pushed back with a retriable status (429/503/…). The concurrency controller
// uses it to back off before those retries turn into failures. Retries cover
// connection errors, retriable statuses AND mid-body read errors, with
// exponential backoff+jitter, and honor a Retry-After header when present.
func (c *Client) FetchBytesEx(ctx context.Context, rawURL, rangeHdr string) (body []byte, finalURL string, pressure int, err error) {
	finalURL = rawURL
	backoff := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		body, finalURL, err = c.fetchOnce(ctx, rawURL, rangeHdr)
		if err == nil {
			return body, finalURL, pressure, nil
		}
		var se *StatusError
		isStatus := errors.As(err, &se)
		if isStatus && retriable(se.Code) {
			pressure++ // server pushed back — signal even if we recover
		}
		permanent := isStatus && !retriable(se.Code)
		if permanent || attempt >= c.retries || ctx.Err() != nil {
			return nil, finalURL, pressure, err
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)))
		if isStatus && se.RetryAfter > 0 { // honor Retry-After, capped
			wait = se.RetryAfter
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return nil, finalURL, pressure, ctx.Err()
		case <-time.After(wait):
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) fetchOnce(ctx context.Context, rawURL, rangeHdr string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, rawURL, err
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, rawURL, err
	}
	defer resp.Body.Close()
	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	if resp.StatusCode >= 400 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, final, &StatusError{Code: resp.StatusCode, URL: rawURL, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, final, fmt.Errorf("read body: %w", err)
	}
	return b, final, nil
}

// parseRetryAfter reads a Retry-After header (delta-seconds or HTTP-date).
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Head issues a HEAD request (no retries — callers treat failure as "unknown").
func (c *Client) Head(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}

// loadNetscapeCookies parses a cookies.txt file into a static jar.
func loadNetscapeCookies(path string) (http.CookieJar, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	jar := &staticJar{byHost: map[string][]*http.Cookie{}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "#HttpOnly_")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		exp, _ := strconv.ParseInt(parts[4], 10, 64)
		ck := &http.Cookie{
			Domain: strings.TrimPrefix(parts[0], "."),
			Path:   parts[2],
			Secure: parts[3] == "TRUE",
			Name:   parts[5],
			Value:  parts[6],
		}
		if exp > 0 {
			ck.Expires = time.Unix(exp, 0)
		}
		jar.byHost[ck.Domain] = append(jar.byHost[ck.Domain], ck)
	}
	return jar, sc.Err()
}

// staticJar matches cookies by domain suffix; read-only after load.
// ponytail: no Set-Cookie persistence; swap for net/http/cookiejar + seed if
// server-set cookies ever matter.
type staticJar struct {
	byHost map[string][]*http.Cookie
}

func (j *staticJar) SetCookies(*url.URL, []*http.Cookie) {}

func (j *staticJar) Cookies(u *url.URL) []*http.Cookie {
	host := u.Hostname()
	var out []*http.Cookie
	for dom, cks := range j.byHost {
		if host == dom || strings.HasSuffix(host, "."+dom) {
			for _, c := range cks {
				if c.Path == "" || strings.HasPrefix(u.Path, c.Path) {
					if c.Secure && u.Scheme != "https" {
						continue
					}
					out = append(out, c)
				}
			}
		}
	}
	return out
}
