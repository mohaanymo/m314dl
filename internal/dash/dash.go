// Package dash parses MPD manifests into the manifest model.
package dash

import (
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mohamed/m314dl/internal/manifest"
)

func IsDASH(body []byte) bool {
	head := string(body[:min(512, len(body))])
	return strings.Contains(head, "<MPD") || strings.Contains(head, "<?xml") && strings.Contains(string(body), "<MPD")
}

// ---- XML model (namespace-agnostic: tags without ns match any) ----

type mpdXML struct {
	Type                      string      `xml:"type,attr"`
	MediaPresentationDuration string      `xml:"mediaPresentationDuration,attr"`
	AvailabilityStartTime     string      `xml:"availabilityStartTime,attr"`
	TimeShiftBufferDepth      string      `xml:"timeShiftBufferDepth,attr"`
	MinimumUpdatePeriod       string      `xml:"minimumUpdatePeriod,attr"`
	BaseURL                   []string    `xml:"BaseURL"`
	Periods                   []periodXML `xml:"Period"`
}

type periodXML struct {
	ID             string     `xml:"id,attr"`
	Start          string     `xml:"start,attr"`
	Duration       string     `xml:"duration,attr"`
	BaseURL        []string   `xml:"BaseURL"`
	AdaptationSets []adaptXML `xml:"AdaptationSet"`
}

type adaptXML struct {
	ID                string    `xml:"id,attr"`
	MimeType          string    `xml:"mimeType,attr"`
	ContentType       string    `xml:"contentType,attr"`
	Lang              string    `xml:"lang,attr"`
	Codecs            string    `xml:"codecs,attr"`
	FrameRate         string    `xml:"frameRate,attr"`
	BaseURL           []string  `xml:"BaseURL"`
	SegmentTemplate   *tmplXML  `xml:"SegmentTemplate"`
	SegmentList       *listXML  `xml:"SegmentList"`
	ContentProtection []cpXML   `xml:"ContentProtection"`
	Roles             []roleXML `xml:"Role"`
	Essential         []propXML `xml:"EssentialProperty"`
	Representations   []repXML  `xml:"Representation"`
}

type roleXML struct {
	Value string `xml:"value,attr"`
}

type propXML struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
}

type repXML struct {
	ID                string    `xml:"id,attr"`
	Bandwidth         int64     `xml:"bandwidth,attr"`
	Width             int       `xml:"width,attr"`
	Height            int       `xml:"height,attr"`
	Codecs            string    `xml:"codecs,attr"`
	MimeType          string    `xml:"mimeType,attr"`
	FrameRate         string    `xml:"frameRate,attr"`
	Lang              string    `xml:"lang,attr"`
	BaseURL           []string  `xml:"BaseURL"`
	SegmentTemplate   *tmplXML  `xml:"SegmentTemplate"`
	SegmentList       *listXML  `xml:"SegmentList"`
	SegmentBase       *baseXML  `xml:"SegmentBase"`
	ContentProtection []cpXML   `xml:"ContentProtection"`
	AudioChannels     []acXML   `xml:"AudioChannelConfiguration"`
	Essential         []propXML `xml:"EssentialProperty"`
}

type acXML struct {
	Value string `xml:"value,attr"`
}

type cpXML struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	DefaultKID  string `xml:"default_KID,attr"`
}

type tmplXML struct {
	Media          string       `xml:"media,attr"`
	Initialization string       `xml:"initialization,attr"`
	Timescale      uint64       `xml:"timescale,attr"`
	Duration       uint64       `xml:"duration,attr"`
	StartNumber    *uint64      `xml:"startNumber,attr"`
	Timeline       *timelineXML `xml:"SegmentTimeline"`
}

type timelineXML struct {
	S []sXML `xml:"S"`
}

type sXML struct {
	T *uint64 `xml:"t,attr"`
	D uint64  `xml:"d,attr"`
	R int64   `xml:"r,attr"`
}

type listXML struct {
	Timescale      uint64    `xml:"timescale,attr"`
	Duration       uint64    `xml:"duration,attr"`
	Initialization *initXML  `xml:"Initialization"`
	SegmentURLs    []segURLx `xml:"SegmentURL"`
}

