package sampleaes

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"testing"
)

var testKey = []byte("0123456789abcdef")
var testIV = []byte("ABCDEFGHIJKLMNOP")

func newBlock(t *testing.T) cipher.Block {
	t.Helper()
	b, err := aes.NewCipher(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// applyEP re-inserts H.264 emulation-prevention bytes (the inverse of the
// package's discardEP): a 0x03 is added before any byte <= 0x03 that follows two
// consecutive zero bytes. Used only by the test encryptor — production
// decryption never re-escapes.
func applyEP(data []byte) []byte {
	out := make([]byte, 0, len(data)+len(data)/50+4)
	zeros := 0
	for _, b := range data {
		if zeros >= 2 && b <= 0x03 {
			out = append(out, 0x03)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// ---- spec-faithful encryptors (mirror the decryptors) ----

func encryptAAC(frame []byte, block cipher.Block, iv []byte) {
	hdr := 9
	if frame[1]&0x01 == 1 {
		hdr = 7
	}
	raw := len(frame) - hdr
	if raw <= aacLeader {
		return
	}
	start := hdr + aacLeader
	end := hdr + (raw &^ 15)
	if end <= start {
		return
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(frame[start:end], frame[start:end])
}

// encryptAVCNAL mirrors a SAMPLE-AES encoder (e.g. Bento4's mp42hls): it
// encrypts the 1-in-10 block pattern in place over the clear NAL, then applies
// start-code emulation prevention over the already-encrypted NAL. The decoder
// removes that one EP layer and decrypts, recovering the original NAL exactly —
// no re-escaping. (Verified against real Bento4-encrypted streams.)
func encryptAVCNAL(nal []byte, block cipher.Block, iv []byte) []byte {
	if len(nal) == 0 {
		return nal
	}
	t := nal[0] & 0x1f
	if (t != 1 && t != 5) || len(nal) <= avcMinNAL {
		return nal
	}
	buf := append([]byte(nil), nal...)
	enc := cipher.NewCBCEncrypter(block, iv)
	data, rem := avcLeader, len(buf)-avcLeader
	for rem > 0 {
		if rem > 16 {
			enc.CryptBlocks(buf[data:data+16], buf[data:data+16])
			data += 16
			rem -= 16
		}
		skip := 144
		if rem < skip {
			skip = rem
		}
		data += skip
		rem -= skip
	}
	return applyEP(buf)
}

// canonicalNAL builds a canonically emulation-prevented NAL from a raw body.
func canonicalNAL(nalType byte, body []byte) []byte {
	return applyEP(append([]byte{nalType & 0x1f}, body...))
}

// deterministic pseudo-random body with embedded start-code-like patterns.
func body(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*37 + 11)
	}
	// sprinkle 00 00 0x runs to exercise emulation prevention on both sides.
	for _, p := range []int{40, 41, 42, 70, 71, 72, 100, 101, 102} {
		if p+2 < n {
			b[p], b[p+1], b[p+2] = 0, 0, byte(p%4)
		}
	}
	return b
}

// ---- pure-function round trips ----

func TestAVCNALRoundTrip(t *testing.T) {
	block := newBlock(t)
	for _, tc := range []struct {
		name string
		typ  byte
		n    int
	}{
		{"idr-large", 5, 4000},
		{"nonidr-large", 1, 800},
		{"just-over-48", 1, 60},
		{"exactly-49-raw", 1, 49},
		{"one-period-plus-tail", 1, 200},
		{"trailing-partial", 1, 160 + 32 + 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nal := canonicalNAL(tc.typ, body(tc.n))
			enc := encryptAVCNAL(append([]byte(nil), nal...), block, testIV)
			dec := decryptAVCNAL(append([]byte(nil), enc...), block, testIV)
			if !bytes.Equal(dec, nal) {
				t.Fatalf("round trip mismatch: got %d bytes, want %d", len(dec), len(nal))
			}
			// something must actually have been encrypted for large NALs.
			if tc.n >= 200 && bytes.Equal(enc, nal) {
				t.Fatalf("nal was not encrypted")
			}
		})
	}
}

func TestAVCNALNotEncrypted(t *testing.T) {
	block := newBlock(t)
	// too small (<=48) and non-VCL types pass through untouched.
	for _, tc := range []struct {
		typ byte
		n   int
	}{
		{1, 30}, {5, 10}, {7, 400}, {8, 400}, {9, 400}, // <=48 (types 1/5) or non-VCL
	} {
		nal := canonicalNAL(tc.typ, body(tc.n))
		if (tc.typ == 1 || tc.typ == 5) && len(nal) > avcMinNAL {
			t.Fatalf("test setup: NAL escaped length %d exceeds threshold", len(nal))
		}
		enc := encryptAVCNAL(append([]byte(nil), nal...), block, testIV)
		if !bytes.Equal(enc, nal) {
			t.Fatalf("type %d len %d should not be encrypted", tc.typ, len(nal))
		}
		dec := decryptAVCNAL(append([]byte(nil), nal...), block, testIV)
		if !bytes.Equal(dec, nal) {
			t.Fatalf("type %d len %d decrypt changed clear data", tc.typ, len(nal))
		}
	}
}

func makeADTS(payloadLen int, crc bool) []byte {
	hdr := 7
	if crc {
		hdr = 9
	}
	total := hdr + payloadLen
	f := make([]byte, total)
	f[0] = 0xff
	f[1] = 0xf0 // syncword; MPEG-4, Layer 0
	if !crc {
		f[1] |= 0x01 // protection_absent
	}
	f[2] = 0x50
	// aac_frame_length (13 bits) across f[3..5]
	f[3] = byte(0x00 | (total>>11)&0x03)
	f[4] = byte((total >> 3) & 0xff)
	f[5] = byte((total & 0x07) << 5)
	f[6] = 0xfc
	for i := hdr; i < total; i++ {
		f[i] = byte(i*29 + 7)
	}
	return f
}

func TestAACRoundTrip(t *testing.T) {
	block := newBlock(t)
	for _, tc := range []struct {
		name    string
		payload int
		crc     bool
	}{
		{"empty-payload", 0, false},
		{"leader-only", 16, false},
		{"under-one-block", 31, false},
		{"exactly-one-block", 32, false},
		{"one-block-plus-tail", 47, false},
		{"many-blocks", 1000, false},
		{"with-crc-header", 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := makeADTS(tc.payload, tc.crc)
			orig := append([]byte(nil), frame...)
			enc := append([]byte(nil), frame...)
			encryptAAC(enc, block, testIV)
			decryptAAC(enc, block, testIV)
			if !bytes.Equal(enc, orig) {
				t.Fatalf("aac round trip mismatch (payload=%d crc=%v)", tc.payload, tc.crc)
			}
			// header + 16-byte leader must never change.
			hdr := 7
			if tc.crc {
				hdr = 9
			}
			lead := hdr + 16
			if lead <= len(orig) {
				enc2 := append([]byte(nil), orig...)
				encryptAAC(enc2, block, testIV)
				if !bytes.Equal(enc2[:min(lead, len(enc2))], orig[:min(lead, len(orig))]) {
					t.Fatalf("aac leader was modified")
				}
			}
		})
	}
}

// ---- full transport-stream integration ----

const (
	patPID   uint16 = 0x0000
	pmtPIDv  uint16 = 0x1000
	videoPID uint16 = 0x0100
	audioPID uint16 = 0x0101
)

func tsPacket(pid uint16, pusiBit bool, cc byte, payload []byte) []byte {
	p := make([]byte, pktLen)
	for i := range p {
		p[i] = 0xff
	}
	p[0] = sync
	p[1] = byte(pid>>8) & 0x1f
	if pusiBit {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	if len(payload) >= 184 {
		p[3] = 0x10 | (cc & 0x0f)
		copy(p[4:], payload[:184])
	} else {
		// adaptation stuffing to fill the packet
		p[3] = 0x30 | (cc & 0x0f)
		afLen := 184 - 1 - len(payload)
		p[4] = byte(afLen)
		if afLen >= 1 {
			p[5] = 0x00
		}
		copy(p[pktLen-len(payload):], payload)
	}
	return p
}

func buildPAT(cc byte) []byte {
	sec := []byte{0x00, 0xb0, 0x0d, 0x00, 0x01, 0xc1, 0x00, 0x00,
		0x00, 0x01, // program_number 1
		byte(0xe0 | (pmtPIDv>>8)&0x1f), byte(pmtPIDv & 0xff)}
	crc := mpegCRC(sec)
	sec = binary.BigEndian.AppendUint32(sec, crc)
	return tsPacket(patPID, true, cc, append([]byte{0x00}, sec...))
}

func buildPMT(cc byte, vType, aType byte) []byte {
	// section body without length/crc filled yet
	body := []byte{
		0x02, 0x00, 0x00, 0x00, 0x01, 0xc1, 0x00, 0x00,
		byte(0xe0 | (videoPID>>8)&0x1f), byte(videoPID & 0xff), // PCR PID
		0x00, 0x00, // program_info_length = 0
		vType, byte(0xe0 | (videoPID>>8)&0x1f), byte(videoPID & 0xff), 0x00, 0x00,
		aType, byte(0xe0 | (audioPID>>8)&0x1f), byte(audioPID & 0xff), 0x00, 0x00,
	}
	sectionLen := len(body) - 3 + 4 // bytes after section_length field, incl CRC
	body[1] = byte(0xb0 | (sectionLen>>8)&0x0f)
	body[2] = byte(sectionLen)
	crc := mpegCRC(body)
	body = binary.BigEndian.AppendUint32(body, crc)
	return tsPacket(pmtPIDv, true, cc, append([]byte{0x00}, body...))
}

// pesWrap wraps an elementary stream in a PES packet with a 5-byte optional
// header (PTS present).
func pesWrap(streamID byte, es []byte) []byte {
	hdr := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, 0x80, 0x05,
		0x21, 0x00, 0x01, 0x00, 0x01}
	pesLen := len(es) + len(hdr) - 6
	if pesLen > 0xffff {
		pesLen = 0
	}
	binary.BigEndian.PutUint16(hdr[4:6], uint16(pesLen))
	return append(hdr, es...)
}

// packetizePES splits a PES onto pid using the same rules as the decoder's
// packetize (first packet PUSI, adaptation stuffing on the last).
func packetizePES(pid uint16, pes []byte) []byte {
	first := []byte{sync, byte(pid>>8)&0x1f | 0x40, byte(pid), 0x10}
	return packetize(pid, first, pes)
}

// annexB joins NAL units with 4-byte start codes.
func annexB(nals ...[]byte) []byte {
	var b []byte
	for _, n := range nals {
		b = append(b, 0x00, 0x00, 0x00, 0x01)
		b = append(b, n...)
	}
	return b
}

// demuxES reassembles a PID's elementary stream from a TS buffer.
func demuxES(ts []byte, pid uint16) []byte {
	var pes []byte
	for off := 0; off+pktLen <= len(ts); off += pktLen {
		p := ts[off : off+pktLen]
		if p[0] != sync || pidOf(p) != pid {
			continue
		}
		po := payloadOffset(p)
		if po >= pktLen {
			continue
		}
		pes = append(pes, p[po:]...)
	}
	es := pesPayload(pes)
	if es < 0 {
		return nil
	}
	return pes[es:]
}

func pidOf(p []byte) uint16 { return uint16(p[1]&0x1f)<<8 | uint16(p[2]) }

func splitNALs(es []byte) [][]byte {
	var out [][]byte
	var starts []int
	for i := 0; i+3 <= len(es); {
		if es[i] == 0 && es[i+1] == 0 && es[i+2] == 1 {
			starts = append(starts, i)
			i += 3
		} else {
			i++
		}
	}
	for j, s := range starts {
		end := len(es)
		if j+1 < len(starts) {
			end = starts[j+1]
			if end > 0 && es[end-1] == 0 {
				end-- // 4-byte start code
			}
		}
		out = append(out, es[s+3:end])
	}
	return out
}

func TestDecryptTransportStream(t *testing.T) {
	block := newBlock(t)

	// clear samples
	clearNALs := [][]byte{
		canonicalNAL(9, []byte{0x10}),   // AUD (not encrypted)
		canonicalNAL(7, body(30)),       // SPS (not encrypted)
		canonicalNAL(5, body(5000)),     // IDR slice (encrypted)
		canonicalNAL(1, body(1200)),     // non-IDR slice (encrypted)
		canonicalNAL(1, body(40)),       // small slice (not encrypted, <=48)
	}
	clearFrames := [][]byte{
		makeADTS(600, false),
		makeADTS(37, false),
		makeADTS(9, false), // no encrypted portion
	}

	// build the ENCRYPTED transport stream the way a packager would.
	encNALs := make([][]byte, len(clearNALs))
	for i, n := range clearNALs {
		encNALs[i] = encryptAVCNAL(append([]byte(nil), n...), block, testIV)
	}
	videoES := annexB(encNALs...)
	videoPkts := packetizePES(videoPID, pesWrap(0xe0, videoES))

	encFrames := make([][]byte, len(clearFrames))
	var audioES []byte
	for i, f := range clearFrames {
		ef := append([]byte(nil), f...)
		encryptAAC(ef, block, testIV)
		encFrames[i] = ef
		audioES = append(audioES, ef...)
	}
	audioPkts := packetizePES(audioPID, pesWrap(0xc0, audioES))

	var encTS []byte
	encTS = append(encTS, buildPAT(0)...)
	encTS = append(encTS, buildPMT(0, 0xdb, 0xcf)...) // encrypted stream types
	encTS = append(encTS, videoPkts...)
	encTS = append(encTS, audioPkts...)

	decTS, err := Decrypt(encTS, testKey, testIV)
	if err != nil {
		t.Fatal(err)
	}

	// structural checks
	if len(decTS)%pktLen != 0 {
		t.Fatalf("output not packet-aligned: %d bytes", len(decTS))
	}
	for off := 0; off < len(decTS); off += pktLen {
		if decTS[off] != sync {
			t.Fatalf("lost sync at packet %d", off/pktLen)
		}
	}
	assertPMTClear(t, decTS)

	// video ES must recover the original clear NALs exactly.
	gotNALs := splitNALs(demuxES(decTS, videoPID))
	if len(gotNALs) != len(clearNALs) {
		t.Fatalf("got %d NALs, want %d", len(gotNALs), len(clearNALs))
	}
	for i := range clearNALs {
		if !bytes.Equal(gotNALs[i], clearNALs[i]) {
			t.Fatalf("NAL %d mismatch: got %d bytes want %d", i, len(gotNALs[i]), len(clearNALs[i]))
		}
	}

	// audio ES must recover the original clear ADTS frames exactly.
	wantAudio := bytes.Join(clearFrames, nil)
	if got := demuxES(decTS, audioPID); !bytes.Equal(got, wantAudio) {
		t.Fatalf("audio ES mismatch: got %d bytes want %d", len(got), len(wantAudio))
	}
}

func assertPMTClear(t *testing.T, ts []byte) {
	t.Helper()
	for off := 0; off+pktLen <= len(ts); off += pktLen {
		p := ts[off : off+pktLen]
		if pidOf(p) != pmtPIDv || !pusi(p) {
			continue
		}
		po := payloadOffset(p)
		base := po + 1 + int(p[po]) // section start
		secLen := int(p[base+1]&0x0f)<<8 | int(p[base+2])
		end := base + 3 + secLen
		if mpegCRC(p[base:end-4]) != binary.BigEndian.Uint32(p[end-4:end]) {
			t.Fatalf("PMT CRC invalid after rewrite")
		}
		progInfo := int(p[base+10]&0x0f)<<8 | int(p[base+11])
		pos := base + 12 + progInfo
		for pos+5 <= end-4 {
			st := p[pos]
			if st == 0xdb || st == 0xcf || st == 0xc1 || st == 0xc2 {
				t.Fatalf("PMT still carries encrypted stream_type 0x%02x", st)
			}
			pos += 5 + (int(p[pos+3]&0x0f)<<8 | int(p[pos+4]))
		}
		return
	}
	t.Fatalf("no PMT packet found in output")
}

func TestDecryptNonTSPassthrough(t *testing.T) {
	in := []byte("this is not a transport stream at all, no sync bytes here really")
	out, err := Decrypt(in, testKey, testIV)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("non-TS input should pass through unchanged")
	}
}

func TestDecryptRejectsAC3(t *testing.T) {
	var ts []byte
	ts = append(ts, buildPAT(0)...)
	ts = append(ts, buildPMT(0, 0xdb, 0xc1)...) // encrypted AC-3 audio
	ts = append(ts, tsPacket(videoPID, true, 0, pesWrap(0xe0, annexB(canonicalNAL(1, body(200)))))...)
	if _, err := Decrypt(ts, testKey, testIV); err == nil {
		t.Fatalf("expected error for encrypted AC-3, got nil")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
