// Package update checks GitHub for a newer released version, so a user running
// an old binary is told one exists. It never blocks the real work and stays
// silent on any error — a missed check is not worth a warning.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Page is where a user goes to get the new build.
	Page       = "https://github.com/mohaanymo/m314dl/releases/latest"
	releaseAPI = "https://api.github.com/repos/mohaanymo/m314dl/releases/latest"
	// checkEvery bounds how often the network is hit: at most once a day.
	checkEvery = 24 * time.Hour
)

// Check returns the latest release tag when it is newer than current, else "".
// It consults a once-a-day cache under cacheDir before hitting the network, and
// swallows every error (no network, GitHub down, malformed cache) — the caller
// prints a notice only when a real newer version is found.
func Check(ctx context.Context, current, cacheDir string) string {
	cf := filepath.Join(cacheDir, "m314dl", "update-check.json")
	var c struct {
		CheckedAt int64  `json:"checked_at"`
		Tag       string `json:"tag"`
	}
	if b, err := os.ReadFile(cf); err == nil {
		json.Unmarshal(b, &c)
	}
	if time.Since(time.Unix(c.CheckedAt, 0)) >= checkEvery {
		if tag, err := fetchLatestTag(ctx); err == nil {
			c.CheckedAt, c.Tag = time.Now().Unix(), tag
			if b, err := json.Marshal(c); err == nil {
				os.MkdirAll(filepath.Dir(cf), 0o755)
				os.WriteFile(cf, b, 0o644)
			}
		}
	}
	if Newer(current, c.Tag) {
		return c.Tag
	}
	return ""
}

func fetchLatestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "m314dl") // GitHub rejects a request with no UA
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github status %d", resp.StatusCode)
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return "", err
	}
	return r.TagName, nil
}

// Newer reports whether latest is a higher version than current. Both are
// dotted numeric versions with an optional leading "v" and any trailing
// pre-release/build suffix, which is ignored (0.3.4, v0.3.4, 0.3.4-rc1).
func Newer(current, latest string) bool {
	c, l := parse(current), parse(latest)
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parse(s string) [3]int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var out [3]int
	for i, part := range strings.SplitN(s, ".", 4) {
		if i >= 3 {
			break
		}
		n := 0
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				break // stop at the first non-digit (a "-rc1" suffix, say)
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out
}
