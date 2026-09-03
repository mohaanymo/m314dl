package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/mohamed/m314dl/internal/manifest"
	"github.com/mohamed/m314dl/internal/mp4"
)

// cencDecryptor holds the protection metadata and content key for one stream.
// It is built once per stream (init segment parsed once) and shared read-only
// across all download workers.
type cencDecryptor struct {
	info *mp4.InitInfo
	key  []byte
}

// decrypt applies native CENC/CBCS decryption to a fragment in place.
func (d *cencDecryptor) decrypt(frag []byte) error {
	return mp4.DecryptFragment(frag, d.info, d.key)
}

// streamIsCENC reports whether any segment uses CENC protection.
func streamIsCENC(st *manifest.Stream) bool {
	for i := range st.Segments {
		if k := st.Segments[i].Key; k != nil && k.Method == manifest.EncCENC {
			return true
		}
	}
	return false
}

// fetchInit downloads the stream's init segment (the range slice of it, for a
// SegmentBase-style init that shares the media file).
func fetchInit(ctx context.Context, cfg Config, st *manifest.Stream) ([]byte, error) {
	rng := ""
	if st.Init.Range != nil {
		rng = st.Init.Range.Header()
	}
	initSeg, _, err := cfg.Client.FetchBytes(ctx, st.Init.URL, rng)
	if err != nil {
		return nil, fmt.Errorf("fetch init for %s: %w", st.ID, err)
	}
	return initSeg, nil
}

// newCencDecryptor parses the stream's init segment, then resolves the content
// key. Key resolution order: the tenc default_KID, then the manifest's KID,
// then a single unlabelled key.
//
// It returns (nil, nil) when the init segment shows the stream is not
// encrypted, so the caller can use it to ask the question as well as to answer
// it. The init is the authority: a manifest can omit ContentProtection while
// the samples are plainly encrypted, and trusting the manifest alone means
// passing encrypted samples through untouched — which plays as a black picture
// with working audio, and reports no error anywhere.
func newCencDecryptor(ctx context.Context, cfg Config, st *manifest.Stream, initSeg []byte) (*cencDecryptor, error) {
	info, err := mp4.ParseInit(initSeg)
	if err != nil {
		return nil, fmt.Errorf("parse init for %s: %w", st.ID, err)
	}
	if info == nil {
		// No tenc box. If the manifest claimed protection, that is a
		// contradiction worth reporting; otherwise the stream is simply clear.
		if streamIsCENC(st) {
			return nil, fmt.Errorf("stream %s is CENC but its init has no tenc protection box", st.ID)
		}
		return nil, nil
	}

	key := resolveKey(cfg.Keys, info.DefaultKID, manifestKID(st))
	if key == nil {
		// SAMPLE-AES (cbcs) may deliver a raw key through a fetchable URI, like
		// AES-128, rather than being supplied with -key.
		if uri := firstFetchableSampleAESURI(st); uri != "" {
			kc := &keyCache{client: cfg.Client}
			k, err := kc.get(ctx, uri)
			if err != nil {
				return nil, fmt.Errorf("fetch SAMPLE-AES key for %s: %w", st.ID, err)
			}
			key = k
		}
	}
	if key == nil {
		return nil, fmt.Errorf("no key for stream %s (KID %x); supply -key %x:<hex-key>",
			st.ID, info.DefaultKID, info.DefaultKID)
	}
	return &cencDecryptor{info: info, key: key}, nil
}

// firstSampleAESKey returns the first SAMPLE-AES key in the stream, or nil.
func firstSampleAESKey(st *manifest.Stream) *manifest.Key {
	for i := range st.Segments {
		if k := st.Segments[i].Key; k != nil && k.Method == manifest.EncSampleAES {
			return k
		}
	}
	return nil
}

// firstFetchableSampleAESURI returns the first SAMPLE-AES key URI that can be
// fetched (http/https/data), or "".
func firstFetchableSampleAESURI(st *manifest.Stream) string {
	if k := firstSampleAESKey(st); k != nil && fetchableKeyURI(k.URI) {
		return k.URI
	}
	return ""
}

// fetchableKeyURI reports whether a key URI points at bytes we can retrieve
// (as opposed to skd:// or an empty URI, whose key must come from -key).
func fetchableKeyURI(uri string) bool {
	return strings.HasPrefix(uri, "http://") ||
		strings.HasPrefix(uri, "https://") ||
		strings.HasPrefix(uri, "data:")
}

// sampleAESKey resolves the content key for a transport-stream SAMPLE-AES
// segment: a fetchable URI (like AES-128) if present, else the user-supplied
// -key (matched by KID, or the single bare key).
func sampleAESKey(ctx context.Context, kc *keyCache, keys map[[16]byte][]byte, k *manifest.Key) ([]byte, error) {
	if fetchableKeyURI(k.URI) {
		return kc.get(ctx, k.URI)
	}
	if key := resolveKey(keys, k.KID); key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("no key for SAMPLE-AES segment; supply the content key with -key <hex-key>")
}

// resolveKey looks up a content key by any known KID, falling back to the sole
// key when exactly one was supplied without a KID match.
func resolveKey(keys map[[16]byte][]byte, kids ...[16]byte) []byte {
	for _, kid := range kids {
		if k, ok := keys[kid]; ok {
			return k
		}
	}
	// single-key convenience: one key, no KID given → use it regardless of KID
	var zero [16]byte
	if k, ok := keys[zero]; ok && len(keys) == 1 {
		return k
	}
	return nil
}

func manifestKID(st *manifest.Stream) [16]byte {
	for i := range st.Segments {
		if k := st.Segments[i].Key; k != nil && k.KID != ([16]byte{}) {
			return k.KID
		}
	}
	return [16]byte{}
}
