package mp4

import "encoding/binary"

// After DecryptFragment turns the samples into plaintext, the boxes still
// declare the stream encrypted — the init types its samples encv/enca and
// carries sinf/tenc/pssh, and each fragment carries senc/saiz/saio. A player
// (or ffmpeg) that trusts those tries to "decrypt" the plaintext and gets a
// black picture. SanitizeInit and StripFragmentProtection remove that metadata
// so the output is a clean, unprotected stream — the same thing mp4decrypt does.

// emitBox wraps payload in a 32-bit box header. Init and fragment boxes are far
// below 4 GiB, so the largesize form is never needed here.
func emitBox(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	copy(out[8:], payload)
	return out
}

// rebuildSeq re-emits the box sequence in buf, letting fn transform each box.
// fn returns (nil,false) to keep the box verbatim, (bytes,true) to replace it,
// or (nil,true) to drop it. Parent sizes are implicitly recomputed because each
// container is re-wrapped by its caller with emitBox. On any parse error the
// original bytes are returned unchanged, never a truncation.
func rebuildSeq(buf []byte, fn func(b box) ([]byte, bool)) []byte {
	out := make([]byte, 0, len(buf))
	if err := walk(buf, func(b box) bool {
		if repl, ok := fn(b); ok {
			out = append(out, repl...) // repl may be nil → box dropped
		} else {
			out = append(out, buf[b.off:b.off+b.hdrLen+int64(len(b.payload))]...)
		}
		return true
	}); err != nil {
		return buf
	}
	return out
}

// StripFragmentProtection removes senc/saiz/saio from every traf, so a fragment
// whose samples are already decrypted is no longer flagged as encrypted.
func StripFragmentProtection(frag []byte) []byte {
	return rebuildSeq(frag, func(b box) ([]byte, bool) {
		if b.typ != "moof" {
			return nil, false
		}
		return emitBox("moof", rebuildSeq(b.payload, func(c box) ([]byte, bool) {
			if c.typ != "traf" {
				return nil, false
			}
			return emitBox("traf", rebuildSeq(c.payload, func(d box) ([]byte, bool) {
				switch d.typ {
				case "senc", "saiz", "saio":
					return nil, true // drop
				}
				return nil, false
			})), true
		})), true
	})
}

// SanitizeInit de-protects an init segment: each encv/enca sample entry becomes
// its original codec (the fourcc in sinf's frma box) with the sinf removed, and
// pssh boxes are dropped. Everything else passes through untouched.
func SanitizeInit(init []byte) []byte {
	return rebuildSeq(init, func(b box) ([]byte, bool) {
		if b.typ != "moov" {
			return nil, false
		}
		return emitBox("moov", rebuildSeq(b.payload, func(c box) ([]byte, bool) {
			switch c.typ {
			case "pssh":
				return nil, true // drop the protection system header
			case "trak":
				return emitBox("trak", sanitizeTrak(c.payload)), true
			}
			return nil, false
		})), true
	})
}

func sanitizeTrak(trak []byte) []byte {
	return rebuildSeq(trak, func(b box) ([]byte, bool) {
		if b.typ != "mdia" {
			return nil, false
		}
		return emitBox("mdia", rebuildSeq(b.payload, func(c box) ([]byte, bool) {
			if c.typ != "minf" {
				return nil, false
			}
			return emitBox("minf", rebuildSeq(c.payload, func(d box) ([]byte, bool) {
				if d.typ != "stbl" {
					return nil, false
				}
				return emitBox("stbl", rebuildSeq(d.payload, func(e box) ([]byte, bool) {
					if e.typ != "stsd" {
						return nil, false
					}
					return sanitizeStsd(e.payload), true
				})), true
			})), true
		})), true
	})
}

// sanitizeStsd de-protects the sample entries. stsd is a FullBox: version+flags
// (4) and entry_count (4) precede the entries.
func sanitizeStsd(stsd []byte) []byte {
	if len(stsd) < 8 {
		return emitBox("stsd", stsd)
	}
	prefix := stsd[:8]
	entries := rebuildSeq(stsd[8:], func(b box) ([]byte, bool) {
		if b.typ != "encv" && b.typ != "enca" {
			return nil, false
		}
		return deprotectSampleEntry(b), true
	})
	return emitBox("stsd", append(append([]byte(nil), prefix...), entries...))
}

// deprotectSampleEntry rewrites an encv/enca entry to its original codec (from
// sinf→frma) with the sinf box removed. If the frma can't be found it leaves the
// entry protected rather than guessing a codec.
func deprotectSampleEntry(b box) []byte {
	children := sampleEntryChildren(b)
	if children == nil {
		return emitBox(b.typ, b.payload)
	}
	prefix := b.payload[:len(b.payload)-len(children)]

	orig := ""
	newChildren := rebuildSeq(children, func(c box) ([]byte, bool) {
		if c.typ != "sinf" {
			return nil, false
		}
		if f, ok := find(c.payload, "frma"); ok && len(f.payload) >= 4 {
			orig = string(f.payload[:4])
		}
		return nil, true // drop sinf
	})
	if orig == "" {
		return emitBox(b.typ, b.payload) // no frma — don't risk mislabeling
	}
	return emitBox(orig, append(append([]byte(nil), prefix...), newChildren...))
}