type segURLx struct {
	Media      string `xml:"media,attr"`
	MediaRange string `xml:"mediaRange,attr"`
}

type baseXML struct {
	IndexRange     string   `xml:"indexRange,attr"`
	Initialization *initXML `xml:"Initialization"`
}

type initXML struct {
	SourceURL string `xml:"sourceURL,attr"`
	Range     string `xml:"range,attr"`
}

// ---- parsing ----

// Now is stubbed in tests; live edge computation needs wall clock.
var Now = time.Now

// Parse converts an MPD into the unified model. Segments are fully populated.
func Parse(body []byte, mpdURL string) (*manifest.Master, error) {
	var doc mpdXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("dash: parse MPD: %w", err)
	}
	live := doc.Type == "dynamic"
	m := &manifest.Master{URL: mpdURL, Live: live}
	mpdBase := resolveChain(mpdURL, doc.BaseURL)
	mpdDur := parseISODuration(doc.MediaPresentationDuration)
	tsbd := parseISODuration(doc.TimeShiftBufferDepth)
	ast, _ := time.Parse(time.RFC3339, doc.AvailabilityStartTime)

	byKey := map[string]*manifest.Stream{}
	periodStart := 0.0
	for pi, p := range doc.Periods {
		pBase := resolveChain(mpdBase, p.BaseURL)
		pDur := parseISODuration(p.Duration)
		if v := parseISODuration(p.Start); v > 0 {
			periodStart = v
		}
		if pDur == 0 && len(doc.Periods) == 1 {
			pDur = mpdDur
		}
		if live && pi > 0 {
			break // live: record newest reachable period only; refresh keeps up
		}
		for ai, as := range p.AdaptationSets {
			aBase := resolveChain(pBase, as.BaseURL)
			for ri, rep := range as.Representations {
				if isTrickMode(&as, &rep) {
					continue
				}
				rBase := resolveChain(aBase, rep.BaseURL)
				st := buildStream(&as, &rep, ai, ri)
				st.Live = live
				st.PlaylistURL = mpdURL
				ctx := segCtx{
					base: rBase, rep: &rep, as: &as,
					periodDur: pDur, periodStart: periodStart,
					live: live, tsbd: tsbd, ast: ast,
				}
				segs, init, err := buildSegments(ctx)
				if err != nil {
					return nil, err
				}
				if len(rep.ContentProtection) > 0 || len(as.ContentProtection) > 0 {
					kid := defaultKID(rep.ContentProtection, as.ContentProtection)
					for i := range segs {
						segs[i].Key = &manifest.Key{Method: manifest.EncCENC, KID: kid}
					}
				}
				key := st.ID
				if prev, ok := byKey[key]; ok {
					// Same track ID recurs across periods. Concatenate (with a
					// discontinuity) ONLY when the encoding is truly continuous:
					// same codecs, resolution, init segment and content key.
					// Ad/bumper periods (SSAI) and per-period re-encodes differ
					// on at least one of those, so keep them as separate tracks
					// the caller can drop (e.g. -sv segsMin=2:plistDurMin=20m)
					// rather than splicing ads in or dropping a period's init.
					if len(segs) > 0 && mergeCompatible(prev, st, init, firstSegKey(segs)) {
						segs[0].Discontinuity = true
						base := int64(len(prev.Segments))
						for i := range segs {
							segs[i].Seq = base + int64(i)
						}
						prev.Segments = append(prev.Segments, segs...)
						continue
					}
					// incompatible: give it a period-unique ID so it is a
					// distinct, filterable track instead of colliding.
					st.ID = fmt.Sprintf("%s.p%d", st.ID, pi)
					key = st.ID
				}
				st.Init = init
				st.Segments = segs
				st.SegmentsFull = true
				if live {
					refresh := parseISODuration(doc.MinimumUpdatePeriod)
					if refresh <= 0 {
						refresh = math.Max(tsbd/2, 2)
					}
					st.Refresh = time.Duration(refresh * float64(time.Second))
				}
				byKey[key] = st
				m.Streams = append(m.Streams, st)
			}
		}
		periodStart += pDur
	}
	return m, nil
}

