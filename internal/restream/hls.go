// Package restream turns the engine's ordered, decrypted segment stream into a
// live HLS presentation served over HTTP — a downloader becomes a re-streamer.
//
// The design deliberately inverts the usual worker model. Instead of piping a
// clear stream into FFmpeg and letting it segment and pace the output, the
// packager receives whole segments already decrypted and in playback order
// (that is exactly what engine.Sink delivers) and does only what a live HLS
// origin must do: keep a rolling window of segments in memory, renumber them
// under its own monotonic sequence, and rewrite the media playlist after each
// one. No subprocess, no -re pacing (the source's own segment cadence paces
// it), no continuity-counter surgery (segments are copied whole, never
// re-muxed), and segments are held once and shared by every viewer rather than
// buffered per-connection.
package restream

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/mohamed/m314dl/internal/engine"
	"github.com/mohamed/m314dl/internal/manifest"
)

const (
	// windowSize is how many segments stay visible in each media playlist. Six
	// is the common live default: enough for a player to build its buffer,
	// small enough to keep latency near three target-durations.
	windowSize = 6
	// tailExtra keeps a few just-evicted segments on hand so a viewer still
	// mid-download of the oldest visible segment finds it instead of a 404.
	tailExtra = 3
	// audioGroup / subsGroup are the HLS rendition-group ids the master's video
	// variants reference. A single group per media type is all a restream needs.
	audioGroup = "aud"
	subsGroup  = "subs"
)

// Publisher holds every track of one live presentation and renders its
// playlists. Tracks are added once at startup, then only their segment windows
// change, so the track list itself needs no lock after setup.
type Publisher struct {
	tracks []*Track
	byID   map[string]*Track
	start  time.Time
}

func NewPublisher() *Publisher {
	return &Publisher{byID: map[string]*Track{}, start: time.Now()}
}

// AddTrack registers a track and returns the engine.Sink that feeds it. Call
// before serving; the returned sink is written by exactly one download stream.
func (p *Publisher) AddTrack(t *Track) engine.Sink {
	p.tracks = append(p.tracks, t)
	p.byID[t.ID] = t
	return t
}

// Track returns a registered track by id.
func (p *Publisher) Track(id string) (*Track, bool) {
	t, ok := p.byID[id]
	return t, ok
}

// End marks every track's playlist finished (appends EXT-X-ENDLIST), used when
// the source stream ends or the operator stops recording.
func (p *Publisher) End() {
	for _, t := range p.tracks {
		t.mu.Lock()
		t.ended = true
		t.mu.Unlock()
	}
}

// Ready reports whether every track has published at least one segment, i.e.
// the presentation is playable. Used to hold the "serving at ..." banner until
// there is actually something to serve.
func (p *Publisher) Ready() bool {
	for _, t := range p.tracks {
		t.mu.RLock()
		n := len(t.segs)
		t.mu.RUnlock()
		if n == 0 {
			return false
		}
	}
	return len(p.tracks) > 0
}

// Stats is a per-track liveness snapshot for the status line.
type Stats struct {
	ID        string
	Type      string
	Published int64 // total segments published over the run
	Window    int   // segments currently visible
	Bitrate   int64 // bits/sec (declared or measured)
}

// StatusLine implements the presentation status contract: one line summarizing
// every track's liveness.
func (p *Publisher) StatusLine() string {
	var parts []string
	for _, s := range p.Stats() {
		br := "?"
		if s.Bitrate > 0 {
			br = fmt.Sprintf("%.1fMbps", float64(s.Bitrate)/1e6)
		}
		parts = append(parts, fmt.Sprintf("%s %d segs %s", s.ID, s.Published, br))
	}
	return strings.Join(parts, " | ")
}

func (p *Publisher) Stats() []Stats {
	out := make([]Stats, 0, len(p.tracks))
	for _, t := range p.tracks {
		t.mu.RLock()
		out = append(out, Stats{
			ID: t.ID, Type: t.typ.String(),
			Published: t.published, Window: len(t.segs), Bitrate: t.bitrateLocked(),
		})
		t.mu.RUnlock()
	}
	return out
}

// ─── track ───────────────────────────────────────────────────────────────────

// Track is one rendition (a video variant, an audio rendition, …). It owns a
// rolling in-memory window of segments and renders its own media playlist. It
// implements engine.Sink: Init/Segment are called in playback order by the
// engine's ordered writer.
type Track struct {
	ID  string
	typ manifest.MediaType

	// Master-playlist metadata, fixed at construction.
	bandwidth  int64
	width      int
	height     int
	frameRate  float64
	codecs     string
	language   string
	name       string
	channels   string
	def        bool
	audioGroup string // video: the audio group it plays with (references audioGroup const)
	subsGroup  string
	fmp4       bool
	live       bool   // live: roll a fixed window; VOD: keep every segment
	segExt     string // "ts" | "m4s"

	mu        sync.RWMutex
	init      []byte
	initAt    time.Time
	segs      []*segment
	targetDur float64
	seq       int64 // output media sequence of the next segment (monotonic; never reset)
	published int64 // total ever published (for stats)
	ended     bool
}

