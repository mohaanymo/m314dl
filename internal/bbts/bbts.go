// Package bbts decrypts "BBTS" (Broadband Transport Stream) MPEG-TS payloads:
// a whole-segment scheme where the video (PID 0x0100) and audio (PID 0x0101)
// elementary streams are AES-128 encrypted, with the base IV carried inside the
// stream's own SDT service name ("mdcm|provider|service|<IV_HEX>"). Only the key
// is supplied from outside; everything else is read from the .ts itself, so this
// stays a generic cipher — it knows about Transport Streams, not about any site.
//
// The block cadence is the format's own: only every 10th 16-byte block (and any
// final short block) is truly AES-encrypted; the rest XOR against the raw
// incremented counter. That is faithful to the source scheme — do not "fix" it.
//
// Ported from bbtsdecrypt (https://github.com/ReiDoBrega/bbtsdecrypt),
// MIT-licensed, (c) 2025-2026 @duck @ReiDoBrega. Adapted to run in-memory
// (buffer in, buffer out) instead of file-to-file.
package bbts

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"io"
)

const (
	tsPkt     = 188
	sync      = 0x47
	pidPAT    = 0x0000
	pidSDT    = 0x0011
	pidPMT    = 0x1000
	videoPID  = 0x0100
	audioPID  = 0x0101
	ivCopyLen = 12
)

