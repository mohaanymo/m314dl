// Package mp4 is a minimal ISO base media file format (ISO/IEC 14496-12)
// reader, just enough to parse CENC protection metadata and decrypt
// fragmented MP4 samples in-process — no mp4decrypt, no shaka-packager.
package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// box is a parsed ISO-BMFF box header plus its raw payload (the bytes after
// the header, still including any child boxes for containers).
type box struct {
	typ     string
	off     int64  // offset of the box (its size field) within the parsed buffer
	hdrLen  int64  // size of size+type(+largesize) header
	payload []byte // bytes after the header, length = size - hdrLen
}

// containers holds box types whose payload is itself a sequence of boxes.
var containers = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"moof": true, "traf": true, "mvex": true, "edts": true, "dinf": true,
	"stsd": true, "sinf": true, "schi": true, "encv": true, "enca": true,
	"mp4a": true, "mp4v": true, "avc1": true, "hev1": true, "hvc1": true,
	"udta": true, "meta": true,
}

// Sample-entry prefix lengths before child boxes begin: 8-byte SampleEntry
// base plus the codec-specific block. Visual = 70, audio v0 = 20, audio v1
// = 36. We try each and validate by exact box tiling, so a wrong guess is
// rejected rather than mis-parsed.
var sampleEntrySkips = []int{8 + 70, 8 + 20, 8 + 36, 8 + 28, 8}

// walk calls fn for every top-level box in buf. Return false from fn to stop.
func walk(buf []byte, fn func(b box) bool) error {
	var off int64
	for off < int64(len(buf)) {
		b, next, err := readBox(buf, off)
		if err != nil {
			return err
		}
		if !fn(b) {
			return nil
		}
		off = next
	}
	return nil
}

// readBox parses one box header at off and returns the box plus the offset of
// the following box.
func readBox(buf []byte, off int64) (box, int64, error) {
	if off+8 > int64(len(buf)) {
		return box{}, 0, errors.New("mp4: truncated box header")
	}
	size := int64(binary.BigEndian.Uint32(buf[off:]))
	typ := string(buf[off+4 : off+8])
	hdr := int64(8)
	switch size {
	case 1: // 64-bit largesize
		if off+16 > int64(len(buf)) {
			return box{}, 0, errors.New("mp4: truncated largesize")
		}
		size = int64(binary.BigEndian.Uint64(buf[off+8:]))
		hdr = 16
	case 0: // extends to end of buffer
		size = int64(len(buf)) - off
	}
	if size < hdr || off+size > int64(len(buf)) {
		return box{}, 0, fmt.Errorf("mp4: box %q size %d out of range at %d", typ, size, off)
	}
	return box{typ: typ, off: off, hdrLen: hdr, payload: buf[off+hdr : off+size]}, off + size, nil
}

// children walks the child boxes of a container box's payload.
func (b box) children(fn func(c box) bool) error {
	return walk(b.payload, fn)
}

// find returns the first descendant box matching the given type path.
// find(buf, "moov","trak","mdia") descends container by container.
func find(buf []byte, path ...string) (box, bool) {
	cur := buf
	var found box
	for depth, want := range path {
		got := false
		walk(cur, func(b box) bool {
			if b.typ == want {
				found = b
				got = true
				return false
			}
			return true
		})
		if !got {
			return box{}, false
		}
		if depth == len(path)-1 {
			return found, true
		}
		cur = found.payload
	}
	return box{}, false
}

// fullBox reads the version+flags of a FullBox payload.
func fullBox(payload []byte) (version byte, flags uint32, rest []byte, err error) {
	if len(payload) < 4 {
		return 0, 0, nil, errors.New("mp4: truncated fullbox")
	}
	version = payload[0]
	flags = uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	return version, flags, payload[4:], nil
}

// sampleEntryChildren finds child boxes (e.g. sinf) inside a sample entry box
// (encv/enca) by skipping the fixed SampleEntry prefix. The prefix length
// differs between visual and audio entries, so we scan forward for the first
// plausible box start instead of hardcoding.
func sampleEntryChildren(b box) []byte {
	p := b.payload
	for _, skip := range sampleEntrySkips {
		if skip <= len(p) && tilesExactly(p[skip:]) && hasChild(p[skip:], "sinf") {
			return p[skip:]
		}
	}
	// fall back to the first exact tiling even without sinf (non-encrypted use)
	for _, skip := range sampleEntrySkips {
		if skip <= len(p) && tilesExactly(p[skip:]) {
			return p[skip:]
		}
	}
	return nil
}

func hasChild(buf []byte, typ string) bool {
	found := false
	walk(buf, func(b box) bool {
		if b.typ == typ {
			found = true
			return false
		}
		return true
	})
	return found
}

// tilesExactly reports whether buf is a sequence of well-formed boxes that
// exactly fills buf with no leftover bytes. A strong signal that buf starts at
// a real box boundary. Empty buf tiles trivially.
func tilesExactly(buf []byte) bool {
	off := 0
	for off < len(buf) {
		if off+8 > len(buf) {
			return false
		}
		size := int(binary.BigEndian.Uint32(buf[off:]))
		typ := string(buf[off+4 : off+8])
		if size == 1 {
			if off+16 > len(buf) {
				return false
			}
			size = int(binary.BigEndian.Uint64(buf[off+8:]))
		}
		if size < 8 || off+size > len(buf) || !isBoxType(typ) {
			return false
		}
		off += size
	}
	return off == len(buf)
}

func isBoxType(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ') {
			return false
		}
	}
	return true
}
