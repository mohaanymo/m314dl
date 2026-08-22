package pick

import (
	"testing"

	"github.com/mohamed/m314dl/internal/manifest"
)

func streams() []*manifest.Stream {
	return []*manifest.Stream{
		{Type: manifest.Video, ID: "v1", Height: 720, Bandwidth: 2_000_000, AudioGroup: "aud"},
		{Type: manifest.Video, ID: "v2", Height: 1080, Bandwidth: 5_000_000, AudioGroup: "aud"},
		{Type: manifest.Audio, ID: "a1", GroupID: "aud", Language: "en", Bandwidth: 128_000},
		{Type: manifest.Audio, ID: "a2", GroupID: "aud", Language: "en", Bandwidth: 64_000},
		{Type: manifest.Audio, ID: "a3", GroupID: "aud", Language: "de", Bandwidth: 128_000},
		{Type: manifest.Audio, ID: "a4", GroupID: "other", Language: "fr", Bandwidth: 128_000},
		{Type: manifest.Subtitles, ID: "s1", Language: "en"},
	}
}

func ids(sts []*manifest.Stream) []string {
	var out []string
	for _, s := range sts {
		out = append(out, s.ID)
	}
	return out
}

func TestDefaultSelection(t *testing.T) {
	ve, _ := ParseExpr("best")
	got := ids(Select(streams(), ve, nil, nil))
	// best video (1080), best audio per language within its group, all subs
	want := []string{"v2", "a1", "a3", "s1"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestFilterTakesAll(t *testing.T) {
	e, err := ParseExpr("lang=en")
	if err != nil {
		t.Fatal(err)
	}
	sts := streams()
	Sort(sts)
	var audio []*manifest.Stream
	for _, s := range sts {
		if s.Type == manifest.Audio {
			audio = append(audio, s)
		}
	}
	got := ids(e.Apply(audio))
	if len(got) != 2 {
		t.Fatalf("lang=en should keep both en audio streams, got %v", got)
	}
}

func TestFilterWithFor(t *testing.T) {
	e, err := ParseExpr("lang=en:for=best")
	if err != nil {
		t.Fatal(err)
	}
	sts := streams()
	Sort(sts)
	var audio []*manifest.Stream
	for _, s := range sts {
		if s.Type == manifest.Audio {
			audio = append(audio, s)
		}
	}
	got := ids(e.Apply(audio))
	if len(got) != 1 || got[0] != "a1" {
		t.Fatalf("got %v", got)
	}
}

func TestMuxedAudioNoDuplicate(t *testing.T) {
	sts := []*manifest.Stream{
		{Type: manifest.Video, ID: "v1", Height: 720, Bandwidth: 2_000_000, MuxedAudio: true},
		{Type: manifest.Audio, ID: "va", Bandwidth: 128_000}, // audio-only variant, no group
	}
	ve, _ := ParseExpr("best")
	got := ids(Select(sts, ve, nil, nil))
	if len(got) != 1 || got[0] != "v1" {
		t.Fatalf("muxed-audio video should not pull audio variant: %v", got)
	}
}

func TestBadExpr(t *testing.T) {
	if _, err := ParseExpr("nonsense-without-equals"); err == nil {
		t.Fatal("want error")
	}
	if _, err := ParseExpr("lang=[unclosed"); err == nil {
		t.Fatal("want regex error")
	}
}

// segList builds a stream with n segments of secs each (for segsMin/plistDur).
func segList(id string, n int, secs float64) *manifest.Stream {
	st := &manifest.Stream{Type: manifest.Video, ID: id, Height: 720, Codecs: "avc1"}
	for i := 0; i < n; i++ {
		st.Segments = append(st.Segments, manifest.Segment{Duration: secs})
	}
	return st
}

func TestSegsMinDropsBumper(t *testing.T) {
	content := segList("content", 75, 4)                                      // 300s
	bumper := segList("bumper", 1, 3)                                         // 3s ad/bumper
	unknown := &manifest.Stream{Type: manifest.Video, ID: "hls", Height: 720} // 0 segs

	e, err := ParseExpr("segsmin=2:for=all")
	if err != nil {
		t.Fatal(err)
	}
	got := e.Apply([]*manifest.Stream{content, bumper, unknown})
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["content"] || ids["bumper"] || !ids["hls"] {
		t.Fatalf("segsmin: want content+hls(unknown passes), not bumper; got %v", ids)
	}
}

func TestPlistDurMin(t *testing.T) {
	e, err := ParseExpr("plistdurmin=1m:for=all") // 60s floor
	if err != nil {
		t.Fatal(err)
	}
	got := e.Apply([]*manifest.Stream{segList("long", 75, 4), segList("short", 1, 3)})
	if len(got) != 1 || got[0].ID != "long" {
		t.Fatalf("plistdurmin should keep only long, got %v", got)
	}
}

func TestNegationDenyList(t *testing.T) {
	e, err := ParseExpr(`id!=^(a|b)$:for=all`)
	if err != nil {
		t.Fatal(err)
	}
	a := &manifest.Stream{Type: manifest.Subtitles, ID: "a"}
	b := &manifest.Stream{Type: manifest.Subtitles, ID: "b"}
	good := &manifest.Stream{Type: manifest.Subtitles, ID: "en"}
	got := e.Apply([]*manifest.Stream{a, b, good})
	if len(got) != 1 || got[0].ID != "en" {
		t.Fatalf("negation should drop a,b keep en; got %v", got)
	}
}
