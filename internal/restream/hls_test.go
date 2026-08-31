package restream

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/manifest"
)

func feed(sink engine.Sink, n int, dur float64) {
	for i := 0; i < n; i++ {
		sink.Segment(engine.SegmentInfo{Duration: dur, Seq: int64(i)}, []byte(fmt.Sprintf("seg-%d", i)))
	}
}

func TestMediaPlaylistFMP4(t *testing.T) {
	pub := NewPublisher()
	st := &manifest.Stream{Type: manifest.Video, ID: "v", Init: &manifest.InitMap{URL: "init.mp4"}}
	sink := pub.AddTrack(TrackFromStream("video", st, true))
	sink.Init([]byte("INIT"))
	feed(sink, 3, 6.0)

	pl := string(pub.tracks[0].mediaPlaylist())
	for _, want := range []string{
		"#EXT-X-VERSION:6", // fMP4 requires v6 (worker shipped v3 here — bug)
		"#EXT-X-TARGETDURATION:6",
		"#EXT-X-MEDIA-SEQUENCE:0",
		"#EXT-X-MAP:URI=\"init.mp4\"",
		"#EXTINF:6.000,\n000000.m4s",
		"000002.m4s",
	} {
		if !strings.Contains(pl, want) {
			t.Fatalf("media playlist missing %q\n%s", want, pl)
		}
	}
	if strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Fatal("live playlist must not have ENDLIST")
	}
}

func TestMediaPlaylistTS(t *testing.T) {
	pub := NewPublisher()
	st := &manifest.Stream{Type: manifest.Video, ID: "v",
		Segments: []manifest.Segment{{URL: "http://x/0.ts"}}}
	sink := pub.AddTrack(TrackFromStream("video", st, true))
	feed(sink, 2, 4.0)

	pl := string(pub.tracks[0].mediaPlaylist())
	if !strings.Contains(pl, "#EXT-X-VERSION:3") {
		t.Fatalf("TS output should be v3\n%s", pl)
	}
	if strings.Contains(pl, "EXT-X-MAP") {
		t.Fatalf("TS output must not emit EXT-X-MAP\n%s", pl)
	}
	if !strings.Contains(pl, "000000.ts") {
		t.Fatalf("TS segment name wrong\n%s", pl)
	}
}

// The window keeps the newest windowSize segments; MEDIA-SEQUENCE advances
// monotonically as older ones age out.
func TestWindowAndSequence(t *testing.T) {
	pub := NewPublisher()
	st := &manifest.Stream{Type: manifest.Video, ID: "v", Init: &manifest.InitMap{URL: "i"}}
	sink := pub.AddTrack(TrackFromStream("video", st, true))
	sink.Init([]byte("INIT"))
	feed(sink, windowSize+10, 2.0)

	tr := pub.tracks[0]
	pl := string(tr.mediaPlaylist())
	lines := 0
	for _, l := range strings.Split(pl, "\n") {
		if strings.HasSuffix(l, ".m4s") {
			lines++
		}
	}
	if lines != windowSize {
		t.Fatalf("visible segments = %d, want %d", lines, windowSize)
	}
	// oldest visible = total(16) - window(6) = seq 10
	if !strings.Contains(pl, fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d", windowSize+10-windowSize)) {
		t.Fatalf("media-sequence wrong\n%s", pl)
	}
}

// A source that rewinds its media sequence must not rewind or wedge output —
// the packager renumbers under its own monotonic counter (worker bug #18).
func TestSourceSequenceRewindIgnored(t *testing.T) {
	pub := NewPublisher()
	sink := pub.AddTrack(TrackFromStream("video", &manifest.Stream{Type: manifest.Video, Init: &manifest.InitMap{URL: "i"}}, true))
	sink.Init([]byte("i"))
	sink.Segment(engine.SegmentInfo{Duration: 2, Seq: 5000}, []byte("a"))
	sink.Segment(engine.SegmentInfo{Duration: 2, Seq: 3}, []byte("b")) // source rewound
	pl := string(pub.tracks[0].mediaPlaylist())
	if !strings.Contains(pl, "000000.m4s") || !strings.Contains(pl, "000001.m4s") {
		t.Fatalf("output sequence not monotonic under source rewind\n%s", pl)
	}
}

