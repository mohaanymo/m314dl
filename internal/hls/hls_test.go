package hls

import (
	"testing"

	"github.com/mohamed/m314dl/internal/manifest"
)

const master = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud1",NAME="English",LANGUAGE="en",DEFAULT=YES,CHANNELS="2",URI="audio/en.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="sub1",NAME="Deutsch",LANGUAGE="de",URI="subs/de.m3u8"
#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID="cc",NAME="CC1",INSTREAM-ID="CC1"
#EXT-X-STREAM-INF:BANDWIDTH=2000000,AVERAGE-BANDWIDTH=1800000,RESOLUTION=1280x720,FRAME-RATE=29.970,CODECS="avc1.64001f,mp4a.40.2",AUDIO="aud1",SUBTITLES="sub1"
720p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=8000000,RESOLUTION=1920x1080
1080/index.m3u8
`

func TestParseMaster(t *testing.T) {
	m, err := ParseMaster([]byte(master), "https://ex.com/hls/main.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 4 {
		t.Fatalf("streams = %d, want 4", len(m.Streams))
	}
	var v, a, s *manifest.Stream
	for _, st := range m.Streams {
		switch {
		case st.Type == manifest.Video && st.Height == 720:
			v = st
		case st.Type == manifest.Audio:
			a = st
		case st.Type == manifest.Subtitles:
			s = st
		}
	}
	if v == nil || v.Bandwidth != 1800000 || v.AudioGroup != "aud1" || v.SubsGroup != "sub1" {
		t.Fatalf("video variant wrong: %+v", v)
	}
	if v.PlaylistURL != "https://ex.com/hls/720p.m3u8" {
		t.Fatalf("video url = %s", v.PlaylistURL)
	}
	if a == nil || a.Language != "en" || !a.Default || a.GroupID != "aud1" || a.Channels != "2" {
		t.Fatalf("audio wrong: %+v", a)
	}
	if s == nil || s.PlaylistURL != "https://ex.com/hls/subs/de.m3u8" {
		t.Fatalf("subs wrong: %+v", s)
	}
}

const media = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-MAP:URI="init.mp4",BYTERANGE="720@0"
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x00000000000000000000000000000042
#EXTINF:6.000,
seg100.m4s
#EXTINF:6.000,
seg101.m4s
#EXT-X-DISCONTINUITY
#EXT-X-KEY:METHOD=NONE
#EXTINF:4.5,
seg102.m4s
#EXT-X-ENDLIST
`

func TestParseMedia(t *testing.T) {
	st := &manifest.Stream{}
	if err := ParseMedia([]byte(media), "https://ex.com/hls/720p.m3u8", st); err != nil {
		t.Fatal(err)
	}
	if st.Live {
		t.Fatal("should be VOD")
	}
	if len(st.Segments) != 3 {
		t.Fatalf("segments = %d", len(st.Segments))
	}
	s0 := st.Segments[0]
	if s0.Seq != 100 || s0.URL != "https://ex.com/hls/seg100.m4s" {
		t.Fatalf("s0 = %+v", s0)
	}
	if s0.Key == nil || s0.Key.Method != manifest.EncAES128 || s0.Key.IV[15] != 0x42 {
		t.Fatalf("s0 key = %+v", s0.Key)
	}
	if s0.Init == nil || s0.Init.Range.End != 719 {
		t.Fatalf("init = %+v", s0.Init)
	}
	if st.Segments[2].Key != nil {
		t.Fatal("seg102 should be clear after METHOD=NONE")
	}
	if !st.Segments[2].Discontinuity {
		t.Fatal("seg102 should carry discontinuity")
	}
}

const byteRangePl = `#EXTM3U
#EXT-X-TARGETDURATION:10
#EXTINF:10,
#EXT-X-BYTERANGE:1000@0
all.ts
#EXTINF:10,
#EXT-X-BYTERANGE:2000
all.ts
#EXT-X-ENDLIST
`

func TestByteRangeChain(t *testing.T) {
	st := &manifest.Stream{}
	if err := ParseMedia([]byte(byteRangePl), "https://ex.com/a.m3u8", st); err != nil {
		t.Fatal(err)
	}
	r1 := st.Segments[1].Range
	if r1.Start != 1000 || r1.End != 2999 {
		t.Fatalf("chained range = %+v", r1)
	}
}

func TestDRMDetected(t *testing.T) {
	// Widevine/PlayReady keyformats are CENC (fMP4); their key is looked up from
	// the init's tenc, so the manifest URI is not treated as a fetchable key.
	pl := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-KEY:METHOD=SAMPLE-AES,URI="data:text/plain;base64,AAAA",KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"
#EXTINF:6,
s1.ts
#EXT-X-ENDLIST
`
	st := &manifest.Stream{}
	if err := ParseMedia([]byte(pl), "https://ex.com/a.m3u8", st); err != nil {
		t.Fatal(err)
	}
	if st.Segments[0].Key.Method != manifest.EncCENC {
		t.Fatalf("want CENC, got %v", st.Segments[0].Key.Method)
	}
}

// A FairPlay SAMPLE-AES stream stays SAMPLE-AES: its raw key is user-supplied
// and it decrypts natively (cbcs for fMP4, or the TS SAMPLE-AES path for TS).
func TestFairPlaySampleAES(t *testing.T) {
	pl := `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://foo",KEYFORMAT="com.apple.streamingkeydelivery"
#EXTINF:6,
s1.ts
#EXT-X-ENDLIST
`
	st := &manifest.Stream{}
	if err := ParseMedia([]byte(pl), "https://ex.com/a.m3u8", st); err != nil {
		t.Fatal(err)
	}
	if st.Segments[0].Key.Method != manifest.EncSampleAES {
		t.Fatalf("want SAMPLE-AES, got %v", st.Segments[0].Key.Method)
	}
}

func TestBareMediaAsMaster(t *testing.T) {
	m, err := ParseMaster([]byte(media), "https://ex.com/hls/720p.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Streams) != 1 || !m.Streams[0].SegmentsFull || len(m.Streams[0].Segments) != 3 {
		t.Fatalf("bare media wrap failed: %+v", m.Streams[0])
	}
}