// trickModeScheme is the DASH-IF EssentialProperty that marks a trick-play
// track (I-frame only, frameRate=1, maxPlayoutRate set). EssentialProperty
// means a client that does not implement the scheme must ignore the element,
// so such representations are dropped instead of offered as ordinary video —
// they often tie with the real top rendition on height and bandwidth, and only
// document order kept "best" from picking the 1 fps track.
const trickModeScheme = "http://dashif.org/guidelines/trickmode"

func isTrickMode(as *adaptXML, rep *repXML) bool {
	for _, p := range append(as.Essential, rep.Essential...) {
		if p.SchemeIDURI == trickModeScheme {
			return true
		}
	}
	return false
}

func buildStream(as *adaptXML, rep *repXML, ai, ri int) *manifest.Stream {
	st := &manifest.Stream{
		ID:        fmt.Sprintf("%s.%d.%d", firstNonEmpty(rep.ID, "r"), ai, ri),
		Bandwidth: rep.Bandwidth,
		Width:     rep.Width,
		Height:    rep.Height,
		Codecs:    firstNonEmpty(rep.Codecs, as.Codecs),
		Language:  firstNonEmpty(rep.Lang, as.Lang),
		Name:      rep.ID,
	}
	st.FrameRate = parseFrameRate(firstNonEmpty(rep.FrameRate, as.FrameRate))
	if len(rep.AudioChannels) > 0 {
		st.Channels = rep.AudioChannels[0].Value
	}
	mime := firstNonEmpty(rep.MimeType, as.MimeType, as.ContentType)
	codec := st.Codecs
	switch {
	case strings.HasPrefix(mime, "video"):
		st.Type = manifest.Video
	case strings.HasPrefix(mime, "audio"):
		st.Type = manifest.Audio
	case strings.HasPrefix(mime, "text") || strings.HasPrefix(mime, "application/ttml") ||
		strings.HasPrefix(mime, "application/mp4") && (strings.HasPrefix(codec, "stpp") || strings.HasPrefix(codec, "wvtt")):
		st.Type = manifest.Subtitles
	case strings.HasPrefix(codec, "stpp"), strings.HasPrefix(codec, "wvtt"):
		st.Type = manifest.Subtitles
	case rep.Width > 0:
		st.Type = manifest.Video
	default:
		st.Type = manifest.Unknown
	}
	for _, r := range as.Roles {
		if strings.Contains(r.Value, "subtitle") {
			st.Type = manifest.Subtitles
		}
	}
	return st
}

type segCtx struct {
	base        string
	rep         *repXML
	as          *adaptXML
	periodDur   float64
	periodStart float64
	live        bool
	tsbd        float64
	ast         time.Time
}

func buildSegments(c segCtx) ([]manifest.Segment, *manifest.InitMap, error) {
	// representation-level wins, adaptation-level fills gaps
	tmpl := mergeTemplates(c.rep.SegmentTemplate, c.as.SegmentTemplate)
	list := c.rep.SegmentList
	if list == nil {
		list = c.as.SegmentList
	}
	switch {
	case tmpl != nil:
		return segmentsFromTemplate(c, tmpl)
	case list != nil:
		return segmentsFromList(c, list)
	case c.rep.SegmentBase != nil:
		// Whole file as one segment, no sidx split. But split off the init header
		// (<Initialization range="0-N">) as a separate init item so a CENC stream
		// can read its tenc/KID — without it the decryptor never runs and every
		// encrypted SegmentBase track dies with "stream is CENC-protected". The
		// media segment then starts AFTER the init range, so the header isn't
		// downloaded twice (the engine writes the init item, then the segment).
		seg := manifest.Segment{URL: c.base, Duration: c.periodDur}
		var init *manifest.InitMap
		if sb := c.rep.SegmentBase; sb.Initialization != nil {
			if r := parseRange(sb.Initialization.Range); r != nil {
				init = &manifest.InitMap{URL: c.base, Range: r}
				seg.Range = &manifest.ByteRange{Start: r.End + 1, End: -1}
			}
		}
		return []manifest.Segment{seg}, init, nil
	default:
		return []manifest.Segment{{URL: c.base, Duration: c.periodDur}}, nil, nil
	}
}

