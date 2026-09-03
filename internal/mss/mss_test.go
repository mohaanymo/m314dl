package mss_test

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/mp4"
	"github.com/mohamed/m314dl/internal/mss"
)

const (
	spsPPS  = "000000016742c00dda05067e7c0440000003004000000c83c50aa80000000168ce3c80"
	baseURL = "https://cdn.example.invalid/vod/movie.ism/Manifest?tok=1"
)

var testKID = [16]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}

// playReadyHeader builds a base64 PlayReady Object carrying a WRMHEADER with
// the given CENC KID (stored GUID-style, little-endian first three fields).
// v4.0 puts the KID in <KID>text</KID>; later versions in <KID VALUE="…">.
func playReadyHeader(kid [16]byte, v41 bool) string {
	g := kid
	g[0], g[1], g[2], g[3] = g[3], g[2], g[1], g[0]
	g[4], g[5] = g[5], g[4]
	g[6], g[7] = g[7], g[6]
	b64 := base64.StdEncoding.EncodeToString(g[:])
	var doc string
	if v41 {
		doc = `<WRMHEADER xmlns="http://schemas.microsoft.com/DRM/2007/03/PlayReadyHeader" version="4.2.0.0"><DATA><PROTECTINFO><KIDS><KID ALGID="AESCTR" CHECKSUM="AAAAAAAAAAA=" VALUE="` + b64 + `"></KID></KIDS></PROTECTINFO><LA_URL>https://license.example.invalid/</LA_URL></DATA></WRMHEADER>`
	} else {
		doc = `<WRMHEADER xmlns="http://schemas.microsoft.com/DRM/2007/03/PlayReadyHeader" version="4.0.0.0"><DATA><PROTECTINFO><KEYLEN>16</KEYLEN><ALGID>AESCTR</ALGID></PROTECTINFO><KID>` + b64 + `</KID><LA_URL>https://license.example.invalid/</LA_URL></DATA></WRMHEADER>`
	}
	u := utf16.Encode([]rune(doc))
	data := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(data[2*i:], c)
	}
	rec := make([]byte, 4)
	binary.LittleEndian.PutUint16(rec, 1) // rights management header
	binary.LittleEndian.PutUint16(rec[2:], uint16(len(data)))
	rec = append(rec, data...)
	pro := make([]byte, 6)
	binary.LittleEndian.PutUint32(pro, uint32(6+len(rec)))
	binary.LittleEndian.PutUint16(pro[4:], 1)
	pro = append(pro, rec...)
	return base64.StdEncoding.EncodeToString(pro)
}

