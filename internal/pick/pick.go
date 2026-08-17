// Package pick sorts and selects streams from filter expressions.
package pick

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mohamed/m314dl/internal/manifest"
)

// Sort orders: video by height then bandwidth desc; audio by default flag,
// channels, bandwidth desc; subs stable.
func Sort(streams []*manifest.Stream) {
	sort.SliceStable(streams, func(i, j int) bool {
		a, b := streams[i], streams[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		switch a.Type {
		case manifest.Video:
			if a.Height != b.Height {
				return a.Height > b.Height
			}
		case manifest.Audio:
			if a.Default != b.Default {
				return a.Default
			}
			ca, _ := strconv.ParseFloat(a.Channels, 64)
			cb, _ := strconv.ParseFloat(b.Channels, 64)
			if ca != cb {
				return ca > cb
			}
		}
		return a.Bandwidth > b.Bandwidth
	})
}

// Expr is a parsed selector: "best", "worst", "all", "bestN", or
// colon-separated filters, optionally with "for=best|worst|all|bestN".
// Regex keys: id, lang, name, codecs, res, range, channel (use key!=regex to
// negate). Numeric keys: bwmin/bwmax (kbps), segsmin/segsmax (segment count),
// plistdurmin/plistdurmax (seconds or "20m"). Numeric filters treat an unknown
// value (e.g. HLS segment count before playlist expansion) as passing.
type Expr struct {
	take    string // best / worst / all / bestN / worstN
	takeN   int
	filters []filter
	none    bool
}

type filter struct {
	key string
	re  *regexp.Regexp
	num float64 // for numeric keys (bandwidth bps, segment count, duration s)
	neg bool    // key!=regex: keep streams that do NOT match
}

var takeRe = regexp.MustCompile(`^(best|worst|all)(\d*)$`)

func ParseExpr(s string) (*Expr, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "none" {
		return &Expr{none: true}, nil
	}
	e := &Expr{take: "all"} // filters without for= keep every match
	if m := takeRe.FindStringSubmatch(s); m != nil {
		e.take = m[1]
		e.takeN = 1
		if m[2] != "" {
			e.takeN, _ = strconv.Atoi(m[2])
		}
		return e, nil
	}
	for _, part := range strings.Split(s, ":") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("bad selector part %q (want key=value)", part)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		// key!=regex negates the match (RE2 has no negative lookahead, so this
		// is how you express a deny-list).
		neg := false
		if strings.HasSuffix(k, "!") {
			neg = true
			k = strings.TrimSuffix(k, "!")
		}
		switch k {
		case "for":
			if m := takeRe.FindStringSubmatch(v); m != nil {
				e.take = m[1]
				e.takeN = 1
				if m[2] != "" {
					e.takeN, _ = strconv.Atoi(m[2])
				}
			} else {
				return nil, fmt.Errorf("bad for=%q", v)
			}
		case "bwmin", "bwmax":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad %s=%q", k, v)
			}
			e.filters = append(e.filters, filter{key: k, num: float64(n * 1000)})
		case "segsmin", "segsmax":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad %s=%q", k, v)
			}
			e.filters = append(e.filters, filter{key: k, num: float64(n)})
		case "plistdurmin", "plistdurmax":
			d, err := parseDurationSeconds(v)
			if err != nil {
				return nil, fmt.Errorf("bad %s=%q: %w", k, v, err)
			}
			e.filters = append(e.filters, filter{key: k, num: d})
		case "id", "lang", "name", "codecs", "res", "range", "channel":
			re, err := regexp.Compile("(?i)" + v)
			if err != nil {
				return nil, fmt.Errorf("bad regex %s=%q: %w", k, v, err)
			}
			e.filters = append(e.filters, filter{key: k, re: re, neg: neg})
		default:
			return nil, fmt.Errorf("unknown selector key %q", k)
		}
	}
	return e, nil
}

// parseDurationSeconds accepts a bare number (seconds) or a Go duration like
// "20m"/"1h30m".
func parseDurationSeconds(v string) (float64, error) {
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	return d.Seconds(), nil
}

