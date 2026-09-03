// Package mss parses Smooth Streaming manifests (SmoothStreamingMedia XML)
// into the manifest model. A Smooth presentation has no init segments: the
// codec setup lives in each QualityLevel's CodecPrivateData, so Parse
// synthesizes an fMP4 init per track and hands it to the engine as a data: URL,
// which the shared HTTP client serves like any other init fetch.
package mss

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/mp4"
)

// IsMSS reports whether body is a Smooth Streaming manifest.
func IsMSS(body []byte) bool {
	return bytes.Contains(body[:min(1024, len(body))], []byte("<SmoothStreamingMedia"))
}

const defaultTimescale = 10000000 // 100 ns ticks, the Smooth default

// ---- XML model ----

type ssmXML struct {
	Duration      uint64     `xml:"Duration,attr"`
	TimeScale     uint64     `xml:"TimeScale,attr"`
	IsLive        string     `xml:"IsLive,attr"`
	Protection    *protXML   `xml:"Protection"`
	StreamIndexes []indexXML `xml:"StreamIndex"`
}

type protXML struct {
	Headers []protHeaderXML `xml:"ProtectionHeader"`
}

type protHeaderXML struct {
	SystemID string `xml:"SystemID,attr"`
	Data     string `xml:",chardata"`
}

type indexXML struct {
	Type          string  `xml:"Type,attr"`
	Name          string  `xml:"Name,attr"`
	Subtype       string  `xml:"Subtype,attr"`
	URL           string  `xml:"Url,attr"`
	Language      string  `xml:"Language,attr"`
	TimeScale     uint64  `xml:"TimeScale,attr"`
	MaxWidth      int     `xml:"MaxWidth,attr"`
	MaxHeight     int     `xml:"MaxHeight,attr"`
	DisplayWidth  int     `xml:"DisplayWidth,attr"`
	DisplayHeight int     `xml:"DisplayHeight,attr"`
	QualityLevels []qlXML `xml:"QualityLevel"`
	Chunks        []cXML  `xml:"c"`
}

type qlXML struct {
	Index            int    `xml:"Index,attr"`
	Bitrate          int64  `xml:"Bitrate,attr"`
	MaxWidth         int    `xml:"MaxWidth,attr"`
	MaxHeight        int    `xml:"MaxHeight,attr"`
	Width            int    `xml:"Width,attr"`
	Height           int    `xml:"Height,attr"`
	FourCC           string `xml:"FourCC,attr"`
	CodecPrivateData string `xml:"CodecPrivateData,attr"`
	SamplingRate     int    `xml:"SamplingRate,attr"`
	Channels         int    `xml:"Channels,attr"`
	AudioTag         int    `xml:"AudioTag,attr"`
	NALUnitLength    int    `xml:"NALUnitLengthField,attr"`
}

type cXML struct {
	T *uint64 `xml:"t,attr"`
	D *uint64 `xml:"d,attr"`
	R int     `xml:"r,attr"`
}

// ---- parsing ----

// Parse converts a Smooth Streaming manifest into the unified model. Every
// (StreamIndex × QualityLevel) becomes one Stream with its segments populated
// and a synthesized init.
func Parse(body []byte, manifestURL string) (*manifest.Master, error) {
	var doc ssmXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("mss: parse manifest: %w", err)
	}
	live := strings.EqualFold(doc.IsLive, "true")
	m := &manifest.Master{URL: manifestURL, Live: live}
	docTS := doc.TimeScale
	if docTS == 0 {
		docTS = defaultTimescale
	}

	var key *manifest.Key
	if doc.Protection != nil {
		key = &manifest.Key{Method: manifest.EncCENC, KID: playReadyKID(doc.Protection.Headers)}
	}

	var unsupported []string
	for si, idx := range doc.StreamIndexes {
		ts := idx.TimeScale
		if ts == 0 {
			ts = docTS
		}
		typ := mediaType(idx.Type)
		chunks := timeline(idx.Chunks, doc.Duration*ts/docTS)
		for qi, ql := range idx.QualityLevels {
			st := &manifest.Stream{
				Type:        typ,
				ID:          fmt.Sprintf("%s.%d.%d", firstNonEmpty(idx.Name, idx.Type), si, qi),
				PlaylistURL: manifestURL,
				Bandwidth:   ql.Bitrate,
				Width:       firstNonZero(ql.MaxWidth, ql.Width, idx.MaxWidth, idx.DisplayWidth),
				Height:      firstNonZero(ql.MaxHeight, ql.Height, idx.MaxHeight, idx.DisplayHeight),
				Language:    idx.Language,
				Name:        idx.Name,
				Live:        live,
			}
			if ql.Channels > 0 {
				st.Channels = strconv.Itoa(ql.Channels)
			}
			if typ != manifest.Subtitles {
				init, codecs, err := synthesizeInit(&ql, st, uint32(ts), key)
				if err != nil {
					unsupported = append(unsupported, fmt.Sprintf("%s (%v)", st.ID, err))
					continue
				}
				st.Init = &manifest.InitMap{URL: "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(init)}
				st.Codecs = codecs
			} else {
				st.Codecs = strings.ToLower(ql.FourCC)
			}
			for i, c := range chunks {
				seg := manifest.Segment{
					URL:      resolveURL(manifestURL, expandTemplate(idx.URL, ql.Bitrate, c.t)),
					Duration: float64(c.d) / float64(ts),
					Seq:      int64(i),
				}
				if key != nil && typ != manifest.Subtitles {
					seg.Key = key
				}
				if seg.Duration > st.TargetDur {
					st.TargetDur = seg.Duration
				}
				st.Segments = append(st.Segments, seg)
			}
			st.SegmentsFull = true
			if live {
				st.Refresh = time.Duration(max(st.TargetDur, 2) * float64(time.Second))
			}
			m.Streams = append(m.Streams, st)
		}
	}
	if len(m.Streams) == 0 {
		if len(unsupported) > 0 {
			return nil, fmt.Errorf("mss: no supported tracks: %s", strings.Join(unsupported, "; "))
		}
		return nil, fmt.Errorf("mss: manifest has no streams")
	}
	return m, nil
}

