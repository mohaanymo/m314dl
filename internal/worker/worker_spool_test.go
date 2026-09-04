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
	base := t.TempDir()

	w := newWorkerServer("tok", 8, "", "test", nil)
	if err := w.useSpool("127.0.0.1:7001", base); err != nil {
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
	if err := w2.useSpool("127.0.0.1:7001", base); err != nil {
		t.Fatalf("useSpool: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a restart left the previous run's staging directory behind")
	}
}

// Two workers on one host must not clear each other's staging area.
func TestWorkersOnDifferentPortsDoNotShareASpool(t *testing.T) {
	base := t.TempDir()

	a := newWorkerServer("tok", 8, "", "test", nil)
	if err := a.useSpool("127.0.0.1:7001", base); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(a.spool, "m314dl-ch-running")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}

	b := newWorkerServer("tok", 8, "", "test", nil)
	if err := b.useSpool("127.0.0.1:7002", base); err != nil {
		t.Fatal(err)
	}
	if a.spool == b.spool {
		t.Fatal("two workers share one staging directory; a restart of either would delete the other's live segments")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("starting the second worker destroyed the first's live channel: %v", err)
	}
}

// -spool-dir puts the spool exactly where the operator asked.
func TestSpoolHonoursSpoolDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "fresh", "spool") // created on demand
	w := newWorkerServer("tok", 8, "", "test", nil)
	if err := w.useSpool("0.0.0.0:7001", base); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.spool, base) {
		t.Fatalf("spool = %q, want it under -spool-dir %q", w.spool, base)
	}
	if fi, err := os.Stat(w.spool); err != nil || !fi.IsDir() {
		t.Fatalf("spool dir not created: %v", err)
	}
}

// Without -spool-dir the spool lands in the current directory — real disk in
// the normal case — never in the system temp dir, which is tmpfs (RAM) on
// most Linux hosts and was where a large VOD restream got OOM-killed.
func TestSpoolDefaultsToCurrentDir(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("TMPDIR", filepath.Join(cwd, "not-here"))

	w := newWorkerServer("tok", 8, "", "test", nil)
	if err := w.useSpool("0.0.0.0:7001", ""); err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(cwd, w.spool); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("spool = %q, want it under the current directory %q", w.spool, cwd)
	}
	if strings.HasPrefix(w.spool, os.TempDir()) {
		t.Fatalf("spool = %q landed in the system temp dir", w.spool)
	}
}
