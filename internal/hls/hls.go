// Package hls parses M3U8 master and media playlists into the manifest model.
package hls

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mohamed/m314dl/internal/manifest"
)

// IsHLS sniffs playlist content.
func IsHLS(body []byte) bool {
	return strings.HasPrefix(strings.TrimLeft(string(body[:min(64, len(body))]), "\uFEFF \t\r\n"), "#EXTM3U")
}

// parseAttrs parses `KEY=VALUE,KEY="VAL,UE"` attribute lists.
func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	for i := 0; i < len(s); {
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		var val string
		if i < len(s) && s[i] == '"' {
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				val = s[i+1:]
				i = len(s)
			} else {
				val = s[i+1 : i+1+end]
				i += end + 2
			}
			if i < len(s) && s[i] == ',' {
				i++
			}
		} else {
			end := strings.IndexByte(s[i:], ',')
			if end < 0 {
				val = s[i:]
				i = len(s)
			} else {
				val = s[i : i+end]
				i += end + 1
			}
		}
		attrs[strings.ToUpper(key)] = val
	}
	return attrs
}

func resolve(base, ref string) string {
	if ref == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// ParseMaster parses a master playlist. If body is a media playlist, it
// returns a single-stream master with segments already populated.
func ParseMaster(body []byte, baseURL string) (*manifest.Master, error) {
	text := string(body)
	if !strings.Contains(text, "#EXTM3U") {
		return nil, fmt.Errorf("hls: not an M3U8 playlist")
	}
	if !strings.Contains(text, "#EXT-X-STREAM-INF") && !strings.Contains(text, "#EXT-X-MEDIA:") {
		// bare media playlist
		st := &manifest.Stream{Type: manifest.Video, ID: "v1", PlaylistURL: baseURL, SegmentsFull: true}
		if err := ParseMedia(body, baseURL, st); err != nil {
			return nil, err
		}
		return &manifest.Master{URL: baseURL, Live: st.Live, Streams: []*manifest.Stream{st}}, nil
	}

	m := &manifest.Master{URL: baseURL}
	lines := strings.Split(text, "\n")
	var pending *manifest.Stream
	vi, ai, si := 0, 0, 0
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			a := parseAttrs(line[len("#EXT-X-STREAM-INF:"):])
			vi++
			st := &manifest.Stream{Type: manifest.Video, ID: fmt.Sprintf("v%d", vi)}
			// prefer AVERAGE-BANDWIDTH for honest sorting
			if v := a["AVERAGE-BANDWIDTH"]; v != "" {
				st.Bandwidth, _ = strconv.ParseInt(v, 10, 64)
			}
			if st.Bandwidth == 0 {
				st.Bandwidth, _ = strconv.ParseInt(a["BANDWIDTH"], 10, 64)
			}
			if res := a["RESOLUTION"]; res != "" {
				fmt.Sscanf(res, "%dx%d", &st.Width, &st.Height)
			}
			st.FrameRate, _ = strconv.ParseFloat(a["FRAME-RATE"], 64)
			st.Codecs = a["CODECS"]
			st.AudioGroup = a["AUDIO"]
			st.SubsGroup = a["SUBTITLES"]
			// audio-only variant: no resolution, no video codec
			if st.Width == 0 && st.Codecs != "" && !hasVideoCodec(st.Codecs) {
				st.Type = manifest.Audio
			} else if hasAudioCodec(st.Codecs) {
				st.MuxedAudio = true
			}
			pending = st
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			a := parseAttrs(line[len("#EXT-X-MEDIA:"):])
			if a["URI"] == "" || a["TYPE"] == "CLOSED-CAPTIONS" {
				continue
			}
			st := &manifest.Stream{
				PlaylistURL: resolve(baseURL, a["URI"]),
				GroupID:     a["GROUP-ID"],
				Language:    a["LANGUAGE"],
				Name:        a["NAME"],
				Channels:    a["CHANNELS"],
				Default:     a["DEFAULT"] == "YES",
			}
			switch a["TYPE"] {
			case "AUDIO":
				ai++
				st.Type, st.ID = manifest.Audio, fmt.Sprintf("a%d", ai)
			case "SUBTITLES":
				si++
				st.Type, st.ID = manifest.Subtitles, fmt.Sprintf("s%d", si)
			default:
				continue
			}
			m.Streams = append(m.Streams, st)
		case line != "" && !strings.HasPrefix(line, "#"):
			if pending != nil {
				pending.PlaylistURL = resolve(baseURL, line)
				m.Streams = append(m.Streams, pending)
				pending = nil
			}
		}
	}
	return m, nil
}

