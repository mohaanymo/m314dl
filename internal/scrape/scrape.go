// Package scrape extracts stream manifest URLs from arbitrary web pages.
package scrape

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/mohamed/m314dl/internal/httpx"
)

// absolute URLs in page source / inline JSON (handles \/ escaping)
var absRe = regexp.MustCompile(`https?:(?:\\?/){2}[^\s"'<>\\]+?\.(?:m3u8|mpd)(?:\?[^\s"'<>\\]*)?`)

// relative manifest paths in src/href/file/source attributes
var attrRe = regexp.MustCompile(`(?:src|href|file|source|data-src|content)\s*=\s*["']([^"']+?\.(?:m3u8|mpd)(?:\?[^"']*)?)["']`)

var iframeRe = regexp.MustCompile(`<iframe[^>]+src\s*=\s*["']([^"']+)["']`)

// Find fetches pageURL and returns candidate manifest URLs (deduped, page
// order). Follows iframes one level deep.
// ponytail: static HTML+JSON scan only; JS-built URLs need a headless
// browser — add CDP sniffing if this misses too much.
func Find(ctx context.Context, client *httpx.Client, pageURL string) ([]string, error) {
	urls, err := findInPage(ctx, client, pageURL)
	if err != nil {
		return nil, err
	}
	if len(urls) > 0 {
		return urls, nil
	}
	// one level of iframes
	body, finalURL, err := client.FetchBytes(ctx, pageURL, "")
	if err != nil {
		return nil, err
	}
	for _, m := range iframeRe.FindAllStringSubmatch(string(body), -1) {
		iframeURL := resolve(finalURL, m[1])
		if iframeURL == "" || strings.HasPrefix(iframeURL, "about:") {
			continue
		}
		if found, err := findInPage(ctx, client, iframeURL); err == nil && len(found) > 0 {
			urls = append(urls, found...)
		}
	}
	return dedupe(urls), nil
}

func findInPage(ctx context.Context, client *httpx.Client, pageURL string) ([]string, error) {
	body, finalURL, err := client.FetchBytes(ctx, pageURL, "")
	if err != nil {
		return nil, err
	}
	text := string(body)
	var out []string
	for _, m := range absRe.FindAllString(text, -1) {
		out = append(out, strings.ReplaceAll(m, `\/`, "/"))
	}
	for _, m := range attrRe.FindAllStringSubmatch(text, -1) {
		if u := resolve(finalURL, m[1]); u != "" {
			out = append(out, u)
		}
	}
	return dedupe(out), nil
}

func resolve(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	return b.ResolveReference(r).String()
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range in {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}