func TestDiscontinuityPassthrough(t *testing.T) {
	pub := NewPublisher()
	sink := pub.AddTrack(TrackFromStream("video", &manifest.Stream{Type: manifest.Video, Init: &manifest.InitMap{URL: "i"}}, true))
	sink.Init([]byte("i"))
	sink.Segment(engine.SegmentInfo{Duration: 2}, []byte("a"))
	sink.Segment(engine.SegmentInfo{Duration: 2, Discontinuity: true}, []byte("b"))
	pl := string(pub.tracks[0].mediaPlaylist())
	i := strings.Index(pl, "#EXT-X-DISCONTINUITY")
	j := strings.Index(pl, "000001.m4s")
	if i < 0 || i > j {
		t.Fatalf("discontinuity tag missing or misplaced\n%s", pl)
	}
}

func TestZeroDurationFallback(t *testing.T) {
	pub := NewPublisher()
	sink := pub.AddTrack(TrackFromStream("video", &manifest.Stream{Type: manifest.Video, Init: &manifest.InitMap{URL: "i"}}, true))
	sink.Init([]byte("i"))
	sink.Segment(engine.SegmentInfo{Duration: 0}, []byte("a")) // unknown → 2s default
	pl := string(pub.tracks[0].mediaPlaylist())
	if !strings.Contains(pl, "#EXTINF:2.000,") {
		t.Fatalf("zero duration should fall back to 2s\n%s", pl)
	}
}

func TestMasterPlaylist(t *testing.T) {
	pub := NewPublisher()
	v := &manifest.Stream{Type: manifest.Video, ID: "v", Width: 1920, Height: 1080,
		FrameRate: 25, Codecs: "avc1.640028", Bandwidth: 5_000_000, Init: &manifest.InitMap{URL: "i"}}
	a := &manifest.Stream{Type: manifest.Audio, ID: "a", Language: "en", Codecs: "mp4a.40.2",
		Channels: "2", Bandwidth: 128_000, Init: &manifest.InitMap{URL: "i"}}
	vs := pub.AddTrack(TrackFromStream("video", v, true))
	as := pub.AddTrack(TrackFromStream("audio-en", a, true))
	vs.Init([]byte("i"))
	as.Init([]byte("i"))
	feed(vs, 2, 6)
	feed(as, 2, 6)

	m := string(pub.masterPlaylist())
	for _, want := range []string{
		"#EXT-X-VERSION:6",
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\"",
		"LANGUAGE=\"en\"",
		"CHANNELS=\"2\"",
		"URI=\"audio-en/index.m3u8\"",
		"#EXT-X-STREAM-INF:",
		"RESOLUTION=1920x1080",
		"AUDIO=\"aud\"",
		"video/index.m3u8",
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("master playlist missing %q\n%s", want, m)
		}
	}
	// BANDWIDTH must include audio: 5_000_000 + 128_000
	if !strings.Contains(m, "BANDWIDTH=5128000") {
		t.Fatalf("variant bandwidth should sum video+audio\n%s", m)
	}
	// CODECS must list both video and audio
	if !strings.Contains(m, "avc1.640028,mp4a.40.2") {
		t.Fatalf("variant codecs should list video+audio\n%s", m)
	}
}

// Writers (one per track) publish while HTTP handlers read playlists and
// segments concurrently. Run under -race to prove the locking holds.
func TestConcurrentServeAndWrite(t *testing.T) {
	pub := NewPublisher()
	vs := pub.AddTrack(TrackFromStream("video", &manifest.Stream{Type: manifest.Video, Init: &manifest.InitMap{URL: "i"}}, true))
	as := pub.AddTrack(TrackFromStream("audio", &manifest.Stream{Type: manifest.Audio, Init: &manifest.InitMap{URL: "i"}}, true))
	vs.Init([]byte("vi"))
	as.Init([]byte("ai"))
	h := NewServer(pub).Handler()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			vs.Segment(engine.SegmentInfo{Duration: 2}, []byte("v"))
			as.Segment(engine.SegmentInfo{Duration: 2}, []byte("a"))
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			for _, p := range []string{"/live.m3u8", "/video/index.m3u8", "/audio/index.m3u8", "/video/000010.m4s"} {
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
			}
		}
	}
}

