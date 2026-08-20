package restream

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/manifest"
)

func dashPub(live bool) (*Publisher, engine.Sink, engine.Sink) {
	pub := NewPublisher()
	v := &manifest.Stream{Type: manifest.Video, ID: "v", Width: 1920, Height: 1080,
		FrameRate: 25, Codecs: "avc1.640028", Bandwidth: 5_000_000, Init: &manifest.InitMap{URL: "i"}, Live: live}
	a := &manifest.Stream{Type: manifest.Audio, ID: "a", Language: "en", Codecs: "mp4a.40.2",
		Channels: "2", Bandwidth: 128_000, Init: &manifest.InitMap{URL: "i"}, Live: live}
	vs := pub.AddTrack(TrackFromStream("video", v, live))
	as := pub.AddTrack(TrackFromStream("audio-en", a, live))
	vs.Init([]byte("vi"))
	as.Init([]byte("ai"))
	return pub, vs, as
}

func TestDASHManifestVOD(t *testing.T) {
	pub, vs, as := dashPub(false)
	feed(vs, 4, 4.0)
	feed(as, 4, 4.0)
	pub.End()

	m := string(pub.DASHManifest())
	for _, want := range []string{
		`type="static"`,
		"profile:isoff-on-demand",
		`mediaPresentationDuration="PT16.000S"`, // 4 × 4s
		`<AdaptationSet contentType="video" mimeType="video/mp4"`,
		`<AdaptationSet contentType="audio" mimeType="audio/mp4"`,
		`lang="en"`,
		`<Representation id="video" bandwidth="5000000"`,
		`codecs="avc1.640028"`,
		`width="1920" height="1080"`,
		`frameRate="25"`,
		`AudioChannelConfiguration`,
		`value="2"`,
		`media="$RepresentationID$/$Number%06d$.m4s"`,
		`initialization="$RepresentationID$/init.mp4"`,
		`startNumber="0"`,
		`<S t="0" d="4000" r="3"/>`, // 4 equal-duration segments collapse
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("VOD MPD missing %q\n%s", want, m)
		}
	}
	if strings.Contains(m, "minimumUpdatePeriod") {
		t.Fatalf("static MPD must not advertise minimumUpdatePeriod\n%s", m)
	}
}

func TestDASHManifestLive(t *testing.T) {
	pub, vs, as := dashPub(true)
	feed(vs, windowSize+3, 4.0) // force a rolling window
	feed(as, windowSize+3, 4.0)

	m := string(pub.DASHManifest())
	for _, want := range []string{
		`type="dynamic"`,
		"profile:isoff-live",
		"availabilityStartTime=",
		"publishTime=",
		`minimumUpdatePeriod="PT4S"`,
		`timeShiftBufferDepth="PT24S"`, // windowSize(6) × 4s
		`suggestedPresentationDelay="PT12S"`,
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("live MPD missing %q\n%s", want, m)
		}
	}
	// startNumber = seq of first visible segment = total(9) - window(6) = 3,
	// and its timeline anchor @t = 3 × 4000ms.
	if !strings.Contains(m, `startNumber="3"`) {
		t.Fatalf("live startNumber should track the window\n%s", m)
	}
	if !strings.Contains(m, `<S t="12000" d="4000"`) {
		t.Fatalf("live timeline anchor wrong\n%s", m)
	}
	if strings.Contains(m, "mediaPresentationDuration") {
		t.Fatalf("dynamic MPD must not have mediaPresentationDuration\n%s", m)
	}
}

func TestDASHTimelineRunLengthSplits(t *testing.T) {
	pub := NewPublisher()
	vs := pub.AddTrack(TrackFromStream("video", &manifest.Stream{Type: manifest.Video, Init: &manifest.InitMap{URL: "i"}}, false))
	vs.Init([]byte("i"))
	vs.Segment(engine.SegmentInfo{Duration: 4}, []byte("a"))
	vs.Segment(engine.SegmentInfo{Duration: 4}, []byte("b"))
	vs.Segment(engine.SegmentInfo{Duration: 2}, []byte("c")) // shorter → new run
	pub.End()

	m := string(pub.DASHManifest())
	if !strings.Contains(m, `<S t="0" d="4000" r="1"/>`) {
		t.Fatalf("first run (2×4s) should collapse\n%s", m)
	}
	if !strings.Contains(m, `<S t="8000" d="2000"/>`) {
		t.Fatalf("differing-duration segment should start a new S\n%s", m)
	}
}

func TestDASHServeEndpoints(t *testing.T) {
	pub, vs, as := dashPub(true)
	feed(vs, 3, 4.0)
	feed(as, 3, 4.0)

	h := NewServer(pub).DASHHandler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	rec := get("/live.mpd")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != mimeMPD {
		t.Fatalf("mpd: code=%d ctype=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "<MPD") {
		t.Fatalf("mpd body not XML:\n%s", rec.Body)
	}
	if rec := get("/video/init.mp4"); rec.Code != 200 || rec.Body.String() != "vi" {
		t.Fatalf("init: code=%d body=%q", rec.Code, rec.Body)
	}
	if rec := get("/video/000000.m4s"); rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("segment: code=%d ctype=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get("/audio-en/000002.m4s"); rec.Code != 200 {
		t.Fatalf("audio segment: code=%d", rec.Code)
	}
	if rec := get("/nope/000000.m4s"); rec.Code != 404 {
		t.Fatalf("unknown track should 404, got %d", rec.Code)
	}
}