func mergeTemplates(r, a *tmplXML) *tmplXML {
	if r == nil {
		return a
	}
	if a == nil {
		return r
	}
	out := *r
	if out.Media == "" {
		out.Media = a.Media
	}
	if out.Initialization == "" {
		out.Initialization = a.Initialization
	}
	if out.Timescale == 0 {
		out.Timescale = a.Timescale
	}
	if out.Duration == 0 {
		out.Duration = a.Duration
	}
	if out.StartNumber == nil {
		out.StartNumber = a.StartNumber
	}
	if out.Timeline == nil {
		out.Timeline = a.Timeline
	}
	return &out
}

func segmentsFromTemplate(c segCtx, t *tmplXML) ([]manifest.Segment, *manifest.InitMap, error) {
	ts := t.Timescale
	if ts == 0 {
		ts = 1
	}
	startNum := uint64(1)
	if t.StartNumber != nil {
		startNum = *t.StartNumber
	}
	var init *manifest.InitMap
	if t.Initialization != "" {
		init = &manifest.InitMap{URL: resolveURL(c.base, expandTemplate(t.Initialization, c.rep.ID, c.rep.Bandwidth, 0, 0))}
	}
	var segs []manifest.Segment
	if t.Timeline != nil {
		var cur uint64
		num := startNum
		for _, s := range t.Timeline.S {
			if s.T != nil {
				cur = *s.T
			}
			repeat := s.R
			if repeat < 0 {
				// negative r: repeat until period end
				if c.periodDur > 0 && s.D > 0 {
					end := uint64(c.periodDur * float64(ts))
					repeat = int64((end - cur) / s.D)
					if repeat > 0 {
						repeat--
					}
				} else {
					repeat = 0
				}
			}
			for i := int64(0); i <= repeat; i++ {
				u := expandTemplate(t.Media, c.rep.ID, c.rep.Bandwidth, num, cur)
				segs = append(segs, manifest.Segment{
					URL:      resolveURL(c.base, u),
					Duration: float64(s.D) / float64(ts),
					Seq:      int64(num),
				})
				cur += s.D
				num++
			}
		}
		return segs, init, nil
	}
	if t.Duration == 0 {
		return nil, nil, fmt.Errorf("dash: SegmentTemplate without duration or timeline")
	}
	segDur := float64(t.Duration) / float64(ts)
	var count uint64
	if c.live {
		// live edge from wall clock inside the DVR window
		window := c.tsbd
		if window <= 0 {
			window = 60
		}
		count = uint64(math.Ceil(window / segDur))
		if !c.ast.IsZero() {
			elapsed := Now().UTC().Sub(c.ast).Seconds() - c.periodStart
			if avail := int64(elapsed/segDur) - int64(count); avail > 0 {
				startNum += uint64(avail)
			}
		}
	} else {
		if c.periodDur <= 0 {
			return nil, nil, fmt.Errorf("dash: cannot size SegmentTemplate: no period duration")
		}
		count = uint64(math.Ceil(c.periodDur / segDur))
	}
	for i := uint64(0); i < count; i++ {
		num := startNum + i
		u := expandTemplate(t.Media, c.rep.ID, c.rep.Bandwidth, num, uint64(float64(num-startNum)*float64(t.Duration)))
		segs = append(segs, manifest.Segment{
			URL:      resolveURL(c.base, u),
			Duration: segDur,
			Seq:      int64(num),
		})
	}
	return segs, init, nil
}

func segmentsFromList(c segCtx, l *listXML) ([]manifest.Segment, *manifest.InitMap, error) {
	ts := l.Timescale
	if ts == 0 {
		ts = 1
	}
	dur := float64(l.Duration) / float64(ts)
	var init *manifest.InitMap
	if l.Initialization != nil {
		init = &manifest.InitMap{URL: resolveURL(c.base, l.Initialization.SourceURL)}
		if r := parseRange(l.Initialization.Range); r != nil {
			init.Range = r
			if init.URL == "" {
				init.URL = c.base
			}
		}
	}
	segs := make([]manifest.Segment, 0, len(l.SegmentURLs))
	for i, su := range l.SegmentURLs {
		u := c.base
		if su.Media != "" {
			u = resolveURL(c.base, su.Media)
		}
		segs = append(segs, manifest.Segment{
			URL:      u,
			Duration: dur,
			Range:    parseRange(su.MediaRange),
			Seq:      int64(i),
		})
	}
	return segs, init, nil
}

