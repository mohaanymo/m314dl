// Package engine downloads one stream (VOD or live) through a single
// pipeline: feeder → worker pool → ordered writer. Live is just a feeder
// that keeps discovering segments; everything downstream is identical —
// no duplicated VOD/live managers.
package engine

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mohamed/m314dl/internal/bbts"
	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
)

type Config struct {
	Client    *httpx.Client
	Threads   int                 // concurrency ceiling; 0 = auto (up to adaptiveMax)
	Keys      map[[16]byte][]byte // CENC content keys by KID (zero KID = bare key)
	BBTSKey   []byte              // 16-byte AES key; set → every .ts segment is BBTS-decrypted
	AdFilters []*regexp.Regexp
	LiveLimit time.Duration   // stop live recording after this long (0 = until end)
	Stop      <-chan struct{} // graceful live stop: feeder exits, pipeline drains, mux proceeds
	FromStart bool            // live: download the whole DVR window instead of starting at the edge
	Progress  *Progress       // shared across streams; may be nil
	Verbose   func(format string, args ...any)
}

// RefreshFunc re-fetches the playlist and returns the fresh stream state.
type RefreshFunc func(ctx context.Context) (*manifest.Stream, error)

type item struct {
	idx    int64
	seg    *manifest.Segment
	isInit bool
	url    string
	rng    *manifest.ByteRange
	key    *manifest.Key
	seq    int64
}

// DownloadStream downloads st into outPath (raw concatenated media).
// refresh is non-nil for live streams.
func DownloadStream(ctx context.Context, cfg Config, st *manifest.Stream, outPath string, refresh RefreshFunc) error {
	// cfg.Threads is the concurrency ceiling; 0 means auto (handled below).
	if cfg.Verbose == nil {
		cfg.Verbose = func(string, ...any) {}
	}
	for _, seg := range st.Segments {
		if seg.Key != nil && seg.Key.Method == manifest.EncSampleAES {
			return fmt.Errorf("stream %s uses SAMPLE-AES encryption (not supported yet)", st.ID)
		}
	}

	// CENC: build a native decryptor if a key is available; otherwise refuse
	// gracefully (no external mp4decrypt needed — decryption is in-process).
	var dec *cencDecryptor
	if streamIsCENC(st) {
		if len(cfg.Keys) == 0 {
			return fmt.Errorf("stream %s is CENC/DRM-protected; supply the content key with -key KID:KEY", st.ID)
		}
		d, err := newCencDecryptor(ctx, cfg, st)
		if err != nil {
			return err
		}
		dec = d
	}

	// single-segment stream (SegmentBase / plain file): direct streaming path
	if !st.Live && len(st.Segments) == 1 && st.Segments[0].Key == nil && st.Segments[0].Range == nil {
		return downloadSingleFile(ctx, cfg, st, outPath)
	}

	state, out, err := openOutput(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	// Resuming: segments already written count as done, so progress and ETA
	// reflect real remaining work instead of restarting from 0/N.
	if cfg.Progress != nil {
		if n := state.NextIdx.Load(); n > 0 {
			cfg.Progress.AddDone(n)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Adaptive concurrency: spawn `ceiling` workers gated by a controller-resized
	// limiter. -t sets the ceiling (a maximum, not a fixed count); the controller
	// still ramps up to it and backs off below it on rate-limit pressure. With no
	// -t the ceiling is adaptiveMax.
	ceiling := cfg.Threads
	var ctl *controller
	if ceiling > 0 {
		ctl = newFixedController(ceiling) // user pinned -t: hold it, don't ramp/sawtooth
	} else {
		ceiling = adaptiveMax
		ctl = newController(ceiling) // no -t: auto-tune up to adaptiveMax
	}
	go ctl.run(ctx, cfg.Verbose)
	go func() { <-ctx.Done(); ctl.lim.unblock() }()
	workers := ceiling

	items := make(chan item, workers*2)
	results := make(chan result, workers*2)

	var wg sync.WaitGroup
	kc := &keyCache{client: cfg.Client}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker(ctx, cfg, ctl, outPath, items, results)
		}()
	}
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writer(ctx, cfg, st.Live, out, outPath, state, kc, dec, results)
	}()

	feedErr := feed(ctx, cfg, st, refresh, state, items)
	close(items)
	wg.Wait()
	close(results)
	werr := <-writerDone

	if feedErr != nil && !errors.Is(feedErr, context.Canceled) {
		return feedErr
	}
	if werr != nil {
		return werr
	}
	if err := ctx.Err(); err != nil && cfg.LiveLimit == 0 {
		return err
	}
	os.Remove(statePath(outPath))
	return nil
}

