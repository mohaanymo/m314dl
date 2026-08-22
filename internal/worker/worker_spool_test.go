package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A worker clears its own staging area on startup.
//
// A channel removes its directory when it stops, but a killed or restarted
// process never does — and the leftovers are invisible, unbounded, and sit on
// the same disk the live segments are being written to. One restart-heavy day
// left 1430 directories and 43GB behind.
func TestAWorkerClearsItsSpoolOnStartup(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	w := newWorkerServer("tok", 8, "", "test", nil)
	if err := w.useSpool("127.0.0.1:7001"); err != nil {
		t.Fatalf("useSpool: %v", err)
	}
	// Something a previous run left behind.
	stale := filepath.Join(w.spool, "m314dl-ch-old-123")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "video.mp4.part1"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The next start of the same worker reclaims it.
	w2 := newWorkerServer("tok", 8, "", "test", nil)
	if err := w2.useSpool("127.0.0.1:7001"); err != nil {
		t.Fatalf("useSpool: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a restart left the previous run's staging directory behind")
	}
}

// Two workers on one host must not clear each other's staging area.
func TestWorkersOnDifferentPortsDoNotShareASpool(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	a := newWorkerServer("tok", 8, "", "test", nil)
	if err := a.useSpool("127.0.0.1:7001"); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(a.spool, "m314dl-ch-running")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}

	b := newWorkerServer("tok", 8, "", "test", nil)
	if err := b.useSpool("127.0.0.1:7002"); err != nil {
		t.Fatal(err)
	}
	if a.spool == b.spool {
		t.Fatal("two workers share one staging directory; a restart of either would delete the other's live segments")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("starting the second worker destroyed the first's live channel: %v", err)
	}
}

// The spool must sit under TMPDIR, so an operator can move it to a tmpfs.
func TestSpoolHonoursTMPDIR(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	w := newWorkerServer("tok", 8, "", "test", nil)
	if err := w.useSpool("0.0.0.0:7001"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.spool, tmp) {
		t.Fatalf("spool = %q, want it under TMPDIR %q", w.spool, tmp)
	}
}
