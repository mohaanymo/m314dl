package serve

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// -spool-dir is honoured (and created); a spool made under the base lands
// inside it.
func TestSpoolBaseOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "deep", "spool")
	base, err := SpoolBase(want)
	if err != nil {
		t.Fatal(err)
	}
	if base != want {
		t.Fatalf("SpoolBase = %q, want %q", base, want)
	}
	dir, err := os.MkdirTemp(base, "m314dl-serve-*")
	if err != nil {
		t.Fatalf("spool not creatable under override: %v", err)
	}
	if !strings.HasPrefix(dir, want) {
		t.Fatalf("spool %q not under -spool-dir %q", dir, want)
	}
}

// With no override the spool base is the current directory, not the system
// temp dir (tmpfs — RAM — on most Linux hosts).
func TestSpoolBaseDefaultsToCurrentDir(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("TMPDIR", filepath.Join(cwd, "not-here"))

	base, err := SpoolBase("")
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(cwd, base); err != nil || rel != "." {
		t.Fatalf("SpoolBase = %q, want the current directory %q", base, cwd)
	}
	if strings.HasPrefix(base, os.TempDir()) {
		t.Fatalf("SpoolBase = %q fell back to the system temp dir although cwd is writable", base)
	}
	if left, _ := filepath.Glob(filepath.Join(cwd, ".m314dl-probe-*")); len(left) != 0 {
		t.Fatalf("writability probe left %v behind", left)
	}
}

// The RAM-backed check is false for a normal directory on every OS, and true
// for a real tmpfs mount on Linux when one is available.
func TestRAMBacked(t *testing.T) {
	if ramBacked(".") {
		t.Skip("package directory itself is on tmpfs; cannot use it as the real-disk sample")
	}
	if ramBacked(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a nonexistent path must not be reported RAM-backed")
	}
	if runtime.GOOS != "linux" {
		return
	}
	for _, mnt := range []string{"/dev/shm", "/run", "/tmp"} {
		var st syscall.Statfs_t
		if syscall.Statfs(mnt, &st) != nil || uint32(st.Type) != 0x01021994 {
			continue // not a tmpfs on this box
		}
		if !ramBacked(mnt) {
			t.Fatalf("%s is tmpfs but ramBacked reported false", mnt)
		}
		return
	}
	t.Log("no tmpfs mount found; positive check skipped")
}