// ParseMedia parses a media playlist into st.Segments.
func ParseMedia(body []byte, baseURL string, st *manifest.Stream) error {
	text := string(body)
	if !strings.Contains(text, "#EXTM3U") {
		return fmt.Errorf("hls: not an M3U8 playlist")
	}
	st.Segments = st.Segments[:0]
	st.Live = true // until ENDLIST/VOD seen
	var (
		curKey   *manifest.Key
		curInit  *manifest.InitMap
		dur      float64
		disc     bool
		nextByte int64
		haveRng  bool
		rng      manifest.ByteRange
	)
	seq := int64(0)
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			st.TargetDur, _ = strconv.ParseFloat(line[len("#EXT-X-TARGETDURATION:"):], 64)
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			seq, _ = strconv.ParseInt(line[len("#EXT-X-MEDIA-SEQUENCE:"):], 10, 64)
			st.MediaSeq = seq
		case strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:"):
			if strings.TrimSpace(line[len("#EXT-X-PLAYLIST-TYPE:"):]) == "VOD" {
				st.Live = false
			}
		case line == "#EXT-X-ENDLIST":
			st.Live = false
		case line == "#EXT-X-DISCONTINUITY":
			disc = true
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			k, err := parseKey(line[len("#EXT-X-KEY:"):], baseURL)
			if err != nil {
				return err
			}
			curKey = k
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			a := parseAttrs(line[len("#EXT-X-MAP:"):])
			curInit = &manifest.InitMap{URL: resolve(baseURL, a["URI"])}
			if br := a["BYTERANGE"]; br != "" {
				r, _ := parseByteRange(br, 0)
				curInit.Range = &r
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			v := line[len("#EXTINF:"):]
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			dur, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			r, next := parseByteRange(line[len("#EXT-X-BYTERANGE:"):], nextByte)
			rng, haveRng, nextByte = r, true, next
		case line != "" && !strings.HasPrefix(line, "#"):
			seg := manifest.Segment{
				URL:           resolve(baseURL, line),
				Duration:      dur,
				Key:           curKey,
				Init:          curInit,
				Seq:           seq,
				Discontinuity: disc,
			}
			if haveRng {
				r := rng
				seg.Range = &r
			} else {
				nextByte = 0 // byterange chains only across consecutive ranged segments
			}
			st.Segments = append(st.Segments, seg)
			seq++
			dur, disc, haveRng = 0, false, false
		}
	}
	if st.Live && st.TargetDur > 0 {
		st.Refresh = time.Duration(st.TargetDur * float64(time.Second) / 2)
	}
	if st.Init == nil {
		st.Init = curInit
	}
	return nil
}

func parseKey(attrs, baseURL string) (*manifest.Key, error) {
	a := parseAttrs(attrs)
	method := a["METHOD"]
	k := &manifest.Key{}
	switch method {
	case "NONE":
		return nil, nil
	case "AES-128":
		k.Method = manifest.EncAES128
	case "SAMPLE-AES", "SAMPLE-AES-CTR", "SAMPLE-AES-CENC":
		k.Method = manifest.EncSampleAES
	default:
		k.Method = manifest.EncMethod(method)
	}
	// Widevine/PlayReady keyformats mean CENC (fMP4) regardless of METHOD. A
	// FairPlay SAMPLE-AES stream (com.apple.streamingkeydelivery / skd://) is
	// left as SAMPLE-AES: its raw key is user-supplied and it decrypts natively —
	// as cbcs for fMP4, or the transport-stream SAMPLE-AES path for TS.
	switch kf := a["KEYFORMAT"]; {
	case strings.Contains(kf, "urn:uuid:edef8ba9"), // widevine
		strings.Contains(kf, "urn:uuid:9a04f079"): // playready
		k.Method = manifest.EncCENC
	}
	if k.Method != manifest.EncCENC {
		k.URI = resolve(baseURL, a["URI"])
		if strings.HasPrefix(a["URI"], "data:") {
			k.URI = a["URI"]
		}
	}
	if iv := a["IV"]; iv != "" {
		b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(iv, "0x"), "0X"))
		if err != nil {
			return nil, fmt.Errorf("hls: bad key IV %q: %w", iv, err)
		}
		k.IV = b
	}
	return k, nil
}

func hasAudioCodec(codecs string) bool {
	for _, c := range strings.Split(codecs, ",") {
		c = strings.TrimSpace(strings.ToLower(c))
		for _, p := range []string{"mp4a", "ac-3", "ec-3", "ac-4", "opus", "flac", "alac"} {
			if strings.HasPrefix(c, p) {
				return true
			}
		}
	}
	return false
}

func hasVideoCodec(codecs string) bool {
	for _, c := range strings.Split(codecs, ",") {
		c = strings.TrimSpace(strings.ToLower(c))
		for _, p := range []string{"avc", "hvc", "hev", "dvh", "vp09", "vp9", "av01", "mp4v"} {
			if strings.HasPrefix(c, p) {
				return true
			}
		}
	}
	return false
}

// parseByteRange parses "length[@offset]"; without offset it starts at prev.
func parseByteRange(s string, prev int64) (manifest.ByteRange, int64) {
	var length, offset int64
	if i := strings.IndexByte(s, '@'); i >= 0 {
		length, _ = strconv.ParseInt(s[:i], 10, 64)
		offset, _ = strconv.ParseInt(s[i+1:], 10, 64)
	} else {
		length, _ = strconv.ParseInt(s, 10, 64)
		offset = prev
	}
	return manifest.ByteRange{Start: offset, End: offset + length - 1}, offset + length
}