// Decrypt decrypts a BBTS-encrypted MPEG-TS buffer with the given 16-byte AES
// key, returning a clean TS buffer. A stream carrying no SDT IV is passed
// through unchanged (nothing to decrypt).
func Decrypt(data, key []byte) ([]byte, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("bbts: key must be 16 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Grow(len(data))

	state := &encState{ivec: make([]byte, 16)}
	var pmtStreams []streamInfo
	sdtAsm := newPSIAssembler()
	pmtAsm := newPSIAssembler()

	var pes []byte
	var pesHeaders []pesHeaderChunk
	var ivSnapForPES []byte
	lastPID := uint16(0xFFFF)

	flushPES := func() {
		if len(pes) == 0 || len(pesHeaders) == 0 || !state.ready {
			pes, pesHeaders, ivSnapForPES, lastPID = nil, nil, nil, 0xFFFF
			return
		}
		sidPrev := byte(0xE1)
		if len(pes) > 3 {
			sidPrev = pes[3]
		}
		if sidPrev == 0xE0 && len(pes) > 8 && ivSnapForPES != nil {
			st := findStreamType(pmtStreams, lastPID)
			decryptPESNormal(pes, st, block, ivSnapForPES)
		}
		pesRemain := len(pes)
		pesPos := 0
		for i, h := range pesHeaders {
			payloadCap := tsPkt - h.headerSize
			var payload []byte
			if pesRemain <= 0 {
				payload = bytes.Repeat([]byte{0xFF}, payloadCap)
			} else if pesRemain < payloadCap {
				if i == len(pesHeaders)-1 {
					hdr := make([]byte, h.headerSize)
					copy(hdr, h.headerBytes)
					stuffingNeeded := payloadCap - pesRemain
					afc := int((hdr[3] >> 4) & 0x3)
					if afc == 1 {
						hdr[3] = (hdr[3] & 0x0F) | 0x30
						if stuffingNeeded == 1 {
							hdr = append(hdr, 0x00)
						} else {
							hdr = append(hdr, byte(stuffingNeeded-1), 0x00)
							hdr = append(hdr, bytes.Repeat([]byte{0xFF}, stuffingNeeded-2)...)
						}
					} else if afc == 3 {
						afLen := int(hdr[4])
						hdr[4] = byte(afLen + stuffingNeeded)
						newHdr := make([]byte, 0, len(hdr)+stuffingNeeded)
						newHdr = append(newHdr, hdr[:5+afLen]...)
						newHdr = append(newHdr, bytes.Repeat([]byte{0xFF}, stuffingNeeded)...)
						newHdr = append(newHdr, hdr[5+afLen:]...)
						hdr = newHdr
					}
					out.Write(hdr)
					out.Write(pes[pesPos : pesPos+pesRemain])
					pesPos += pesRemain
					pesRemain = 0
					continue
				}
				payload = pes[pesPos : pesPos+pesRemain]
				pesPos += pesRemain
				pesRemain = 0
			} else {
				payload = pes[pesPos : pesPos+payloadCap]
				pesPos += payloadCap
				pesRemain -= payloadCap
			}
			out.Write(h.headerBytes)
			out.Write(payload)
		}
		pes, pesHeaders, ivSnapForPES, lastPID = nil, nil, nil, 0xFFFF
	}

	r := bytes.NewReader(data)
	buf := make([]byte, tsPkt)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				flushPES()
				break
			}
			return nil, err
		}
		pkt := make([]byte, tsPkt)
		copy(pkt, buf)

		if pkt[0] != sync {
			out.Write(pkt)
			continue
		}
		pid := tsPID(pkt)

		switch {
		case pid == pidPAT:
			flushPES()
			out.Write(pkt)
			continue
		case pid == pidSDT:
			flushPES()
			if sec := sdtAsm.push(pkt); sec != nil {
				newIVec := make([]byte, 16)
				if parseSDTAndSetIV(sec, newIVec) {
					if !bytes.Equal(state.ivec, newIVec) {
						copy(state.ivec, newIVec)
						pmtStreams = nil
					}
					state.ready = true
				}
			}
			out.Write(pkt)
			continue
		case pid == pidPMT:
			flushPES()
			if sec := pmtAsm.push(pkt); sec != nil && state.ready {
				pmtStreams = parsePMTStreams(sec)
			}
			out.Write(pkt)
			continue
		}

		if !state.ready {
			out.Write(pkt)
			continue
		}
		if pid != videoPID && pid != audioPID {
			flushPES()
			out.Write(pkt)
			continue
		}
		if !tsHasPayload(pkt) {
			flushPES()
			out.Write(pkt)
			continue
		}
		off := tsPayloadOffset(pkt)
		if off >= tsPkt {
			flushPES()
			out.Write(pkt)
			continue
		}

		isNewPES := false
		if off+8 < tsPkt && pkt[off] == 0x00 && pkt[off+1] == 0x00 && pkt[off+2] == 0x01 {
			if sid := pkt[off+3]; sid == 0xC0 || sid == 0xE0 {
				isNewPES = true
			}
		}
		if isNewPES && len(pes) > 0 {
			flushPES()
		}
		if !isNewPES && len(pes) == 0 {
			out.Write(pkt)
			continue
		}
		if isNewPES {
			ivSnapForPES = make([]byte, 16)
			copy(ivSnapForPES, state.ivec)
		}
		if tsAFC(pkt) == 3 {
			pes = append(pes, pkt[off:]...)
			pesHeaders = append(pesHeaders, pesHeaderChunk{headerBytes: append([]byte{}, pkt[:off]...), headerSize: off})
		} else {
			pes = append(pes, pkt[4:]...)
			pesHeaders = append(pesHeaders, pesHeaderChunk{headerBytes: append([]byte{}, pkt[:4]...), headerSize: 4})
		}
		lastPID = pid
	}

	return out.Bytes(), nil
}

type encState struct {
	ivec  []byte
	ready bool
}

type pesHeaderChunk struct {
	headerBytes []byte
	headerSize  int
}

type streamInfo struct {
	PID        uint16
	StreamType byte
}

func ctrInc(counter []byte) {
	c := 1
	for i := 15; i >= 0; i-- {
		c += int(counter[i])
		counter[i] = byte(c & 0xFF)
		c >>= 8
		if c == 0 {
			break
		}
	}
}

func tsPID(pkt []byte) uint16 { return (uint16(pkt[1]&0x1F) << 8) | uint16(pkt[2]) }
func tsPUSI(pkt []byte) bool  { return (pkt[1] & 0x40) != 0 }
func tsAFC(pkt []byte) int    { return int((pkt[3] >> 4) & 0x3) }
func tsHasPayload(pkt []byte) bool {
	afc := tsAFC(pkt)
	return afc == 1 || afc == 3
}
func tsPayloadOffset(pkt []byte) int {
	switch tsAFC(pkt) {
	case 1:
		return 4
	case 3:
		return 5 + int(pkt[4])
	default:
		return tsPkt
	}
}

// psiAssembler reassembles a PSI section that spans multiple TS packets.
type psiAssembler struct {
	buf           []byte
	expectedTotal *int
	collecting    bool
}