type chunk struct{ t, d uint64 }

// timeline expands the <c t d r> list: t defaults to the previous chunk's end,
// r is the total number of chunks of duration d (Smooth semantics, unlike
// DASH's "additional repeats"), and a missing d is taken from the next chunk's
// start or the presentation end.
func timeline(cs []cXML, total uint64) []chunk {
	var out []chunk
	var cur uint64
	for i, c := range cs {
		if c.T != nil {
			cur = *c.T
		}
		var d uint64
		switch {
		case c.D != nil:
			d = *c.D
		case i+1 < len(cs) && cs[i+1].T != nil && *cs[i+1].T > cur:
			d = *cs[i+1].T - cur
		case total > cur:
			d = total - cur
		}
		n := c.R
		if n < 1 {
			n = 1
		}
		for j := 0; j < n; j++ {
			out = append(out, chunk{t: cur, d: d})
			cur += d
		}
	}
	return out
}

func mediaType(s string) manifest.MediaType {
	switch strings.ToLower(s) {
	case "video":
		return manifest.Video
	case "audio":
		return manifest.Audio
	case "text":
		return manifest.Subtitles
	}
	return manifest.Unknown
}

// expandTemplate fills the StreamIndex Url pattern. Both the spec's spellings
// ({start time}/{start_time}, {bitrate}/{Bitrate}) appear in the wild.
func expandTemplate(tmpl string, bitrate int64, t uint64) string {
	r := strings.NewReplacer(
		"{bitrate}", strconv.FormatInt(bitrate, 10),
		"{Bitrate}", strconv.FormatInt(bitrate, 10),
		"{start time}", strconv.FormatUint(t, 10),
		"{start_time}", strconv.FormatUint(t, 10),
	)
	return r.Replace(tmpl)
}

func resolveURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// ---- init synthesis ----

// synthesizeInit builds the track's init from its codec private data and
// returns it with the RFC 6381 codecs string.
func synthesizeInit(ql *qlXML, st *manifest.Stream, timescale uint32, key *manifest.Key) ([]byte, string, error) {
	spec := mp4.InitSpec{TrackID: 1, Timescale: timescale, Language: st.Language}
	if key != nil {
		spec.Protection = &mp4.ProtectionSpec{KID: key.KID, IVSize: 8}
	}
	priv, err := hex.DecodeString(strings.TrimSpace(ql.CodecPrivateData))
	if err != nil {
		return nil, "", fmt.Errorf("bad CodecPrivateData")
	}
	var codecs string
	switch fourcc := strings.ToUpper(ql.FourCC); fourcc {
	case "H264", "AVC1", "DAVC", "X264":
		sps, pps := splitAnnexB(priv)
		if len(sps) == 0 || len(pps) == 0 {
			return nil, "", fmt.Errorf("CodecPrivateData has no SPS/PPS")
		}
		spec.Video = &mp4.VideoSpec{
			Width: uint16(st.Width), Height: uint16(st.Height),
			SPS: sps, PPS: pps, NALLengthSize: ql.NALUnitLength,
		}
		codecs = fmt.Sprintf("avc1.%02x%02x%02x", sps[0][1], sps[0][2], sps[0][3])
	case "AACL", "AACH", "AACP", "AAC":
		asc := priv
		rate, channels := ql.SamplingRate, ql.Channels
		if len(asc) < 2 {
			if rate == 0 || channels == 0 {
				return nil, "", fmt.Errorf("AAC without CodecPrivateData needs SamplingRate and Channels")
			}
			asc = buildASC(rate, channels)
		}
		objType, ascRate, ascChannels := readASC(asc)
		if rate == 0 {
			rate = ascRate
		}
		if channels == 0 {
			channels = ascChannels
		}
		spec.Audio = &mp4.AudioSpec{Channels: uint16(channels), SampleRate: uint32(rate), ASC: asc}
		if fourcc == "AACH" || fourcc == "AACP" {
			objType = 5 // HE-AAC signals SBR; the ASC's base object type is still LC
		}
		codecs = fmt.Sprintf("mp4a.40.%d", objType)
	default:
		return nil, "", fmt.Errorf("unsupported FourCC %q", ql.FourCC)
	}
	return mp4.BuildInit(spec), codecs, nil
}

