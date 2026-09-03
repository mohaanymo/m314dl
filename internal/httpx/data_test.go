package httpx

import (
	"context"
	"io"
	"testing"
)

func TestDataURL(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	body, final, err := c.FetchBytes(context.Background(), "data:video/mp4;base64,aGVsbG8=", "")
	if err != nil || string(body) != "hello" || final != "data:video/mp4;base64,aGVsbG8=" {
		t.Fatalf("base64 data URL: body=%q final=%q err=%v", body, final, err)
	}
	// a ranged GET (what the segment worker issues) answers the whole payload
	resp, err := c.RangeGet(context.Background(), "data:,a%20b", "bytes=1-")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "a b" || resp.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("plain data URL: status=%d body=%q type=%q", resp.StatusCode, b, resp.Header.Get("Content-Type"))
	}
	if _, _, err := c.FetchBytes(context.Background(), "data:video/mp4;base64,***", ""); err == nil {
		t.Fatal("bad base64 accepted")
	}
}