// ---- resume state ----

// resumeState is shared between the feeder (reads NextIdx) and the writer
// (advances NextIdx, owns Offset), so NextIdx is atomic. Offset is written only
// by the writer and read only within writer-goroutine saves.
type resumeState struct {
	NextIdx atomic.Int64
	Offset  int64
	mu      sync.Mutex // serializes save()
}

// resumeSnapshot is the on-disk JSON form.
type resumeSnapshot struct {
	NextIdx int64 `json:"next_idx"`
	Offset  int64 `json:"offset"`
}

func statePath(outPath string) string { return outPath + ".m314dl-state" }

func openOutput(outPath string) (*resumeState, *os.File, error) {
	st := &resumeState{}
	if b, err := os.ReadFile(statePath(outPath)); err == nil {
		var snap resumeSnapshot
		if json.Unmarshal(b, &snap) == nil {
			st.NextIdx.Store(snap.NextIdx)
			st.Offset = snap.Offset
		}
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	if fi, err := f.Stat(); err == nil && fi.Size() < st.Offset {
		st.NextIdx.Store(0) // state file lies; start over
		st.Offset = 0
	}
	if err := f.Truncate(st.Offset); err != nil {
		return nil, nil, err
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return nil, nil, err
	}
	return st, f, nil
}

// save persists the resume checkpoint atomically (write-tmp + rename), so a
// process killed without warning mid-write can never leave a torn state file.
func (s *resumeState) save(outPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(resumeSnapshot{NextIdx: s.NextIdx.Load(), Offset: s.Offset})
	tmp := statePath(outPath) + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, statePath(outPath))
	}
}

// ---- feeder ----

func feed(ctx context.Context, cfg Config, st *manifest.Stream, refresh RefreshFunc, state *resumeState, items chan<- item) error {
	var idx int64
	seen := map[string]bool{}
	lastInit := ""
	deadline := time.Time{}
	if cfg.LiveLimit > 0 {
		deadline = time.Now().Add(cfg.LiveLimit)
	}

	emit := func(it item) bool {
		if it.idx < state.NextIdx.Load() {
			return true // already written in a previous run
		}
		select {
		case items <- it:
			return true
		case <-ctx.Done():
			return false
		}
	}

	push := func(cur *manifest.Stream) (int, bool) {
		// Build the whole batch first (a cheap in-memory walk), publish the
		// total, then emit. Emitting blocks on the worker channel at download
		// speed, so counting there would inflate the denominator over the run
		// and slide the percentage backwards. Counting up front makes it final
		// before the first progress tick. Live grows per refresh, but live shows
		// elapsed/count, not a percentage, so there is no bar to run backwards.
		fresh := 0
		var batch []item
		for i := range cur.Segments {
			seg := &cur.Segments[i]
			key := seg.URL + rangeKey(seg.Range)
			if seen[key] {
				continue
			}
			seen[key] = true
			if skipAd(cfg.AdFilters, seg.URL) {
				cfg.Verbose("ad-skip: %s", seg.URL)
				continue
			}
			fresh++
			if init := segInit(seg, cur); init != nil && init.URL != lastInit {
				lastInit = init.URL
				batch = append(batch, item{idx: idx, isInit: true, url: init.URL, rng: init.Range})
				idx++
			}
			batch = append(batch, item{idx: idx, seg: seg, url: seg.URL, rng: seg.Range, key: seg.Key, seq: seg.Seq})
			idx++
		}
		if cfg.Progress != nil {
			cfg.Progress.AddTotal(int64(len(batch)))
		}
		for i := range batch {
			if !emit(batch[i]) {
				return fresh, false
			}
		}
		return fresh, true
	}

	if st.Live && refresh != nil && !cfg.FromStart && state.NextIdx.Load() == 0 {
		// start at the live edge: mark the DVR backlog seen, keep last 3
		keep := 3
		if n := len(st.Segments); n > keep {
			for i := 0; i < n-keep; i++ {
				seg := &st.Segments[i]
				seen[seg.URL+rangeKey(seg.Range)] = true
			}
		}
	}
	if _, ok := push(st); !ok {
		return ctx.Err()
	}
	if !st.Live || refresh == nil {
		return nil
	}

	// live loop
	interval := st.Refresh
	if interval <= 0 {
		interval = 4 * time.Second
	}
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			cfg.Verbose("live: recording limit reached")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cfg.Stop:
			cfg.Verbose("live: stop requested, finishing up")
			return nil
		case <-time.After(interval):
		}
		cur, err := refresh(ctx)
		if err != nil {
			cfg.Verbose("live: refresh failed (will keep trying): %v", err)
			continue
		}
		if _, ok := push(cur); !ok {
			return ctx.Err()
		}
		if !cur.Live {
			cfg.Verbose("live: stream ended")
			return nil
		}
		if cur.Refresh > 0 {
			interval = cur.Refresh
		}
	}
}

