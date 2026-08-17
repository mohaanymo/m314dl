package engine

import (
	"context"
	"fmt"

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

// newCencDecryptor fetches and parses the stream's init segment, then resolves
// the content key. Key resolution order: the tenc default_KID, then the
// manifest KID, then a single bare key if exactly one was supplied.
func newCencDecryptor(ctx context.Context, cfg Config, st *manifest.Stream) (*cencDecryptor, error) {
	if st.Init == nil || st.Init.URL == "" {
		return nil, fmt.Errorf("stream %s is CENC but has no init segment to read protection info from", st.ID)
	}
	rng := ""
	if st.Init.Range != nil {
		rng = st.Init.Range.Header()
	}
	initSeg, _, err := cfg.Client.FetchBytes(ctx, st.Init.URL, rng)
	if err != nil {
		return nil, fmt.Errorf("fetch init for %s: %w", st.ID, err)
	}
	info, err := mp4.ParseInit(initSeg)
	if err != nil {
		return nil, fmt.Errorf("parse init for %s: %w", st.ID, err)
	}
	if info == nil {
		return nil, fmt.Errorf("stream %s is CENC but its init has no tenc protection box", st.ID)
	}

	key := resolveKey(cfg.Keys, info.DefaultKID, manifestKID(st))
	if key == nil {
		return nil, fmt.Errorf("no key for stream %s (KID %x); supply -key %x:<hex-key>",
			st.ID, info.DefaultKID, info.DefaultKID)
	}
	return &cencDecryptor{info: info, key: key}, nil
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
