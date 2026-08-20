package restream

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mohamed/m314dl/internal/manifest"
)

// Live/VOD DASH output. The same Track window that feeds HLS also feeds a DASH
// SegmentTemplate+SegmentTimeline manifest — one AdaptationSet per media type,
// one Representation per track, each with its own timeline built from the
// segments' real durations. SegmentTimeline (over $Number$/$Time$ arithmetic) is
// the robust choice: it states each segment's start and duration explicitly, so
// variable-duration segments and a source that rewinds its own numbering can't
// desync the manifest from what's on disk.
//
// A dynamic (live) MPD advertises minimumUpdatePeriod so players re-fetch it;
// a static (VOD) MPD lists the whole timeline and a mediaPresentationDuration.
// DASH segments are fMP4 — a TS source must be remuxed first (a later phase),
// which the coordinator enforces before choosing this output.

const isoTime = "2006-01-02T15:04:05Z"

// DASHManifest renders the presentation as an MPD.
func (p *Publisher) DASHManifest() []byte {
	live := p.isLive()
	td := p.maxTargetDur()
	now := time.Now().UTC()

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")

	profile := "urn:mpeg:dash:profile:isoff-on-demand:2011"
	typ := "static"
	if live {
		profile = "urn:mpeg:dash:profile:isoff-live:2011"
		typ = "dynamic"
	}
	fmt.Fprintf(&b, `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles=%q type=%q minBufferTime="PT%.1fS"`,
		profile, typ, td)
	if live {
		fmt.Fprintf(&b, ` availabilityStartTime=%q publishTime=%q minimumUpdatePeriod="PT%.0fS" timeShiftBufferDepth="PT%.0fS" suggestedPresentationDelay="PT%.0fS"`,
			p.start.UTC().Format(isoTime), now.Format(isoTime), td, td*float64(windowSize), td*3)
	} else {
		fmt.Fprintf(&b, ` mediaPresentationDuration="PT%.3fS"`, p.totalDur())
	}
	b.WriteString(">\n")
	b.WriteString(`  <Period id="0" start="PT0S">` + "\n")

	// One AdaptationSet per media type, in a stable order (video, then audio).
	var video, audio []*Track
	for _, t := range p.tracks {
		switch t.typ {
		case manifest.Audio:
			audio = append(audio, t)
		case manifest.Video:
			video = append(video, t)
		}
	}
	for _, t := range video {
		writeAdaptationSet(&b, t, "video", "video/mp4")
	}
	for _, t := range audio {
		writeAdaptationSet(&b, t, "audio", "audio/mp4")
	}

	b.WriteString("  </Period>\n</MPD>\n")
	return []byte(b.String())
}

func (p *Publisher) isLive() bool {
	for _, t := range p.tracks {
		if t.live {
			return true
		}
	}
	return false
}

func (p *Publisher) maxTargetDur() float64 {
	td := 0.0
	for _, t := range p.tracks {
		t.mu.RLock()
		if t.targetDur > td {
			td = t.targetDur
		}
		t.mu.RUnlock()
	}
	if td <= 0 {
		td = 2
	}
	return td
}

// totalDur is the published presentation length (VOD), taken from the longest
// track's last segment.
func (p *Publisher) totalDur() float64 {
	var max float64
	for _, t := range p.tracks {
		t.mu.RLock()
		if n := len(t.segs); n > 0 {
			end := float64(t.segs[n-1].startMS)/1000 + t.segs[n-1].dur
			if end > max {
				max = end
			}
		}
		t.mu.RUnlock()
	}
	return max
}

func writeAdaptationSet(b *strings.Builder, t *Track, contentType, mime string) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	fmt.Fprintf(b, `    <AdaptationSet contentType=%q mimeType=%q segmentAlignment="true"`, contentType, mime)
	if t.language != "" {
		fmt.Fprintf(b, ` lang=%q`, t.language)
	}
	b.WriteString(">\n")

	bw := t.bitrateLocked()
	if bw == 0 {
		bw = 2_000_000
	}
	fmt.Fprintf(b, `      <Representation id=%q bandwidth="%d"`, t.ID, bw)
	if t.codecs != "" {
		fmt.Fprintf(b, ` codecs=%q`, t.codecs)
	}
	if contentType == "video" && (t.width > 0 || t.height > 0) {
		fmt.Fprintf(b, ` width="%d" height="%d"`, t.width, t.height)
	}
	if contentType == "video" && t.frameRate > 0 {
		fmt.Fprintf(b, ` frameRate="%g"`, t.frameRate)
	}
	b.WriteString(">\n")

	if contentType == "audio" && t.channels != "" {
		fmt.Fprintf(b, `        <AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value=%q/>`+"\n", t.channels)
	}

	visible := t.visibleLocked()
	startNum := int64(0)
	if len(visible) > 0 {
		startNum = visible[0].seq
	}
	// $Number%06d$ matches the six-digit segment filenames the window stores.
	fmt.Fprintf(b, `        <SegmentTemplate timescale="1000" initialization="$RepresentationID$/init.mp4" media="$RepresentationID$/$Number%%06d$.m4s" startNumber="%d">`+"\n", startNum)
	b.WriteString("          <SegmentTimeline>\n")
	writeTimeline(b, visible)
	b.WriteString("          </SegmentTimeline>\n")
	b.WriteString("        </SegmentTemplate>\n      </Representation>\n    </AdaptationSet>\n")
}

// writeTimeline emits <S> entries, collapsing runs of equal-duration segments
// into a single entry with @r. Every run carries an explicit @t, so a gap or a
// trimmed window can never leave the timeline mis-anchored.
func writeTimeline(b *strings.Builder, segs []*segment) {
	for i := 0; i < len(segs); {
		d := int64(math.Round(segs[i].dur * 1000))
		t0 := segs[i].startMS
		r := 0
		for i+r+1 < len(segs) && int64(math.Round(segs[i+r+1].dur*1000)) == d {
			r++
		}
		if r > 0 {
			fmt.Fprintf(b, `            <S t="%d" d="%d" r="%d"/>`+"\n", t0, d, r)
		} else {
			fmt.Fprintf(b, `            <S t="%d" d="%d"/>`+"\n", t0, d)
		}
		i += r + 1
	}
}