func segInit(seg *manifest.Segment, st *manifest.Stream) *manifest.InitMap {
	if seg.Init != nil {
		return seg.Init
	}
	return st.Init
}

func rangeKey(r *manifest.ByteRange) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("@%d-%d", r.Start, r.End)
}

func skipAd(filters []*regexp.Regexp, u string) bool {
	for _, re := range filters {
		if re.MatchString(u) {
			return true
		}
	}
	return false
}

// ---- workers ----

type result struct {
	it       item
	err      error
	pressure int
}

// worker streams each segment to its part file. It does not decrypt or hold the
// segment in memory — the writer decrypts and assembles in order.
func worker(ctx context.Context, cfg Config, ctl *controller, outPath string, items <-chan item, results chan<- result) {
	for it := range items {
		if err := ctl.lim.acquire(ctx); err != nil {
			return
		}
		pressure, err := downloadSegment(ctx, cfg, ctl, it, partPath(outPath, it.idx))
		ctl.lim.release()
		if pressure > 0 {
			ctl.addPressure(pressure)
		}
		if err != nil {
			ctl.addErr()
		}
		select {
		case results <- result{it: it, err: err, pressure: pressure}:
		case <-ctx.Done():
			return
		}
	}
}

// decryptSegment applies the same in-place transforms the old inline path did,
// in the same order: strip fake image header, then CENC, then AES-128, then BBTS.
func decryptSegment(ctx context.Context, kc *keyCache, dec *cencDecryptor, bbtsKey []byte, it item, data []byte) ([]byte, error) {
	data = stripFakeImageHeader(data)
	if !it.isInit && dec != nil && it.key != nil && it.key.Method == manifest.EncCENC {
		if err := dec.decrypt(data); err != nil {
			return nil, fmt.Errorf("CENC decrypt: %w", err)
		}
	}
	if it.key != nil && it.key.Method == manifest.EncAES128 {
		key, err := kc.get(ctx, it.key.URI)
		if err != nil {
			return nil, fmt.Errorf("fetch AES key: %w", err)
		}
		iv := it.key.IV
		if iv == nil {
			iv = make([]byte, 16)
			binary.BigEndian.PutUint64(iv[8:], uint64(it.seq))
		}
		data, err = decryptAES128CBC(data, key, iv)
		if err != nil {
			return nil, err
		}
	}
	// BBTS: whole-segment iQiyi-style TS cipher. The key is supplied out of band;
	// the IV rides in each segment's own SDT. Media segments only (no init).
	if len(bbtsKey) > 0 && !it.isInit {
		out, err := bbts.Decrypt(data, bbtsKey)
		if err != nil {
			return nil, fmt.Errorf("BBTS decrypt: %w", err)
		}
		data = out
	}
	return data, nil
}