func newPSIAssembler() *psiAssembler { return &psiAssembler{} }

func (pa *psiAssembler) push(pkt []byte) []byte {
	if len(pkt) != tsPkt || !tsHasPayload(pkt) {
		return nil
	}
	off := tsPayloadOffset(pkt)
	if off >= tsPkt {
		return nil
	}
	payload := pkt[off:]
	if tsPUSI(pkt) {
		pointer := int(payload[0])
		payload = payload[1:]
		if pointer > len(payload) {
			return nil
		}
		payload = payload[pointer:]
		pa.buf = nil
		pa.expectedTotal = nil
		pa.collecting = true
	}
	if !pa.collecting {
		return nil
	}
	pa.buf = append(pa.buf, payload...)
	if pa.expectedTotal == nil && len(pa.buf) >= 3 {
		sectionLength := int((uint16(pa.buf[1]&0x0F) << 8) | uint16(pa.buf[2]))
		expected := 3 + sectionLength
		pa.expectedTotal = &expected
	}
	if pa.expectedTotal != nil && len(pa.buf) >= *pa.expectedTotal {
		section := make([]byte, *pa.expectedTotal)
		copy(section, pa.buf[:*pa.expectedTotal])
		pa.buf = nil
		pa.expectedTotal = nil
		pa.collecting = false
		return section
	}
	return nil
}

// parseSDTAndSetIV pulls the base IV out of the SDT service name
// ("mdcm|provider|service|<X+IV_HEX>"), copying the first ivCopyLen bytes.
func parseSDTAndSetIV(section, ivec []byte) bool {
	if len(section) < 16 || section[0] != 0x42 {
		return false
	}
	sectionLength := int((uint16(section[1]&0x0F) << 8) | uint16(section[2]))
	end := 3 + sectionLength
	if end > len(section) {
		return false
	}
	pos := 3 + 8
	for pos+5 <= end-4 {
		descLoopLen := int((uint16(section[pos+3]&0x0F) << 8) | uint16(section[pos+4]))
		dpos := pos + 5
		dend := dpos + descLoopLen
		for dpos+2 <= dend && dpos+2 <= end-4 {
			tag := section[dpos]
			length := int(section[dpos+1])
			dpos += 2
			if dpos+length > len(section) {
				break
			}
			body := section[dpos : dpos+length]
			dpos += length
			if tag == 0x48 && len(body) >= 3 {
				providerLen := int(body[1])
				if 2+providerLen >= len(body) {
					continue
				}
				snLenIdx := 2 + providerLen
				snLen := int(body[snLenIdx])
				if snLenIdx+1+snLen > len(body) {
					continue
				}
				serviceName := string(body[snLenIdx+1 : snLenIdx+1+snLen])
				parts := splitPipe(serviceName)
				if len(parts) < 4 || parts[0] != "mdcm" || parts[3] == "" {
					continue
				}
				ivBin, err := hexBytes(parts[3][1:], 16) // drop the leading marker char
				if err != nil {
					continue
				}
				for i := range ivec {
					ivec[i] = 0
				}
				for i := 0; i < ivCopyLen && i < len(ivBin); i++ {
					ivec[i] = ivBin[i]
				}
				return true
			}
		}
		pos = dend
	}
	return false
}

func parsePMTStreams(section []byte) []streamInfo {
	if len(section) < 12 || section[0] != 0x02 {
		return nil
	}
	sectionLength := int((uint16(section[1]&0x0F) << 8) | uint16(section[2]))
	end := 3 + sectionLength
	if end > len(section) {
		return nil
	}
	programInfoLen := int((uint16(section[10]&0x0F) << 8) | uint16(section[11]))
	pos := 12 + programInfoLen
	var out []streamInfo
	for pos+5 <= end-4 {
		st := section[pos]
		pid := (uint16(section[pos+1]&0x1F) << 8) | uint16(section[pos+2])
		esInfoLen := int((uint16(section[pos+3]&0x0F) << 8) | uint16(section[pos+4]))
		out = append(out, streamInfo{PID: pid, StreamType: st})
		pos += 5 + esInfoLen
	}
	return out
}

func findStreamType(streams []streamInfo, pid uint16) byte {
	for _, s := range streams {
		if s.PID == pid {
			return s.StreamType
		}
	}
	return 0
}

