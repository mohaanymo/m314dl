// Package httpx is the shared HTTP layer: default headers, cookies, proxy,
// and body-read-covering retries with exponential backoff.
package httpx

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
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
	tr.MaxIdleConnsPerHost = 64
	tr.ForceAttemptHTTP2 = true
	if o.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if o.Proxy != "" {
		pu, err := url.Parse(o.Proxy)
		if err != nil {
			return nil, fmt.Errorf("bad proxy url: %w", err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	hc := &http.Client{Transport: tr, Timeout: o.Timeout}
	if o.CookieFile != "" {
		jar, err := loadNetscapeCookies(o.CookieFile)
		if err != nil {
			return nil, err
		}
		hc.Jar = jar
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

// Do sends a request with default headers applied.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	for k, v := range c.headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	return c.hc.Do(req)
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
	Code int
	URL  string
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.URL) }

// FetchBytes GETs a URL (optionally with a Range header) and returns the full
// body plus the final post-redirect URL. Retries cover connection errors,
// retriable statuses AND mid-body read errors, with exponential backoff+jitter.
func (c *Client) FetchBytes(ctx context.Context, rawURL, rangeHdr string) (body []byte, finalURL string, err error) {
	finalURL = rawURL
	backoff := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		body, finalURL, err = c.fetchOnce(ctx, rawURL, rangeHdr)
		if err == nil {
			return body, finalURL, nil
		}
		var se *StatusError
		permanent := errors.As(err, &se) && !retriable(se.Code)
		if permanent || attempt >= c.retries || ctx.Err() != nil {
			return nil, finalURL, err
		}
		select {
		case <-ctx.Done():
			return nil, finalURL, ctx.Err()
		case <-time.After(backoff + time.Duration(rand.Int64N(int64(backoff/2)))):
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
		return nil, final, &StatusError{Code: resp.StatusCode, URL: rawURL}
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, final, fmt.Errorf("read body: %w", err)
	}
	return b, final, nil
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