func (e *Expr) match(st *manifest.Stream) bool {
	for _, f := range e.filters {
		var val string
		switch f.key {
		case "id":
			val = st.ID
		case "lang":
			val = st.Language
		case "name":
			val = st.Name
		case "codecs":
			val = st.Codecs
		case "res":
			val = st.Resolution()
		case "channel":
			val = st.Channels
		case "bwmin":
			if st.Bandwidth < int64(f.num) {
				return false
			}
			continue
		case "bwmax":
			if st.Bandwidth > int64(f.num) {
				return false
			}
			continue
		case "segsmin":
			// unknown count (HLS before media-playlist expansion) passes
			if n := len(st.Segments); n > 0 && float64(n) < f.num {
				return false
			}
			continue
		case "segsmax":
			if n := len(st.Segments); n > 0 && float64(n) > f.num {
				return false
			}
			continue
		case "plistdurmin":
			if d := st.Duration(); d > 0 && d < f.num {
				return false
			}
			continue
		case "plistdurmax":
			if d := st.Duration(); d > 0 && d > f.num {
				return false
			}
			continue
		}
		if f.re.MatchString(val) == f.neg { // neg: keep non-matches
			return false
		}
	}
	return true
}

// Apply filters+takes from a pre-sorted same-type slice.
func (e *Expr) Apply(streams []*manifest.Stream) []*manifest.Stream {
	if e.none {
		return nil
	}
	var pool []*manifest.Stream
	for _, st := range streams {
		if e.match(st) {
			pool = append(pool, st)
		}
	}
	switch e.take {
	case "all":
		return pool
	case "worst":
		if len(pool) > e.takeN {
			pool = pool[len(pool)-e.takeN:]
		}
		return pool
	default: // best
		if len(pool) > e.takeN {
			pool = pool[:e.takeN]
		}
		return pool
	}
}

// Select applies the three expressions with HLS group awareness: when the
// chosen video names an audio/subtitle rendition group, candidates from that
// group are preferred so "best audio" belongs to the picked variant.
func Select(streams []*manifest.Stream, ve, ae, se *Expr) []*manifest.Stream {
	Sort(streams)
	byType := map[manifest.MediaType][]*manifest.Stream{}
	for _, st := range streams {
		byType[st.Type] = append(byType[st.Type], st)
	}
	var out []*manifest.Stream
	videos := ve.Apply(byType[manifest.Video])
	out = append(out, videos...)

	audioPool := byType[manifest.Audio]
	subPool := byType[manifest.Subtitles]
	if len(videos) == 1 {
		if g := videos[0].AudioGroup; g != "" {
			if grp := inGroup(audioPool, g); len(grp) > 0 {
				audioPool = grp
			}
		}
		if g := videos[0].SubsGroup; g != "" {
			if grp := inGroup(subPool, g); len(grp) > 0 {
				subPool = grp
			}
		}
	}
	// default audio: best rendition per language; audio-only variants are
	// player fallbacks — only used when there are no renditions and the
	// picked video carries no muxed audio
	if ae == nil {
		renditions, variants := splitAudio(audioPool)
		switch {
		case len(renditions) > 0:
			out = append(out, bestPerLanguage(renditions)...)
		case allMuxedAudio(videos):
			// video segments already contain audio
		case len(variants) > 0:
			out = append(out, variants[0])
		}
	} else {
		out = append(out, ae.Apply(audioPool)...)
	}
	if se == nil {
		out = append(out, subPool...) // subtitles are cheap: default all
	} else {
		out = append(out, se.Apply(subPool)...)
	}
	return out
}

func splitAudio(streams []*manifest.Stream) (renditions, variants []*manifest.Stream) {
	for _, st := range streams {
		if st.GroupID != "" {
			renditions = append(renditions, st)
		} else {
			variants = append(variants, st)
		}
	}
	return
}

func allMuxedAudio(videos []*manifest.Stream) bool {
	if len(videos) == 0 {
		return false
	}
	for _, v := range videos {
		if !v.MuxedAudio {
			return false
		}
	}
	return true
}

func inGroup(streams []*manifest.Stream, group string) []*manifest.Stream {
	var out []*manifest.Stream
	for _, st := range streams {
		if st.GroupID == group {
			out = append(out, st)
		}
	}
	return out
}

func bestPerLanguage(streams []*manifest.Stream) []*manifest.Stream {
	seen := map[string]bool{}
	var out []*manifest.Stream
	for _, st := range streams { // pre-sorted best-first
		if !seen[st.Language] {
			seen[st.Language] = true
			out = append(out, st)
		}
	}
	return out
}