// decryptESSparse decrypts elementary-stream bytes in place. The counter copy is
// local so the caller's base IV is never touched. Only every 10th block (and a
// final short block) is AES-encrypted; the format XORs the rest against the raw
// counter. Emulation-prevention bytes (00 00 03 -> 00 00) are stripped first.
func decryptESSparse(es []byte, block cipher.Block, ivStart []byte) {
	newES := make([]byte, 0, len(es))
	for i := 0; i < len(es); {
		if i+2 < len(es) && es[i] == 0 && es[i+1] == 0 && es[i+2] == 3 {
			newES = append(newES, 0, 0)
			i += 3
		} else {
			newES = append(newES, es[i])
			i++
		}
	}
	iv := make([]byte, 16)
	copy(iv, ivStart)
	esLen := len(newES)
	pos := 0
	counter := 0
	for esLen > 0 {
		ctrInc(iv)
		tmp := make([]byte, 16)
		copy(tmp, iv)
		if esLen <= 16 || counter%10 == 0 {
			block.Encrypt(tmp, tmp)
		}
		decLen := 16
		if esLen < 16 {
			decLen = esLen
		}
		for k := 0; k < decLen; k++ {
			newES[pos+k] ^= tmp[k]
		}
		esLen -= decLen
		pos += 16
		counter++
	}
	if len(newES) != len(es) {
		if diff := len(es) - len(newES); diff > 0 {
			newES = append(newES, es[len(es)-diff:]...)
		}
	}
	copy(es, newES)
}

func decryptPESNormal(pes []byte, streamType byte, block cipher.Block, ivSnap []byte) {
	if len(pes) < 9 {
		return
	}
	pesHeaderLen := int(pes[8])
	headerEnd := 9 + pesHeaderLen
	if headerEnd > len(pes) {
		return
	}
	newPES := make([]byte, 0, len(pes))
	newPES = append(newPES, pes[:headerEnd]...)

	nalHdrLen := 1
	if streamType != 0x1B {
		nalHdrLen = 2
	}
	posSt := headerEnd
	for i := posSt; i < len(pes); i++ {
		if i == len(pes)-1 {
			if len(pes)-2 > posSt+3+nalHdrLen {
				newPES = append(newPES, pes[posSt:posSt+3+nalHdrLen]...)
				es := pes[posSt+3+nalHdrLen : len(pes)-2]
				esCopy := make([]byte, len(es))
				copy(esCopy, es)
				if len(esCopy) > 0 {
					decryptESSparse(esCopy, block, ivSnap)
				}
				newPES = append(newPES, esCopy...)
				newPES = append(newPES, pes[len(pes)-2:]...)
			} else {
				newPES = append(newPES, pes[posSt:]...)
			}
		} else if i+2 < len(pes) && pes[i] == 0 && pes[i+1] == 0 && pes[i+2] == 1 {
			if i != posSt {
				if i-2 > posSt+3+nalHdrLen {
					newPES = append(newPES, pes[posSt:posSt+3+nalHdrLen]...)
					var es []byte
					flag := false
					if pes[i-1] == 0 {
						flag = true
						es = append(es, pes[posSt+3+nalHdrLen:i-3]...)
					} else {
						es = append(es, pes[posSt+3+nalHdrLen:i-2]...)
					}
					if len(es) > 0 {
						decryptESSparse(es, block, ivSnap)
					}
					newPES = append(newPES, es...)
					if flag {
						newPES = append(newPES, pes[i-3:i]...)
					} else {
						newPES = append(newPES, pes[i-2:i]...)
					}
				} else {
					newPES = append(newPES, pes[posSt:i]...)
				}
				posSt = i
			}
		}
	}
	copy(pes, newPES)
}

func splitPipe(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func hexBytes(s string, n int) ([]byte, error) {
	if len(s) != n*2 {
		return nil, fmt.Errorf("bbts: bad IV hex length %d, want %d", len(s), n*2)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		var b byte
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			var v byte
			switch {
			case c >= '0' && c <= '9':
				v = c - '0'
			case c >= 'a' && c <= 'f':
				v = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				v = c - 'A' + 10
			default:
				return nil, fmt.Errorf("bbts: bad IV hex char %q", c)
			}
			b = b<<4 | v
		}
		out[i] = b
	}
	return out, nil
}