type segment struct {
	name string
	seq  int64
	dur  float64
	disc bool
	data []byte
	at   time.Time
}

// TrackFromStream builds a Track from a selected stream. id is a short,
// URL-safe identifier assigned by the caller (e.g. "video", "audio-fr"). live
// selects the output shape: a rolling window (live) or a full growing playlist
// that ends in EXT-X-ENDLIST (VOD).
func TrackFromStream(id string, st *manifest.Stream, live bool) *Track {
	fmp4 := st.Init != nil || segmentIsFMP4(st)
	ext := "ts"
	if fmp4 {
		ext = "m4s"
	}
	return &Track{
		ID: id, typ: st.Type,
		bandwidth: st.Bandwidth, width: st.Width, height: st.Height,
		frameRate: st.FrameRate, codecs: st.Codecs,
		language: st.Language, name: st.Name, channels: st.Channels, def: st.Default,
		fmp4: fmp4, live: live, segExt: ext,
	}
}

func segmentIsFMP4(st *manifest.Stream) bool {
	if len(st.Segments) == 0 {
		return false
	}
	u := st.Segments[0].URL
	return strings.Contains(u, ".m4s") || strings.Contains(u, ".mp4") || strings.Contains(u, ".cmf")
}

// Init implements engine.Sink: stores the fMP4 initialization segment.
func (t *Track) Init(data []byte) error {
	buf := append([]byte(nil), data...)
	t.mu.Lock()
	t.init = buf
	t.initAt = time.Now()
	t.mu.Unlock()
	return nil
}

// Segment implements engine.Sink: appends one segment to the rolling window and
// trims anything aged out. Renumbers under the track's own monotonic sequence
// so a source that rewinds its media sequence cannot wedge or rewind output.
func (t *Track) Segment(info engine.SegmentInfo, data []byte) error {
	dur := info.Duration
	t.mu.Lock()
	defer t.mu.Unlock()

	// A zero/missing source duration would emit an EXTINF of 0 and break player
	// pacing; fall back to the largest duration seen (or a 2s default).
	if dur <= 0 {
		if dur = t.targetDur; dur <= 0 {
			dur = 2.0
		}
	}
	if dur > t.targetDur {
		t.targetDur = dur
	}

	seq := t.seq
	seg := &segment{
		name: fmt.Sprintf("%06d.%s", seq, t.segExt),
		seq:  seq, dur: dur, disc: info.Discontinuity,
		data: append([]byte(nil), data...), at: time.Now(),
	}
	t.segs = append(t.segs, seg)
	t.seq++
	t.published++

	// Live: trim beyond window+tail; the tail keeps just-evicted segments
	// fetchable a moment longer for clients mid-download of the oldest visible
	// one. VOD: keep everything so the finished playlist is seekable end to end.
	if max := windowSize + tailExtra; t.live && len(t.segs) > max {
		t.segs = t.segs[len(t.segs)-max:]
	}
	return nil
}

// visibleLocked returns the segments that belong in the published playlist:
// every segment for VOD, the newest windowSize for live. Caller holds t.mu.
func (t *Track) visibleLocked() []*segment {
	if !t.live || len(t.segs) <= windowSize {
		return t.segs
	}
	return t.segs[len(t.segs)-windowSize:]
}

// segmentByName returns a segment's bytes and modtime for the HTTP layer.
func (t *Track) segmentByName(name string) ([]byte, time.Time, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, s := range t.segs {
		if s.name == name {
			return s.data, s.at, true
		}
	}
	return nil, time.Time{}, false
}

// initSegment returns the fMP4 init bytes and modtime.
func (t *Track) initSegment() ([]byte, time.Time, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.init == nil {
		return nil, time.Time{}, false
	}
	return t.init, t.initAt, true
}

// bitrateLocked returns the track's bitrate: the declared value, else the peak
// measured from the current window, else 0. Caller holds t.mu.
func (t *Track) bitrateLocked() int64 {
	if t.bandwidth > 0 {
		return t.bandwidth
	}
	var peak int64
	for _, s := range t.segs {
		if s.dur > 0 {
			if br := int64(float64(len(s.data)) * 8 / s.dur); br > peak {
				peak = br
			}
		}
	}
	return peak
}

// mediaPlaylist renders this track's live media playlist (RFC 8216).
func (t *Track) mediaPlaylist() []byte {
	t.mu.RLock()
	defer t.mu.RUnlock()

	visible := t.visibleLocked()
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	// fMP4 output uses EXT-X-MAP, which requires version 6; plain TS is version 3.
	ver := 3
	if t.fmp4 {
		ver = 6
	}
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", ver)
	td := int(math.Ceil(t.targetDur))
	if td < 1 {
		td = 1
	}
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", td)
	seq := int64(0)
	if len(visible) > 0 {
		seq = visible[0].seq
	}
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", seq)
	if t.fmp4 && t.init != nil {
		b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
	}
	for _, s := range visible {
		if s.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", s.dur, s.name)
	}
	if t.ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(b.String())
}

