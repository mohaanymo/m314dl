package restream

import (
	"bytes"
	"net/http"
	"strings"
)

// Server exposes a Publisher over HTTP as a live HLS origin:
//
//	GET /live.m3u8          → multivariant (master) playlist
//	GET /{track}/index.m3u8 → a track's media playlist
//	GET /{track}/init.mp4   → a track's fMP4 init segment (EXT-X-MAP target)
//	GET /{track}/{seg}      → one media segment (000123.ts / .m4s)
//
// Playlists are rendered fresh per request (no-cache); segments are served with
// http.ServeContent so players get Range, ETag, and conditional requests for
// free, straight from the shared in-memory window — one copy feeds every viewer.
type Server struct {
	pub *Publisher
}

func NewServer(pub *Publisher) *Server { return &Server{pub: pub} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live.m3u8", s.master)
	mux.HandleFunc("GET /{track}/index.m3u8", s.media)
	mux.HandleFunc("GET /{track}/init.mp4", s.initSeg)
	mux.HandleFunc("GET /{track}/{seg}", s.segment)
	return mux
}

// DASHHandler serves the same in-memory window as a DASH presentation:
//
//	GET /live.mpd         → the manifest (SegmentTemplate + SegmentTimeline)
//	GET /{track}/init.mp4 → init segment (shared with the HLS handler)
//	GET /{track}/{seg}    → one media segment (shared with the HLS handler)
func (s *Server) DASHHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live.mpd", s.mpd)
	mux.HandleFunc("GET /{track}/init.mp4", s.initSeg)
	mux.HandleFunc("GET /{track}/{seg}", s.segment)
	return mux
}

const (
	mimeM3U8 = "application/vnd.apple.mpegurl"
	mimeMPD  = "application/dash+xml"
)

func (s *Server) mpd(w http.ResponseWriter, r *http.Request) {
	cors(w)
	w.Header().Set("Content-Type", mimeMPD)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write(s.pub.DASHManifest())
}

func (s *Server) master(w http.ResponseWriter, r *http.Request) {
	writePlaylist(w, s.pub.masterPlaylist())
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	t, ok := s.pub.Track(r.PathValue("track"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writePlaylist(w, t.mediaPlaylist())
}

func (s *Server) initSeg(w http.ResponseWriter, r *http.Request) {
	t, ok := s.pub.Track(r.PathValue("track"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, modtime, ok := t.initSegment()
	if !ok {
		http.NotFound(w, r)
		return
	}
	cors(w)
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, "init.mp4", modtime, bytes.NewReader(data))
}

func (s *Server) segment(w http.ResponseWriter, r *http.Request) {
	t, ok := s.pub.Track(r.PathValue("track"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("seg")
	data, modtime, ok := t.segmentByName(name)
	if !ok {
		// Aged out of the window, or never existed. 404 is correct: a live
		// client should already have advanced to a newer segment.
		http.NotFound(w, r)
		return
	}
	cors(w)
	w.Header().Set("Content-Type", segmentMIME(name))
	http.ServeContent(w, r, name, modtime, bytes.NewReader(data))
}

func writePlaylist(w http.ResponseWriter, body []byte) {
	cors(w)
	w.Header().Set("Content-Type", mimeM3U8)
	// Playlists change every segment; never let a cache pin a stale window.
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Write(body)
}

// NewTSServer serves a continuous MPEG-TS broadcast:
//
//	GET /live.ts → one never-ending transport stream, fanned out from the
//	              shared broadcaster; each viewer joins on a segment boundary.
func NewTSServer(b *TSBroadcaster) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live.ts", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		flusher, _ := w.(http.Flusher)

		sub := b.Subscribe()
		defer b.Unsubscribe(sub)
		for {
			select {
			case seg := <-sub.data:
				if _, err := w.Write(seg); err != nil {
					return // client went away
				}
				if flusher != nil {
					flusher.Flush()
				}
			case <-sub.kicked:
				return // fell too far behind
			case <-b.done:
				return // stream ended
			case <-r.Context().Done():
				return // client disconnected
			}
		}
	})
	return mux
}

func segmentMIME(name string) string {
	if strings.HasSuffix(name, ".ts") {
		return "video/mp2t"
	}
	return "video/mp4" // .m4s and friends
}

// cors lets browser players (hls.js, Shaka) fetch from a different origin.
func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}