// stripFakeImageHeader removes fake PNG/JPEG/GIF/BMP prefixes some CDNs
// prepend to TS segments as crude obfuscation. The real TS/fMP4 payload
// starts at the first 0x47 sync byte or 'ftyp'/'styp'/'moof' box.
func stripFakeImageHeader(b []byte) []byte {
	if len(b) < 8 {
		return b
	}
	isPNG := b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G'
	isJPG := b[0] == 0xFF && b[1] == 0xD8
	isGIF := b[0] == 'G' && b[1] == 'I' && b[2] == 'F'
	isBMP := b[0] == 'B' && b[1] == 'M'
	if !isPNG && !isJPG && !isGIF && !isBMP {
		return b
	}
	// TS: scan for 3 consecutive sync bytes 188 apart
	for i := 0; i < len(b)-188*2-1; i++ {
		if b[i] == 0x47 && b[i+188] == 0x47 && b[i+376] == 0x47 {
			return b[i:]
		}
	}
	// fMP4: box header
	for i := 4; i < len(b)-8; i++ {
		tag := string(b[i : i+4])
		if tag == "ftyp" || tag == "styp" || tag == "moof" || tag == "sidx" {
			return b[i-4:]
		}
	}
	return b
}

// ---- AES-128 ----

type keyCache struct {
	client *httpx.Client
	mu     sync.Mutex
	keys   map[string][]byte
}

func (kc *keyCache) get(ctx context.Context, uri string) ([]byte, error) {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	if kc.keys == nil {
		kc.keys = map[string][]byte{}
	}
	if k, ok := kc.keys[uri]; ok {
		return k, nil
	}
	var key []byte
	if strings.HasPrefix(uri, "data:") {
		i := strings.Index(uri, ",")
		if i < 0 {
			return nil, fmt.Errorf("bad data: key URI")
		}
		payload := uri[i+1:]
		if strings.Contains(uri[:i], "base64") {
			b, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return nil, err
			}
			key = b
		} else {
			key = []byte(payload)
		}
	} else {
		b, _, err := kc.client.FetchBytes(ctx, uri, "")
		if err != nil {
			return nil, err
		}
		key = b
	}
	if len(key) != 16 {
		return nil, fmt.Errorf("AES-128 key is %d bytes, want 16", len(key))
	}
	kc.keys[uri] = key
	return key, nil
}

func decryptAES128CBC(data, key, iv []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted segment length %d not a multiple of 16", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	// PKCS7 unpad (tolerant: leave as-is when padding looks invalid)
	pad := int(out[len(out)-1])
	if pad >= 1 && pad <= 16 && pad <= len(out) {
		valid := true
		for _, p := range out[len(out)-pad:] {
			if int(p) != pad {
				valid = false
				break
			}
		}
		if valid {
			out = out[:len(out)-pad]
		}
	}
	return out, nil
}

// ---- ordered writer ----

// pend records a downloaded-but-not-yet-committed segment: either an item whose
// part file is on disk, or a live hole (failed segment, skipped).
type pend struct {
	it   item
	hole bool
}

