package curl

import "testing"

func TestParseCopyAsCurlHLS(t *testing.T) {
	cmd := `curl --url 'https://host.example/master.m3u8' \
  -H 'accept: */*' \
  -H 'referer: https://www.example.com/' \
  -H 'sec-ch-ua: "Not=A?Brand";v="99", "Brave";v="151", "Chromium";v="151"' \
  -H 'user-agent: Mozilla/5.0 (X11; Linux x86_64) Chrome/151.0.0.0 Safari/537.36'`

	u, h, err := Parse(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://host.example/master.m3u8" {
		t.Fatalf("url = %q", u)
	}
	// header key is canonicalized; the value (with its inner double quotes from
	// the single-quoted arg) is preserved verbatim.
	if got := h["Sec-Ch-Ua"]; got != `"Not=A?Brand";v="99", "Brave";v="151", "Chromium";v="151"` {
		t.Fatalf("sec-ch-ua = %q", got)
	}
	if h["Referer"] != "https://www.example.com/" {
		t.Fatalf("referer = %q", h["Referer"])
	}
	if h["Accept"] != "*/*" {
		t.Fatalf("accept = %q", h["Accept"])
	}
	if h["User-Agent"] == "" {
		t.Fatal("user-agent lost")
	}
}

func TestParseBareURLAndFolding(t *testing.T) {
	// URL as a bare positional (not --url), plus -A/-e/-b and an ignored flag
	// whose value must not be read as the URL.
	cmd := `curl 'https://host.example/155.mp4?token=abc' --compressed -o out.mp4 ` +
		`-A 'Mozilla/5.0' -e 'https://ref.example/;auto' -b 'sid=xyz; k=v' -X GET`
	u, h, err := Parse(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://host.example/155.mp4?token=abc" {
		t.Fatalf("url = %q", u)
	}
	if h["User-Agent"] != "Mozilla/5.0" {
		t.Fatalf("ua = %q", h["User-Agent"])
	}
	if h["Referer"] != "https://ref.example/" { // ";auto" stripped
		t.Fatalf("referer = %q", h["Referer"])
	}
	if h["Cookie"] != "sid=xyz; k=v" {
		t.Fatalf("cookie = %q", h["Cookie"])
	}
}

func TestParseNoURL(t *testing.T) {
	if _, _, err := Parse(`curl -H 'accept: */*'`); err == nil {
		t.Fatal("want error when no URL present")
	}
}

func TestTokenizeQuotingAndContinuation(t *testing.T) {
	toks, err := tokenize("a 'b c' \"d\\\"e\" \\\n f")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b c", `d"e`, "f"}
	if len(toks) != len(want) {
		t.Fatalf("tokens = %#v", toks)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Fatalf("token %d = %q want %q", i, toks[i], want[i])
		}
	}
}