// ─── master playlist ─────────────────────────────────────────────────────────

// masterPlaylist renders the multivariant playlist: audio/subtitle renditions
// as EXT-X-MEDIA, video variants as EXT-X-STREAM-INF referencing them. It is
// regenerated per request, so BANDWIDTH self-corrects from measured bitrate as
// segments arrive rather than being pinned to a guess.
func (p *Publisher) masterPlaylist() []byte {
	var audio, subs, video []*Track
	for _, t := range p.tracks {
		switch t.typ {
		case manifest.Audio:
			audio = append(audio, t)
		case manifest.Subtitles:
			subs = append(subs, t)
		default:
			video = append(video, t)
		}
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n")

	for i, a := range audio {
		def := i == 0 // first rendition of a group is the default
		writeMedia(&b, "AUDIO", audioGroup, a, def)
	}
	for i, s := range subs {
		writeMedia(&b, "SUBTITLES", subsGroup, s, i == 0)
	}

	haveAudio := len(audio) > 0
	haveSubs := len(subs) > 0
	audioBitrate := int64(0)
	if haveAudio {
		audio[0].mu.RLock()
		audioBitrate = audio[0].bitrateLocked()
		audio[0].mu.RUnlock()
	}

	for _, v := range video {
		v.mu.RLock()
		bw := v.bitrateLocked() + audioBitrate
		if bw == 0 {
			bw = 2_000_000 // no segments measured yet; corrects on the next fetch
		}
		codecs := v.codecs
		res := v.resolution()
		fr := v.frameRate
		v.mu.RUnlock()
		if haveAudio && audio[0].codecs != "" && codecs != "" {
			codecs = codecs + "," + audio[0].codecs
		}

		var attrs []string
		attrs = append(attrs, fmt.Sprintf("BANDWIDTH=%d", bw))
		if codecs != "" {
			attrs = append(attrs, fmt.Sprintf("CODECS=%q", codecs))
		}
		if res != "" {
			attrs = append(attrs, "RESOLUTION="+res)
		}
		if fr > 0 {
			attrs = append(attrs, fmt.Sprintf("FRAME-RATE=%.3f", fr))
		}
		if haveAudio {
			attrs = append(attrs, fmt.Sprintf("AUDIO=%q", audioGroup))
		}
		if haveSubs {
			attrs = append(attrs, fmt.Sprintf("SUBTITLES=%q", subsGroup))
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\n%s/index.m3u8\n", strings.Join(attrs, ","), v.ID)
	}

	// Audio-only presentation (no video variant): still needs a STREAM-INF or
	// players see an empty master. Point it at the first audio rendition.
	if len(video) == 0 && haveAudio {
		a := audio[0]
		a.mu.RLock()
		bw := a.bitrateLocked()
		a.mu.RUnlock()
		if bw == 0 {
			bw = 128_000
		}
		attrs := fmt.Sprintf("BANDWIDTH=%d", bw)
		if a.codecs != "" {
			attrs += fmt.Sprintf(",CODECS=%q", a.codecs)
		}
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:%s\n%s/index.m3u8\n", attrs, a.ID)
	}
	return []byte(b.String())
}

func writeMedia(b *strings.Builder, kind, group string, t *Track, def bool) {
	yesno := func(v bool) string {
		if v {
			return "YES"
		}
		return "NO"
	}
	attrs := []string{
		fmt.Sprintf("TYPE=%s", kind),
		fmt.Sprintf("GROUP-ID=%q", group),
		fmt.Sprintf("NAME=%q", t.displayName()),
		fmt.Sprintf("DEFAULT=%s", yesno(def || t.def)),
		fmt.Sprintf("AUTOSELECT=%s", yesno(def || t.def)),
	}
	if t.language != "" {
		attrs = append(attrs, fmt.Sprintf("LANGUAGE=%q", t.language))
	}
	if kind == "AUDIO" && t.channels != "" {
		attrs = append(attrs, fmt.Sprintf("CHANNELS=%q", t.channels))
	}
	attrs = append(attrs, fmt.Sprintf("URI=%q", t.ID+"/index.m3u8"))
	fmt.Fprintf(b, "#EXT-X-MEDIA:%s\n", strings.Join(attrs, ","))
}

func (t *Track) displayName() string {
	switch {
	case t.name != "":
		return t.name
	case t.language != "":
		return t.language
	default:
		return t.ID
	}
}

func (t *Track) resolution() string {
	if t.width == 0 && t.height == 0 {
		return ""
	}
	return fmt.Sprintf("%dx%d", t.width, t.height)
}
