package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohamed/m314dl/internal/httpx"
	"github.com/mohamed/m314dl/internal/manifest"
)

// box builds one MP4 box: 4-byte size, 4-byte type, payload.
func box(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], typ)
	return append(out, body...)
}

// encryptedInit is a minimal init segment whose video sample entry is `encv`
// carrying a tenc box — the shape a packager emits for CENC content.
func encryptedInit(kid [16]byte) []byte {
	tenc := box("tenc", append([]byte{
		0, 0, 0, 0, // version 0, flags
		0, // reserved
		0, // reserved
		1, // is_protected
		8, // per-sample IV size
	}, kid[:]...))
	schi := box("schi", tenc)
	schm := box("schm", []byte{0, 0, 0, 0}, []byte("cenc"), []byte{0, 1, 0, 0})
	sinf := box("sinf", schm, schi)

	// encv: 6 reserved + 2 data_reference_index, then the 70-byte visual
	// sample entry body, then children.
	visual := make([]byte, 78)
	visual[7] = 1 // data_reference_index
	encv := box("encv", visual, sinf)

	stsd := box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, encv)
	return box("moov", box("trak", box("mdia", box("minf", box("stbl", stsd)))))
}

// A manifest that says nothing about encryption does not make a stream clear.
//
// Some packagers omit ContentProtection entirely. Judging the stream by the
// manifest alone means copying encrypted samples straight through: the player
// gets a black picture with working audio, and nothing anywhere reports a
// fault. The init segment is the authority, so the refusal must come from it.
func TestEncryptionIsDetectedFromTheInitNotTheManifest(t *testing.T) {
	kid := [16]byte{0xab, 0xcd, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	initSeg := encryptedInit(kid)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "init.mp4") {
			w.Write(initSeg)
			return
		}
		w.Write([]byte("segment"))
	}))
	defer srv.Close()

	// Note: no Key on any segment. The manifest claims a clear stream.
	st := &manifest.Stream{
		ID:   "video",
		Init: &manifest.InitMap{URL: srv.URL + "/init.mp4"},
		Segments: []manifest.Segment{
			{URL: srv.URL + "/seg0", Seq: 0, Duration: 1},
		},
	}
	client, _ := httpx.New(httpx.Options{Retries: 1})
	err := DownloadStream(context.Background(), Config{Client: client},
		st, filepath.Join(t.TempDir(), "out.mp4"), nil)

	if err == nil {
		t.Fatal("an encrypted stream was accepted as clear; its samples would reach the player still encrypted")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("no key for stream")) {
		t.Fatalf("error = %v, want it to say no key is available", err)
	}
	// And it must name the KID, so an operator can go and fetch that key.
	if !strings.Contains(err.Error(), "abcd0102") {
		t.Fatalf("error = %v, want it to name the KID", err)
	}
}

// The same stream with its key runs, and is not refused.
func TestKnownKeyDecryptsAnInitDetectedStream(t *testing.T) {
	kid := [16]byte{0xab, 0xcd, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	initSeg := encryptedInit(kid)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "init.mp4") {
			w.Write(initSeg)
			return
		}
		w.Write(box("moof", box("mfhd", []byte{0, 0, 0, 0, 0, 0, 0, 1})))
	}))
	defer srv.Close()

	st := &manifest.Stream{
		ID:       "video",
		Init:     &manifest.InitMap{URL: srv.URL + "/init.mp4"},
		Segments: []manifest.Segment{{URL: srv.URL + "/seg0", Seq: 0, Duration: 1}},
	}
	client, _ := httpx.New(httpx.Options{Retries: 1})
	key := make([]byte, 16)
	err := DownloadStream(context.Background(),
		Config{Client: client, Keys: map[[16]byte][]byte{kid: key}},
		st, filepath.Join(t.TempDir(), "out.mp4"), nil)
	if err != nil && strings.Contains(err.Error(), "no key for stream") {
		t.Fatalf("the supplied key was not used: %v", err)
	}
}

// cbcsInit is a minimal init whose sample entry declares the cbcs scheme
// (constant IV, 1:9 pattern) — the shape an fMP4 HLS SAMPLE-AES stream uses.
func cbcsInit(kid [16]byte, constIV []byte) []byte {
	tenc := box("tenc", append(append([]byte{
		1, 0, 0, 0, // version 1 (pattern present), flags
		0,          // reserved
		(1 << 4) | 9, // crypt_byte_block=1, skip_byte_block=9
		1,          // is_protected
		0,          // per_sample_IV_size = 0 -> constant IV
	}, kid[:]...), append([]byte{byte(len(constIV))}, constIV...)...))
	schi := box("schi", tenc)
	schm := box("schm", []byte{0, 0, 0, 0}, []byte("cbcs"), []byte{0, 1, 0, 0})
	sinf := box("sinf", schm, schi)
	visual := make([]byte, 78)
	visual[7] = 1
	encv := box("encv", visual, sinf)
	stsd := box("stsd", []byte{0, 0, 0, 0, 0, 0, 0, 1}, encv)
	return box("moov", box("trak", box("mdia", box("minf", box("stbl", stsd)))))
}

// An fMP4 stream flagged SAMPLE-AES (== cbcs) with a supplied key is routed
// through the CENC/cbcs decryptor, not refused.
func TestSampleAESfMP4RoutesToCBCS(t *testing.T) {
	kid := [16]byte{0x11, 0x22, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	constIV := bytes.Repeat([]byte{0x42}, 16)
	initSeg := cbcsInit(kid, constIV)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "init.mp4") {
			w.Write(initSeg)
			return
		}
		w.Write(box("moof", box("mfhd", []byte{0, 0, 0, 0, 0, 0, 0, 1})))
	}))
	defer srv.Close()

	st := &manifest.Stream{
		ID:   "video",
		Init: &manifest.InitMap{URL: srv.URL + "/init.mp4"},
		Segments: []manifest.Segment{{
			URL: srv.URL + "/seg0", Seq: 0, Duration: 1,
			Key: &manifest.Key{Method: manifest.EncSampleAES, URI: "skd://x"},
		}},
	}
	client, _ := httpx.New(httpx.Options{Retries: 1})
	var zero [16]byte
	err := DownloadStream(context.Background(),
		Config{Client: client, Keys: map[[16]byte][]byte{zero: make([]byte, 16)}},
		st, filepath.Join(t.TempDir(), "out.mp4"), nil)
	if err != nil {
		if strings.Contains(err.Error(), "not supported") || strings.Contains(err.Error(), "no key") {
			t.Fatalf("fMP4 SAMPLE-AES was not routed to the cbcs path: %v", err)
		}
		t.Fatalf("unexpected error: %v", err)
	}
}
