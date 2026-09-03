package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.3.3", "0.3.4", true},
		{"0.3.3", "v0.3.4", true},     // leading v ignored
		{"0.3.4", "0.3.4", false},     // equal
		{"0.3.4", "0.3.3", false},     // older
		{"0.3.4", "0.4.0", true},      // minor bump
		{"0.3.4", "1.0.0", true},      // major bump
		{"0.9.9", "0.10.0", true},     // numeric, not lexical
		{"1.0.0", "0.9.9", false},     // major dominates
		{"0.3.4", "0.3.4-rc1", false}, // suffix ignored, so not newer
		{"0.3.4-rc1", "0.3.4", false}, // both parse to 0.3.4
		{"0.3", "0.3.1", true},        // missing patch treated as 0
		{"0.3.4", "", false},          // no data
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestCheckUsesFreshCacheWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "m314dl")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	// A fresh cache advertising a newer tag: Check must use it, no network.
	body := fmt.Sprintf(`{"checked_at":%d,"tag":"v9.9.9"}`, time.Now().Unix())
	os.WriteFile(filepath.Join(seed, "update-check.json"), []byte(body), 0o644)

	if got := Check(context.Background(), "0.3.3", dir); got != "v9.9.9" {
		t.Fatalf("Check = %q, want v9.9.9 from the fresh cache", got)
	}
	// Already up to date → no notice even though the cache has a tag.
	if got := Check(context.Background(), "9.9.9", dir); got != "" {
		t.Fatalf("Check = %q, want empty when current is up to date", got)
	}
}
