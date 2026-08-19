package engine

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"time"

	"github.com/mohamed/m314dl/internal/httpx"
)

// partPath is where a segment's raw (still-encrypted, still-fake-header) bytes
// stream to before the writer decrypts and appends them. Derived from the
// output so concurrent downloads never collide, and stable across reruns so a
// killed download resumes from the partial bytes already on disk.
func partPath(outPath string, idx int64) string {
	return fmt.Sprintf("%s.part%d", outPath, idx)
}

// downloadSegment streams one segment's raw body to its part file, resuming from
// whatever bytes are already there via HTTP byte ranges, retrying transient
// failures. It does NOT decrypt — the writer does that in index order, one
// segment at a time, so peak memory is one segment rather than one per worker.
// Returns the count of rate-limit (429/5xx) responses seen, for the controller.
func downloadSegment(ctx context.Context, cfg Config, ctl *controller, it item, path string) (int, error) {
	// segment byte-range (HLS EXT-X-BYTERANGE / DASH SegmentBase); else whole object
	var segStart, segEnd int64 = 0, -1
	if it.rng != nil {
		segStart, segEnd = it.rng.Start, it.rng.End
	}
	segLen := int64(-1)
	if segEnd >= 0 {
		segLen = segEnd - segStart + 1
	}

	var have int64
	if fi, err := os.Stat(path); err == nil {
		have = fi.Size()
	}
	if segLen >= 0 && have >= segLen {
		return 0, nil // ranged segment already complete on disk
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return 0, err
	}

	pressure := 0
	backoff := 500 * time.Millisecond
	for attempt := 0; ; attempt++ {
		rngHdr := resumeRange(it.rng != nil, segStart, segEnd, have)
		// Per-attempt idle watchdog: a throttling CDN (common when concurrency is
		// pushed high) often holds the connection open and sends nothing rather
		// than returning 429. Without this the read below blocks forever and the
		// whole download stalls at 0 B/s. The watchdog cancels this attempt if no
		// byte arrives for segIdleTimeout — headers or body — so the retry loop
		// resumes from the bytes already on disk. ctx.Err() stays nil (only the
		// child was cancelled), which is exactly why these paths must treat a
		// cancelled attempt as retryable, not fatal.
		reqCtx, cancel := context.WithCancel(ctx)
		wd := time.AfterFunc(segIdleTimeout, cancel)
		resp, err := cfg.Client.RangeGet(reqCtx, it.url, rngHdr)
		if err != nil {
			wd.Stop()
			cancel()
			if ctx.Err() != nil || attempt >= cfg.Client.Retries() {
				return pressure, err
			}
			if !retryWait(ctx, &backoff, 0) {
				return pressure, ctx.Err()
			}
			continue
		}

		switch {
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
			resp.Body.Close()
			wd.Stop()
			cancel()
			return pressure, nil // server says we already have it all
		case resp.StatusCode == http.StatusOK && have > 0:
			// server ignored Range: restart from the beginning
			resp.Body.Close()
			wd.Stop()
			cancel()
			if err := f.Truncate(0); err != nil {
				return pressure, err
			}
			f.Seek(0, io.SeekStart)
			have = 0
			continue
		case resp.StatusCode >= 400:
			code := resp.StatusCode
			ra := httpx.ParseRetryAfter(resp.Header.Get("Retry-After"))
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			wd.Stop()
			cancel()
			if httpx.Retriable(code) {
				pressure++
			}
			if !httpx.Retriable(code) || attempt >= cfg.Client.Retries() || ctx.Err() != nil {
				return pressure, &httpx.StatusError{Code: code, URL: it.url}
			}
			if !retryWait(ctx, &backoff, ra) {
				return pressure, ctx.Err()
			}
			continue
		}

		// 2xx: stream the body to disk, counting bytes as they arrive. Each byte
		// received re-arms the watchdog, so a live-but-slow transfer is never cut.
		n, rerr := streamBody(resp.Body, f, cfg.Progress, ctl, func() {
			wd.Reset(segIdleTimeout)
		})
		resp.Body.Close()
		wd.Stop()
		cancel()
		have += n
		if rerr == nil {
			return pressure, nil // complete
		}
		// mid-body failure (including a watchdog cancel): retry, resuming from the
		// bytes we kept. A watchdog stall counts as rate-limit pressure. Note the
		// guard is on the PARENT ctx — a watchdog-cancelled attempt leaves it nil,
		// so it retries; a user abort cancels the parent and stops.
		if attempt >= cfg.Client.Retries() || ctx.Err() != nil {
			return pressure, fmt.Errorf("read body: %w", rerr)
		}
		pressure++
		if !retryWait(ctx, &backoff, 0) {
			return pressure, ctx.Err()
		}
	}
}

// segIdleTimeout bounds how long a single segment fetch may receive no bytes
// before it is abandoned and retried. Generous enough that a slow-but-alive
// link is never cut, short enough that a silently-throttled connection self-heals
// instead of stalling the whole download at 0 B/s.
const segIdleTimeout = 30 * time.Second

// resumeRange builds the Range header to fetch the not-yet-downloaded tail.
func resumeRange(ranged bool, segStart, segEnd, have int64) string {
	if ranged {
		start := segStart + have
		if segEnd >= 0 {
			return fmt.Sprintf("bytes=%d-%d", start, segEnd)
		}
		return fmt.Sprintf("bytes=%d-", start)
	}
	if have > 0 {
		return fmt.Sprintf("bytes=%d-", have)
	}
	return ""
}

// streamBody copies body into f, reporting bytes to progress and the controller
// as they arrive (smooth throughput, and a live counter during a huge segment).
func streamBody(body io.Reader, f *os.File, prog *Progress, ctl *controller, keepalive func()) (int64, error) {
	buf := make([]byte, 1<<20)
	var total int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if keepalive != nil {
				keepalive() // bytes arrived — re-arm the idle watchdog
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if prog != nil {
				prog.AddBytes(int64(n))
			}
			if ctl != nil {
				ctl.addBytes(n)
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// retryWait sleeps with exponential backoff+jitter, or the (capped) Retry-After
// when the server supplied one. Returns false if ctx is cancelled while waiting.
func retryWait(ctx context.Context, backoff *time.Duration, retryAfter time.Duration) bool {
	wait := *backoff + time.Duration(rand.Int64N(int64(*backoff/2)))
	if retryAfter > 0 {
		wait = retryAfter
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
	}
	if *backoff < 8*time.Second {
		*backoff *= 2
	}
	return true
}
