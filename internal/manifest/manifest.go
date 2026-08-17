// Package manifest defines the protocol-agnostic streaming model.
// HLS and DASH parsers both normalize into these types; the download
// engine only ever sees this model.
package manifest

import (
	"fmt"
	"time"
)

type MediaType int

const (
	Video MediaType = iota
	Audio
	Subtitles
	Unknown
)

func (t MediaType) String() string {
	switch t {
	case Video:
		return "video"
	case Audio:
		return "audio"
	case Subtitles:
		return "subtitles"
	}
	return "unknown"
}

// EncMethod is the segment encryption method.
type EncMethod string

const (
	EncNone      EncMethod = ""
	EncAES128    EncMethod = "AES-128"    // whole-segment AES-CBC, supported in-process
	EncSampleAES EncMethod = "SAMPLE-AES" // unsupported: reported, not downloaded blindly
	EncCENC      EncMethod = "CENC"       // DRM: detected and refused
)

// ByteRange is an inclusive HTTP byte range.
type ByteRange struct {
	Start int64
	End   int64 // -1 = open-ended
}

func (r *ByteRange) Header() string {
	if r.End < 0 {
		return fmt.Sprintf("bytes=%d-", r.Start)
	}
	return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
}

type Key struct {
	Method EncMethod
	URI    string   // http(s), data:, or empty
	IV     []byte   // nil = derive from segment media sequence (HLS rule)
	KID    [16]byte // CENC: default key id (from manifest or init tenc); zero = unknown
}

type InitMap struct {
	URL   string
	Range *ByteRange
}

type Segment struct {
	URL           string
	Duration      float64 // seconds
	Range         *ByteRange
	Key           *Key // nil = clear
	Init          *InitMap
	Seq           int64 // media sequence / $Number$; IV fallback + ordering
	Discontinuity bool
}

// Stream is one downloadable track (video variant, audio rendition, subtitle).
type Stream struct {
	Type         MediaType
	ID           string // stable id for selection/display
	PlaylistURL  string // media playlist / representation locator (for live refresh)
	Bandwidth    int64
	Width        int
	Height       int
	FrameRate    float64
	Codecs       string
	Language     string
	Name         string
	Channels     string
	Default      bool
	AudioGroup   string // HLS: groups this video variant references
	SubsGroup    string
	GroupID      string // HLS: group this rendition belongs to
	MuxedAudio   bool   // HLS variant whose segments carry audio too
	Live         bool
	MediaSeq     int64 // first segment's media sequence (live window tracking)
	TargetDur    float64
	Refresh      time.Duration // live playlist refresh interval
	Init         *InitMap
	Segments     []Segment
	SegmentsFull bool // segments already populated (bare media playlist)
}

// Resolution returns "WxH" or "".
func (s *Stream) Resolution() string {
	if s.Width == 0 && s.Height == 0 {
		return ""
	}
	return fmt.Sprintf("%dx%d", s.Width, s.Height)
}

// Duration sums segment durations.
func (s *Stream) Duration() float64 {
	var d float64
	for i := range s.Segments {
		d += s.Segments[i].Duration
	}
	return d
}

func (s *Stream) String() string {
	out := s.Type.String()
	if r := s.Resolution(); r != "" {
		out += " " + r
	}
	if s.Bandwidth > 0 {
		out += fmt.Sprintf(" %dkbps", s.Bandwidth/1000)
	}
	if s.Codecs != "" {
		out += " " + s.Codecs
	}
	if s.Language != "" {
		out += " [" + s.Language + "]"
	}
	if s.Name != "" {
		out += " (" + s.Name + ")"
	}
	if s.Live {
		out += " LIVE"
	}
	return out
}

// Master is the parsed top-level manifest.
type Master struct {
	URL     string // final (post-redirect) manifest URL, base for relatives
	Live    bool
	Streams []*Stream
}