func manifestXML(protection string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<SmoothStreamingMedia MajorVersion="2" MinorVersion="2" Duration="80000000" TimeScale="10000000">
` + protection + `
<StreamIndex Type="video" Name="video" Chunks="5" QualityLevels="2" MaxWidth="640" MaxHeight="360" Url="QualityLevels({bitrate})/Fragments(video={start time})">
  <QualityLevel Index="0" Bitrate="1000000" FourCC="H264" MaxWidth="640" MaxHeight="360" CodecPrivateData="` + spsPPS + `" />
  <QualityLevel Index="1" Bitrate="300000" FourCC="H264" MaxWidth="320" MaxHeight="180" CodecPrivateData="` + spsPPS + `" />
  <c d="20000000" r="2" />
  <c t="50000000" d="10000000" />
  <c />
</StreamIndex>
<StreamIndex Type="audio" Name="audio_eng" Language="eng" Chunks="2" QualityLevels="2" Url="QualityLevels({Bitrate})/Fragments(audio_eng={start_time})">
  <QualityLevel Index="0" Bitrate="128000" FourCC="AACL" SamplingRate="48000" Channels="2" BitsPerSample="16" AudioTag="255" CodecPrivateData="1190" />
  <QualityLevel Index="1" Bitrate="64000" FourCC="AACH" SamplingRate="24000" Channels="2" BitsPerSample="16" AudioTag="255" CodecPrivateData="" />
  <c t="0" d="40000000" />
  <c d="40000000" />
</StreamIndex>
<StreamIndex Type="text" Name="subs" Language="fra" Subtype="SUBT" Chunks="1" QualityLevels="1" Url="QualityLevels({bitrate})/Fragments(subs={start time})">
  <QualityLevel Index="0" Bitrate="1000" FourCC="TTML" />
  <c t="0" d="80000000" />
</StreamIndex>
<StreamIndex Type="video" Name="hevc" Chunks="1" QualityLevels="1" Url="QualityLevels({bitrate})/Fragments(hevc={start time})">
  <QualityLevel Index="0" Bitrate="5000000" FourCC="HEV1" CodecPrivateData="00" />
  <c t="0" d="80000000" />
</StreamIndex>
</SmoothStreamingMedia>`
}

func byID(t *testing.T, m *manifest.Master, id string) *manifest.Stream {
	t.Helper()
	for _, st := range m.Streams {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("no stream %q", id)
	return nil
}

func decodeInit(t *testing.T, st *manifest.Stream) []byte {
	t.Helper()
	const prefix = "data:video/mp4;base64,"
	if st.Init == nil || !strings.HasPrefix(st.Init.URL, prefix) {
		t.Fatalf("stream %s has no data: init", st.ID)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(st.Init.URL, prefix))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIsMSS(t *testing.T) {
	if !mss.IsMSS([]byte(manifestXML(""))) {
		t.Fatal("manifest not detected")
	}
	if mss.IsMSS([]byte(`<?xml version="1.0"?><MPD type="static"></MPD>`)) || mss.IsMSS([]byte("#EXTM3U")) {
		t.Fatal("false positive")
	}
}

func TestParseClear(t *testing.T) {
	m, err := mss.Parse([]byte(manifestXML("")), baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if m.Live || m.URL != baseURL {
		t.Fatalf("master: live=%v url=%q", m.Live, m.URL)
	}
	// 2 video + 2 audio + 1 text; the HEVC level is dropped (no init synthesis)
	if len(m.Streams) != 5 {
		for _, st := range m.Streams {
			t.Logf("  %s %s", st.ID, st)
		}
		t.Fatalf("%d streams, want 5", len(m.Streams))
	}

	v := byID(t, m, "video.0.0")
	if v.Type != manifest.Video || v.Width != 640 || v.Height != 360 || v.Bandwidth != 1000000 || v.Codecs != "avc1.42c00d" {
		t.Fatalf("video stream: %+v", v)
	}
	// timeline: r=2 → two 2s chunks at 0 and 2s; explicit t=5s (a gap); then a
	// chunk with no d that runs to the 8s presentation end
	wantURLs := []string{
		"https://cdn.example.invalid/vod/movie.ism/QualityLevels(1000000)/Fragments(video=0)",
		"https://cdn.example.invalid/vod/movie.ism/QualityLevels(1000000)/Fragments(video=20000000)",
		"https://cdn.example.invalid/vod/movie.ism/QualityLevels(1000000)/Fragments(video=50000000)",
		"https://cdn.example.invalid/vod/movie.ism/QualityLevels(1000000)/Fragments(video=60000000)",
	}
	wantDur := []float64{2, 2, 1, 2}
	if len(v.Segments) != len(wantURLs) {
		t.Fatalf("%d video segments, want %d", len(v.Segments), len(wantURLs))
	}
	for i, seg := range v.Segments {
		if seg.URL != wantURLs[i] || seg.Duration != wantDur[i] || seg.Seq != int64(i) || seg.Key != nil {
			t.Fatalf("segment %d: %+v", i, seg)
		}
	}
	if v.TargetDur != 2 || !v.SegmentsFull || v.PlaylistURL != baseURL {
		t.Fatalf("video meta: target=%v full=%v plist=%q", v.TargetDur, v.SegmentsFull, v.PlaylistURL)
	}
	init := decodeInit(t, v)
	if info, err := mp4.ParseInit(init); err != nil || info != nil {
		t.Fatalf("clear init parsed as protected: %v %v", info, err)
	}
	if mp4.SoleTrackID(init) != 1 {
		t.Fatal("init track id")
	}
	if v2 := byID(t, m, "video.0.1"); v2.Width != 320 || v2.Bandwidth != 300000 {
		t.Fatalf("second quality level: %+v", v2)
	}

	a := byID(t, m, "audio_eng.1.0")
	if a.Type != manifest.Audio || a.Language != "eng" || a.Channels != "2" || a.Codecs != "mp4a.40.2" || a.Name != "audio_eng" {
		t.Fatalf("audio stream: %+v", a)
	}
	if len(a.Segments) != 2 || a.Segments[1].URL != "https://cdn.example.invalid/vod/movie.ism/QualityLevels(128000)/Fragments(audio_eng=40000000)" || a.Segments[1].Duration != 4 {
		t.Fatalf("audio segments: %+v", a.Segments)
	}
	decodeInit(t, a)
	// HE-AAC with no CodecPrivateData: ASC derived from SamplingRate/Channels
	if he := byID(t, m, "audio_eng.1.1"); he.Codecs != "mp4a.40.5" || he.Init == nil {
		t.Fatalf("HE-AAC stream: %+v", he)
	}

	s := byID(t, m, "subs.2.0")
	if s.Type != manifest.Subtitles || s.Init != nil || s.Language != "fra" || s.Codecs != "ttml" || len(s.Segments) != 1 {
		t.Fatalf("text stream: %+v", s)
	}
}

func TestParseProtected(t *testing.T) {
	for _, v41 := range []bool{false, true} {
		prot := `<Protection><ProtectionHeader SystemID="9A04F079-9840-4286-AB92-E65BE0885F95">` +
			playReadyHeader(testKID, v41) + `</ProtectionHeader></Protection>`
		m, err := mss.Parse([]byte(manifestXML(prot)), baseURL)
		if err != nil {
			t.Fatal(err)
		}
		v := byID(t, m, "video.0.0")
		for _, seg := range v.Segments {
			if seg.Key == nil || seg.Key.Method != manifest.EncCENC || seg.Key.KID != testKID {
				t.Fatalf("v41=%v: segment key %+v", v41, seg.Key)
			}
		}
		// the synthesized init carries the tenc the decryptor reads
		info, err := mp4.ParseInit(decodeInit(t, v))
		if err != nil || info == nil || info.DefaultKID != testKID || info.Scheme != mp4.SchemeCENC || info.PerSampleIVLen != 8 {
			t.Fatalf("v41=%v: init protection %+v err=%v", v41, info, err)
		}
		if a := byID(t, m, "audio_eng.1.0"); a.Segments[0].Key == nil {
			t.Fatal("audio not marked encrypted")
		}
		// text is never encrypted
		if s := byID(t, m, "subs.2.0"); s.Segments[0].Key != nil {
			t.Fatal("text marked encrypted")
		}
	}
	// a Protection element whose header yields no KID: still CENC, zero KID
	// (the -key value or a bare key then stands in)
	m, err := mss.Parse([]byte(manifestXML(`<Protection><ProtectionHeader SystemID="EDEF8BA9-79D6-4ACE-A3C8-27DCD51D21ED">AAAAAA==</ProtectionHeader></Protection>`)), baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if k := byID(t, m, "video.0.0").Segments[0].Key; k == nil || k.Method != manifest.EncCENC || k.KID != ([16]byte{}) {
		t.Fatalf("unknown system: key %+v", k)
	}
}

func TestParseLive(t *testing.T) {
	body := `<SmoothStreamingMedia MajorVersion="2" MinorVersion="0" Duration="0" IsLive="TRUE" LookAheadFragmentCount="2" DVRWindowLength="600000000" TimeScale="10000000">
<StreamIndex Type="audio" Name="audio" TimeScale="48000" Chunks="3" QualityLevels="1" Url="QualityLevels({bitrate})/Fragments(audio={start time})">
  <QualityLevel Index="0" Bitrate="96000" FourCC="AACL" SamplingRate="48000" Channels="2" AudioTag="255" CodecPrivateData="1190" />
  <c t="4800000" d="96000" r="3" />
</StreamIndex>
</SmoothStreamingMedia>`
	m, err := mss.Parse([]byte(body), "http://live.example.invalid/ch.isml/Manifest")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Live || len(m.Streams) != 1 {
		t.Fatalf("live master: %+v", m)
	}
	a := m.Streams[0]
	if !a.Live || a.Refresh != 2*time.Second || len(a.Segments) != 3 {
		t.Fatalf("live stream: live=%v refresh=%v segs=%d", a.Live, a.Refresh, len(a.Segments))
	}
	// per-index timescale (48000): 96000 ticks = 2s, start 4800000 = 100s
	if s := a.Segments[2]; s.Duration != 2 || s.URL != "http://live.example.invalid/ch.isml/QualityLevels(96000)/Fragments(audio=4992000)" {
		t.Fatalf("live segment: %+v", s)
	}
}

func TestParseUnsupportedOnly(t *testing.T) {
	body := `<SmoothStreamingMedia MajorVersion="2" MinorVersion="0" Duration="10000000">
<StreamIndex Type="audio" Chunks="1" QualityLevels="1" Url="QualityLevels({bitrate})/Fragments(audio={start time})">
  <QualityLevel Index="0" Bitrate="96000" FourCC="WMAP" SamplingRate="48000" Channels="2" AudioTag="354" CodecPrivateData="1000" />
  <c t="0" d="10000000" />
</StreamIndex>
</SmoothStreamingMedia>`
	if _, err := mss.Parse([]byte(body), baseURL); err == nil || !strings.Contains(err.Error(), "WMAP") {
		t.Fatalf("expected an unsupported-FourCC error, got %v", err)
	}
}
