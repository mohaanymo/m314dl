package engine

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// Progress aggregates counters across all concurrently-downloading streams.
// Speed is a sliding 5-second window, not a lifetime average.
type Progress struct {
	total      atomic.Int64
	done       atomic.Int64
	bytes      atomic.Int64
	knownBytes atomic.Int64

	mu      sync.Mutex
	samples []sample // ring of (time, bytes) for windowed speed
	live    bool
	start   time.Time
}

type sample struct {
	t time.Time
	b int64
}

func NewProgress(live bool) *Progress {
	return &Progress{live: live, start: time.Now()}
}

func (p *Progress) AddTotal(n int64)      { p.total.Add(n) }
func (p *Progress) AddDone(n int64)       { p.done.Add(n) }
func (p *Progress) SetKnownBytes(n int64) { p.knownBytes.Store(n) }

func (p *Progress) AddBytes(n int64) {
	p.bytes.Add(n)
	p.mu.Lock()
	p.samples = append(p.samples, sample{time.Now(), p.bytes.Load()})
	if len(p.samples) > 256 {
		p.samples = p.samples[128:]
	}
	p.mu.Unlock()
}

// Speed returns bytes/sec over the last 5 seconds.
func (p *Progress) Speed() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	cur := p.bytes.Load()
	for _, s := range p.samples {
		if now.Sub(s.t) <= 5*time.Second {
			dt := now.Sub(s.t).Seconds()
			if dt < 0.1 {
				return 0
			}
			return float64(cur-s.b) / dt
		}
	}
	return 0
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}

// Line renders one status line.
func (p *Progress) Line() string {
	done, total := p.done.Load(), p.total.Load()
	bytes := p.bytes.Load()
	speed := p.Speed()
	elapsed := time.Since(p.start).Round(time.Second)
	if p.live {
		return fmt.Sprintf("REC %s | %d segs | %s | %s/s", elapsed, done, fmtBytes(bytes), fmtBytes(int64(speed)))
	}
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}
	eta := "?"
	if speed > 1 && total > done && done > 8 {
		perSeg := float64(bytes) / float64(done)
		etaSec := perSeg * float64(total-done) / speed
		if etaSec < 360000 {
			eta = (time.Duration(etaSec) * time.Second).Round(time.Second).String()
		}
	}
	return fmt.Sprintf("%5.1f%% | %d/%d segs | %s | %s/s | ETA %s", pct, done, total, fmtBytes(bytes), fmtBytes(int64(speed)), eta)
}

// Render prints progress to stderr until stop is closed. On a TTY it rewrites
// one line; otherwise it prints a plain line every 5s (machine-parseable,
// survives redirection — no ANSI garbage).
func (p *Progress) Render(stop <-chan struct{}) {
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))
	interval := time.Second
	if !isTTY {
		interval = 5 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			if isTTY {
				fmt.Fprintf(os.Stderr, "\r\x1b[2K%s\n", p.Line())
			} else {
				fmt.Fprintln(os.Stderr, p.Line())
			}
			return
		case <-tick.C:
			if isTTY {
				line := p.Line()
				if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && len(line) > w && w > 4 {
					line = line[:w-1]
				}
				fmt.Fprintf(os.Stderr, "\r\x1b[2K%s", line)
			} else {
				fmt.Fprintln(os.Stderr, p.Line())
			}
		}
	}
}