// ---- helpers ----

var tmplVarRe = regexp.MustCompile(`\$(RepresentationID|Bandwidth|Number|Time)(%0(\d+)d)?\$`)

func expandTemplate(s, repID string, bw int64, number, t uint64) string {
	s = tmplVarRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := tmplVarRe.FindStringSubmatch(m)
		var val string
		switch sub[1] {
		case "RepresentationID":
			return repID
		case "Bandwidth":
			val = strconv.FormatInt(bw, 10)
		case "Number":
			val = strconv.FormatUint(number, 10)
		case "Time":
			val = strconv.FormatUint(t, 10)
		}
		if sub[3] != "" {
			width, _ := strconv.Atoi(sub[3])
			for len(val) < width {
				val = "0" + val
			}
		}
		return val
	})
	return strings.ReplaceAll(s, "$$", "$")
}

func resolveChain(base string, urls []string) string {
	if len(urls) == 0 {
		return base
	}
	return resolveURL(base, strings.TrimSpace(urls[0]))
}

func resolveURL(base, ref string) string {
	if ref == "" {
		return base
	}
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

func parseRange(s string) *manifest.ByteRange {
	if s == "" {
		return nil
	}
	var a, b int64
	if _, err := fmt.Sscanf(s, "%d-%d", &a, &b); err != nil {
		return nil
	}
	return &manifest.ByteRange{Start: a, End: b}
}

var isoDurRe = regexp.MustCompile(`^-?P(?:(\d+(?:\.\d+)?)Y)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// parseISODuration returns seconds; 0 on empty/invalid.
func parseISODuration(s string) float64 {
	if s == "" {
		return 0
	}
	m := isoDurRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	f := func(v string) float64 { x, _ := strconv.ParseFloat(v, 64); return x }
	return f(m[1])*365.25*86400 + f(m[2])*30*86400 + f(m[3])*86400 + f(m[4])*3600 + f(m[5])*60 + f(m[6])
}

func parseFrameRate(s string) float64 {
	if s == "" {
		return 0
	}
	if n, d, ok := strings.Cut(s, "/"); ok {
		nf, _ := strconv.ParseFloat(n, 64)
		df, _ := strconv.ParseFloat(d, 64)
		if df > 0 {
			return nf / df
		}
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// mergeCompatible reports whether cur (a later period with the same track ID)
// is a true continuation of prev — same codecs, resolution, init segment and
// content key — so their segments can be concatenated into one track. Anything
// else (SSAI ad periods, per-period re-encodes, per-period KIDs) is not merged.
func mergeCompatible(prev, cur *manifest.Stream, curInit *manifest.InitMap, curKey *manifest.Key) bool {
	if prev.Codecs != cur.Codecs || prev.Width != cur.Width || prev.Height != cur.Height {
		return false
	}
	if !sameInit(prev.Init, curInit) {
		return false
	}
	var prevKey *manifest.Key
	if len(prev.Segments) > 0 {
		prevKey = prev.Segments[0].Key
	}
	return sameKey(prevKey, curKey)
}

func sameInit(a, b *manifest.InitMap) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || a.URL == b.URL
}

func sameKey(a, b *manifest.Key) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || (a.Method == b.Method && a.KID == b.KID)
}

func firstSegKey(segs []manifest.Segment) *manifest.Key {
	if len(segs) > 0 {
		return segs[0].Key
	}
	return nil
}

// defaultKID extracts the cenc:default_KID from ContentProtection descriptors
// (representation-level wins). Returns the zero KID when absent/malformed.
func defaultKID(lists ...[]cpXML) [16]byte {
	var zero [16]byte
	for _, list := range lists {
		for _, cp := range list {
			if cp.DefaultKID == "" {
				continue
			}
			hexStr := strings.ReplaceAll(cp.DefaultKID, "-", "")
			b, err := hex.DecodeString(hexStr)
			if err == nil && len(b) == 16 {
				var kid [16]byte
				copy(kid[:], b)
				return kid
			}
		}
	}
	return zero
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