// splitAnnexB splits a start-code-delimited H.264 parameter-set blob into its
// SPS (type 7) and PPS (type 8) NAL units.
func splitAnnexB(b []byte) (sps, pps [][]byte) {
	var nals [][]byte
	start := -1
	for i := 0; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				nals = append(nals, trimZeros(b[start:i]))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		nals = append(nals, b[start:])
	}
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		switch n[0] & 0x1f {
		case 7:
			sps = append(sps, n)
		case 8:
			pps = append(pps, n)
		}
	}
	return sps, pps
}

// trimZeros drops the trailing zero byte of a 4-byte start code's leading 00.
func trimZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

var aacRates = []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// buildASC makes a 2-byte AAC-LC AudioSpecificConfig from the manifest's
// sampling rate and channel count.
func buildASC(rate, channels int) []byte {
	idx := 3 // 48000 when the rate is not a standard one
	for i, r := range aacRates {
		if r == rate {
			idx = i
			break
		}
	}
	v := uint16(2)<<11 | uint16(idx)<<7 | uint16(channels)<<3
	return []byte{byte(v >> 8), byte(v)}
}

// readASC reads the object type, sampling rate and channel configuration from
// an AudioSpecificConfig (the 5/4/4-bit header, with the escape forms).
func readASC(asc []byte) (objType, rate, channels int) {
	if len(asc) < 2 {
		return 2, 0, 0
	}
	// the fields we need sit in the first 37 bits at most; pad to a full word
	bits := binary.BigEndian.Uint64(append(append([]byte(nil), asc...), make([]byte, 8)...)[:8])
	pos := 0
	take := func(n int) int {
		v := int(bits >> (64 - pos - n) & (1<<n - 1))
		pos += n
		return v
	}
	objType = take(5)
	if objType == 31 {
		objType = 32 + take(6)
	}
	if fi := take(4); fi == 15 {
		rate = take(24)
	} else if fi < len(aacRates) {
		rate = aacRates[fi]
	}
	channels = take(4)
	return objType, rate, channels
}

// ---- PlayReady ----

var kidRe = regexp.MustCompile(`<KID(?:\s[^>]*?\bVALUE="([^"]+)"[^>]*)?>(?:([^<]*)</KID>)?`)

// playReadyKID extracts the default KID from a PlayReady ProtectionHeader: the
// base64 PlayReady Object holds a UTF-16LE WRMHEADER document whose KID is a
// base64 GUID in little-endian layout, byte-swapped here to the CENC form. The
// zero KID means none was found; the -key KID (or a single bare key) then
// stands in.
func playReadyKID(headers []protHeaderXML) [16]byte {
	var zero [16]byte
	for _, h := range headers {
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(h.Data), ""))
		if err != nil {
			continue
		}
		for _, doc := range playReadyRecords(raw) {
			m := kidRe.FindStringSubmatch(doc)
			if m == nil {
				continue
			}
			guid, err := base64.StdEncoding.DecodeString(firstNonEmpty(m[1], m[2]))
			if err != nil || len(guid) != 16 {
				continue
			}
			var kid [16]byte
			copy(kid[:], guid)
			// GUID → CENC KID: the first three fields are little-endian.
			kid[0], kid[1], kid[2], kid[3] = kid[3], kid[2], kid[1], kid[0]
			kid[4], kid[5] = kid[5], kid[4]
			kid[6], kid[7] = kid[7], kid[6]
			return kid
		}
	}
	return zero
}

// playReadyRecords decodes the rights-management (type 1) records of a
// PlayReady Object as text. When the record framing doesn't parse, the whole
// blob is decoded instead so an unframed header still yields its KID.
func playReadyRecords(raw []byte) []string {
	var docs []string
	if len(raw) >= 10 {
		count := int(binary.LittleEndian.Uint16(raw[4:]))
		off := 6
		for i := 0; i < count && off+4 <= len(raw); i++ {
			typ := binary.LittleEndian.Uint16(raw[off:])
			n := int(binary.LittleEndian.Uint16(raw[off+2:]))
			off += 4
			if off+n > len(raw) {
				break
			}
			if typ == 1 {
				docs = append(docs, utf16le(raw[off:off+n]))
			}
			off += n
		}
	}
	if len(docs) == 0 {
		docs = append(docs, utf16le(raw))
	}
	return docs
}

func utf16le(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// ---- helpers ----

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