func writer(ctx context.Context, cfg Config, live bool, out *os.File, outPath string, state *resumeState, kc *keyCache, dec *cencDecryptor, results <-chan result) error {
	pending := map[int64]pend{}
	var failed error

	// commit decrypts one segment's part file and appends it to the output.
	// ponytail: reads the whole part file into memory to decrypt (CENC/AES need
	// the full segment); peak memory is one segment, not one per worker. A
	// stream-copy fast path for clear segments could drop that to O(buffer) if a
	// pathological huge-clear-segment case ever shows up.
	commit := func(idx int64, p pend) error {
		if p.hole {
			return nil
		}
		part := partPath(outPath, idx)
		data, err := os.ReadFile(part)
		if err != nil {
			return fmt.Errorf("read segment %d: %w", idx, err)
		}
		data, err = decryptSegment(ctx, kc, dec, cfg.BBTSKey, p.it, data)
		if err != nil {
			return err
		}
		if _, err := out.Write(data); err != nil {
			return err
		}
		state.Offset += int64(len(data))
		os.Remove(part)
		return nil
	}

	for r := range results {
		if r.err != nil {
			if failed == nil {
				failed = fmt.Errorf("segment %d (%s): %w", r.it.idx, r.it.url, r.err)
			}
			cfg.Verbose("segment failed: %s: %v", r.it.url, r.err)
			if live {
				// live: the segment is gone; hole keeps the recording moving
				pending[r.it.idx] = pend{hole: true}
			}
			// VOD: no hole — the resume index must not advance past a failed
			// segment, or resume would silently skip it
			continue
		}
		pending[r.it.idx] = pend{it: r.it}
		for {
			idx := state.NextIdx.Load()
			p, ok := pending[idx]
			if !ok {
				break
			}
			delete(pending, idx)
			if err := commit(idx, p); err != nil {
				return err
			}
			next := state.NextIdx.Add(1)
			if cfg.Progress != nil {
				cfg.Progress.AddDone(1)
			}
			// Checkpoint on every commit. This process may be killed without
			// warning (a raw-mode TUI turns Ctrl-C into a keypress, not a signal),
			// so the state must match what's on disk *before* the kill: a stale
			// offset would make openOutput truncate committed data and re-download
			// it. The uncommitted tail still resumes from its part files, so a kill
			// costs at most the one segment being written. The write is atomic and
			// tiny; fsync stays periodic (power-loss hardening).
			state.save(outPath)
			if next%20 == 0 {
				out.Sync()
			}
		}
	}
	out.Sync()
	state.save(outPath)
	// missing index means the segment failed to download
	if len(pending) > 0 && failed == nil {
		failed = fmt.Errorf("%d segments missing at end of stream", len(pending))
	}
	return failed
}

// ---- single big file ----

// downloadSingleFile streams one URL to disk with byte-offset resume.
func downloadSingleFile(ctx context.Context, cfg Config, st *manifest.Stream, outPath string) error {
	u := st.Segments[0].URL
	var offset int64
	if fi, err := os.Stat(outPath); err == nil {
		offset = fi.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return nil // already complete
	case resp.StatusCode == http.StatusOK:
		offset = 0 // server ignored Range; start over
	case resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode >= 400:
		return &httpx.StatusError{Code: resp.StatusCode, URL: u}
	}
	if cfg.Progress != nil {
		cfg.Progress.AddTotal(1)
		if resp.ContentLength > 0 {
			cfg.Progress.SetKnownBytes(offset + resp.ContentLength)
		}
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(offset); err != nil {
		return err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			if cfg.Progress != nil {
				cfg.Progress.AddBytes(int64(n))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read body: %w", rerr)
		}
	}
	if cfg.Progress != nil {
		cfg.Progress.AddDone(1)
	}
	return nil
}

// TempStreamPath returns the raw per-stream temp file path. The name is
// derived from the OUTPUT file, not just the stream id, so two concurrent
// downloads that share an output directory but write different files (e.g. two
// episodes of one series whose manifests reuse stream ids like "video=3000000"
// or "1") never collide on the same temp file. The path is stable across reruns
// of the same command, so resume still works.
func TempStreamPath(outPath string, st *manifest.Stream, ext string) string {
	dir := filepath.Dir(outPath)
	outBase := filepath.Base(strings.TrimSuffix(outPath, filepath.Ext(outPath)))
	name := fmt.Sprintf("%s.m314dl-%s-%s%s", outBase, st.Type, sanitize(st.ID), ext)
	return filepath.Join(dir, name)
}

var badChars = regexp.MustCompile(`[^\w.\-]+`)

func sanitize(s string) string {
	s = badChars.ReplaceAllString(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
