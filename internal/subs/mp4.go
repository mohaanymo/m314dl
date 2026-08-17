package subs

import (
	"encoding/binary"
	"strings"
)

// ExtractMdat walks top-level MP4 boxes and returns all mdat payloads
// concatenated. For stpp tracks the payloads are whole TTML documents, which
// parseTTML consumes directly — no ffmpeg needed.
func ExtractMdat(b []byte) []byte {
	var out []byte
	for i := 0; i+8 <= len(b); {
		size := int64(binary.BigEndian.Uint32(b[i : i+4]))
		typ := string(b[i+4 : i+8])
		hdr := int64(8)
		if size == 1 {
			if i+16 > len(b) {
				break
			}
			size = int64(binary.BigEndian.Uint64(b[i+8 : i+16]))
			hdr = 16
		} else if size == 0 {
			size = int64(len(b) - i) // box extends to EOF
		}
		if size < hdr || int64(i)+size > int64(len(b)) {
			break
		}
		if typ == "mdat" {
			out = append(out, b[int64(i)+hdr:int64(i)+size]...)
		}
		i += int(size)
	}
	return out
}

// IsTTMLPayload reports whether extracted mdat data looks like TTML.
func IsTTMLPayload(b []byte) bool {
	head := string(b[:min(2048, len(b))])
	return strings.Contains(head, "<tt") || strings.Contains(head, "<?xml")
}
