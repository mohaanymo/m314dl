// Package curl parses a copy-as-curl command line into a URL and headers, so a
// user can paste a browser's "Copy as cURL" instead of hand-typing every -H.
package curl

import (
	"fmt"
	"net/http"
	"strings"
)

// Parse extracts the request URL and headers from a curl command. It reads the
// URL (`--url X`, `--url=X`, or the first bare http(s) argument) and every
// header (`-H`/`--header`), and folds `-A`/`--user-agent`, `-e`/`--referer`, and
// `-b`/`--cookie` into their header equivalents. Flags it doesn't use are
// ignored; the ones that take a value are skipped so their value can't be
// mistaken for the URL. Header keys are canonicalized so a later -H on the CLI
// overrides cleanly.
func Parse(cmd string) (rawURL string, headers map[string]string, err error) {
	toks, err := tokenize(cmd)
	if err != nil {
		return "", nil, err
	}
	headers = map[string]string{}
	set := func(k, v string) {
		if v != "" {
			headers[http.CanonicalHeaderKey(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	addHeader := func(h string) {
		if k, v, ok := strings.Cut(h, ":"); ok {
			set(k, v)
		}
	}
	// next returns the following token (a flag's value) and advances i.
	i := 0
	next := func() string {
		if i+1 < len(toks) {
			i++
			return toks[i]
		}
		return ""
	}
	// valueFlags take an argument we skip past (so it isn't read as the URL).
	valueFlags := map[string]bool{
		"-X": true, "--request": true, "-d": true, "--data": true,
		"--data-raw": true, "--data-binary": true, "--data-ascii": true,
		"--data-urlencode": true, "-x": true, "--proxy": true, "-o": true,
		"--output": true, "--connect-timeout": true, "-m": true, "--max-time": true,
		"-w": true, "--write-out": true, "--retry": true, "-T": true, "--upload-file": true,
	}
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t == "curl":
		case t == "--url":
			rawURL = next()
		case strings.HasPrefix(t, "--url="):
			rawURL = strings.TrimPrefix(t, "--url=")
		case t == "-H" || t == "--header":
			addHeader(next())
		case strings.HasPrefix(t, "--header="):
			addHeader(strings.TrimPrefix(t, "--header="))
		case t == "-A" || t == "--user-agent":
			set("User-Agent", next())
		case strings.HasPrefix(t, "--user-agent="):
			set("User-Agent", strings.TrimPrefix(t, "--user-agent="))
		case t == "-e" || t == "--referer":
			ref, _, _ := strings.Cut(next(), ";") // curl allows "url;auto"
			set("Referer", ref)
		case t == "-b" || t == "--cookie":
			if c := next(); strings.Contains(c, "=") { // a cookie string, not a file
				set("Cookie", c)
			}
		case valueFlags[t]:
			next() // consume and drop the value
		case strings.HasPrefix(t, "-"):
			// An unused flag (--compressed, -sSL, -k, …); ignore, keep its
			// neighbours. Bundled short flags never carry the URL.
		default:
			if rawURL == "" && looksLikeURL(t) {
				rawURL = t
			}
		}
	}
	if rawURL == "" {
		return "", nil, fmt.Errorf("no http(s) URL found in curl command")
	}
	return rawURL, headers, nil
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// tokenize splits a shell-style command line, honoring single quotes (literal),
// double quotes (backslash escapes the next byte), backslash escapes outside
// quotes, and a trailing "\<newline>" line continuation.
func tokenize(s string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	inTok := false
	flush := func() {
		if inTok {
			toks = append(toks, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '\'':
			inTok = true
			j := i + 1
			for j < len(s) && s[j] != '\'' {
				cur.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated single quote")
			}
			i = j + 1
		case '"':
			inTok = true
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				cur.WriteByte(s[j])
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			i = j + 1
		case '\\':
			if i+1 < len(s) {
				if s[i+1] == '\n' { // line continuation
					i += 2
					continue
				}
				cur.WriteByte(s[i+1])
				inTok = true
				i += 2
			} else {
				i++
			}
		case ' ', '\t', '\n', '\r':
			flush()
			i++
		default:
			cur.WriteByte(c)
			inTok = true
			i++
		}
	}
	flush()
	return toks, nil
}