// A finite (VOD) source keeps every segment (no rolling window) and, once
// ended, publishes a complete seekable playlist with EXT-X-ENDLIST.
func TestVODKeepsAllSegments(t *testing.T) {
	pub := NewPublisher()
	st := &manifest.Stream{Type: manifest.Video, ID: "v", Init: &manifest.InitMap{URL: "i"}}
	sink := pub.AddTrack(TrackFromStream("video", st, false)) // VOD
	sink.Init([]byte("i"))
	feed(sink, windowSize+10, 3.0)
	pub.End()

	pl := string(pub.tracks[0].mediaPlaylist())
	segs := strings.Count(pl, ".m4s\n")
	if segs != windowSize+10 {
		t.Fatalf("VOD should list all %d segments, listed %d\n%s", windowSize+10, segs, pl)
	}
	if !strings.Contains(pl, "#EXT-X-MEDIA-SEQUENCE:0") {
		t.Fatalf("VOD media-sequence should start at 0\n%s", pl)
	}
	if !strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Fatalf("ended VOD playlist must have ENDLIST\n%s", pl)
	}
}

func TestServeEndpoints(t *testing.T) {
	pub := NewPublisher()
	st := &manifest.Stream{Type: manifest.Video, ID: "v", Init: &manifest.InitMap{URL: "i"}}
	sink := pub.AddTrack(TrackFromStream("video", st, true))
	sink.Init([]byte("INITDATA"))
	feed(sink, windowSize+tailExtra+5, 4.0) // force eviction

	h := NewServer(pub).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	if rec := get("/live.m3u8"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "video/index.m3u8") {
		t.Fatalf("master: code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := get("/video/index.m3u8"); rec.Code != 200 || rec.Header().Get("Content-Type") != mimeM3U8 {
		t.Fatalf("media: code=%d ctype=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get("/video/init.mp4"); rec.Code != 200 || rec.Body.String() != "INITDATA" {
		t.Fatalf("init: code=%d body=%q", rec.Code, rec.Body)
	}
	if rec := get("/video/000000.m4s"); rec.Code != 404 {
		t.Fatalf("evicted segment should 404, got %d", rec.Code)
	}
	// a currently-visible segment serves with the right type + CORS
	rec := get("/video/000013.m4s")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("visible segment: code=%d ctype=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("segment missing CORS header")
	}
	if rec := get("/nope/index.m3u8"); rec.Code != 404 {
		t.Fatalf("unknown track should 404, got %d", rec.Code)
	}
}

// A VOD track keeps the full playlist only until maxWindowBytes; past it the
// oldest segments are evicted (sliding DVR window) so a long asset can't OOM.
func TestTrackVODWindowByteCap(t *testing.T) {
	old := maxWindowBytes
	maxWindowBytes = 1000
	defer func() { maxWindowBytes = old }()

	st := &manifest.Stream{ID: "v", Type: manifest.Video, Segments: []manifest.Segment{{URL: "x.ts"}}}
	tr := TrackFromStream("video", st, false) // false = VOD (keep-all, until the cap)
	for i := 0; i < 30; i++ {
		if err := tr.Segment(engine.SegmentInfo{Duration: 2}, make([]byte, 200)); err != nil {
			t.Fatal(err)
		}
	}
	tr.mu.RLock()
	defer tr.mu.RUnlock()

	if len(tr.segs) >= 30 {
		t.Fatalf("VOD window never trimmed: %d segments held", len(tr.segs))
	}
	if len(tr.segs) > windowSize && tr.winBytes > maxWindowBytes {
		t.Fatalf("window over budget: %d bytes in %d segments, cap %d", tr.winBytes, len(tr.segs), maxWindowBytes)
	}
	if tr.segs[0].seq == 0 {
		t.Fatal("oldest segment not evicted: first seq still 0")
	}
	var sum int64
	for _, s := range tr.segs {
		sum += int64(len(s.data))
	}
	if sum != tr.winBytes {
		t.Fatalf("winBytes bookkeeping off: tracked %d, actual %d", tr.winBytes, sum)
	}
}
