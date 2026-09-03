package mp4

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"testing"
)

// Hermetic round-trip tests: build a real fragmented-MP4 fragment (moof/traf/
// tfhd/trun/senc + mdat), encrypt sample data with an independent oracle, then
// assert DecryptFragment recovers the plaintext. These run without fixtures or
// mp4decrypt, so CI proves the crypto + box parsing on their own.

func mkBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	copy(b[8:], payload)
	return b
}

func mkFullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return mkBox(typ, append(head, payload...))
}

func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }

type testSample struct {
	plain []byte
	subs  []subsample // clear/encrypted split; nil => whole sample encrypted
	iv    []byte      // per-sample IV (nil for constant-IV schemes)
}

// buildFragment assembles a valid fragment whose mdat holds cipher (already
// encrypted), described by trun sizes and senc IV/subsample data.
func buildFragment(samples []testSample, cipherData []byte, ivLen int, useSub bool) []byte {
	// trun: data-offset(0x1) + sample-size(0x200) present
	trunPayload := append(u32(uint32(len(samples))), u32(0)...) // count, data_offset placeholder
	for _, s := range samples {
		trunPayload = append(trunPayload, u32(uint32(len(s.plain)))...)
	}
	trun := mkFullBox("trun", 0, 0x000001|0x000200, trunPayload)

	// senc
	sencFlags := uint32(0)
	if useSub {
		sencFlags = 0x000002
	}
	sencPayload := u32(uint32(len(samples)))
	for _, s := range samples {
		if ivLen > 0 {
			sencPayload = append(sencPayload, s.iv[:ivLen]...)
		}
		if useSub {
			sencPayload = append(sencPayload, u16(uint16(len(s.subs)))...)
			for _, ss := range s.subs {
				sencPayload = append(sencPayload, u16(uint16(ss.clear))...)
				sencPayload = append(sencPayload, u32(ss.encrypted)...)
			}
		}
	}
	senc := mkFullBox("senc", 0, sencFlags, sencPayload)

	tfhd := mkFullBox("tfhd", 0, 0x020000, u32(1)) // default-base-is-moof, track_ID=1
	traf := mkBox("traf", concat(tfhd, trun, senc))
	moof := mkBox("moof", traf)

	// patch trun data_offset = len(moof) + mdat header(8)
	dataOff := uint32(len(moof) + 8)
	ti := bytes.Index(moof, []byte("trun"))
	binary.BigEndian.PutUint32(moof[ti+4+4+4:], dataOff) // +type +ver/flags +count → data_offset

	return concat(moof, mkBox("mdat", cipherData))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// encryptCTR is the independent oracle: encrypt each sample's protected bytes
// under one CTR stream per sample, skipping clear bytes.
func encryptCTR(key []byte, samples []testSample) []byte {
	block, _ := aes.NewCipher(key)
	var out []byte
	for _, s := range samples {
		buf := append([]byte(nil), s.plain...)
		iv := make([]byte, 16)
		copy(iv, s.iv)
		stream := cipher.NewCTR(block, iv)
		if len(s.subs) == 0 {
			stream.XORKeyStream(buf, buf)
		} else {
			pos := 0
			for _, ss := range s.subs {
				pos += int(ss.clear)
				e := pos + int(ss.encrypted)
				stream.XORKeyStream(buf[pos:e], buf[pos:e])
				pos = e
			}
		}
		out = append(out, buf...)
	}
	return out
}

func TestDecryptCTRHermetic(t *testing.T) {
	key := []byte("0123456789abcdef")
	samples := []testSample{
		{plain: bytes.Repeat([]byte{0xA1}, 96), subs: []subsample{{clear: 16, encrypted: 80}}, iv: seqIV(1)},
		{plain: bytes.Repeat([]byte{0xB2}, 64), subs: nil, iv: seqIV(2)}, // whole-sample encrypted
		{plain: bytes.Repeat([]byte{0xC3}, 160), subs: []subsample{{clear: 5, encrypted: 32}, {clear: 3, encrypted: 120}}, iv: seqIV(3)},
	}
	cipherData := encryptCTR(key, samples)
	frag := buildFragment(samples, cipherData, 16, true)
	info := &InitInfo{Scheme: SchemeCENC, PerSampleIVLen: 16, Protected: true}
	if err := DecryptFragment(frag, info, key); err != nil {
		t.Fatal(err)
	}
	got := mdatOf(frag)
	want := concat(samples[0].plain, samples[1].plain, samples[2].plain)
	if !bytes.Equal(got, want) {
		t.Fatalf("CTR round-trip mismatch:\n got %x\nwant %x", got[:32], want[:32])
	}
}

// encryptCBCSPattern is the independent oracle for cbcs: constant IV per
// subsample, CBC chaining across encrypted blocks, skip the rest, trailing
// partial block left clear.
func encryptCBCSPattern(key, constIV []byte, samples []testSample, cryptBlk, skipBlk int) []byte {
	block, _ := aes.NewCipher(key)
	var out []byte
	for _, s := range samples {
		buf := append([]byte(nil), s.plain...)
		ranges := s.subs
		if len(ranges) == 0 {
			ranges = []subsample{{clear: 0, encrypted: uint32(len(buf))}}
		}
		pos := 0
		for _, ss := range ranges {
			pos += int(ss.clear)
			e := pos + int(ss.encrypted)
			enc := cipher.NewCBCEncrypter(block, constIV[:16])
			unit := (cryptBlk + skipBlk) * 16
			p := pos
			for p+16 <= e {
				cn := cryptBlk * 16
				if p+cn > e {
					cn = (e - p) / 16 * 16
				}
				if cn > 0 {
					enc.CryptBlocks(buf[p:p+cn], buf[p:p+cn])
				}
				p += unit
				if skipBlk == 0 {
					break
				}
			}
			pos = e
		}
		out = append(out, buf...)
	}
	return out
}

func TestDecryptCBCSHermetic(t *testing.T) {
	key := []byte("fedcba9876543210")
	constIV := bytes.Repeat([]byte{0x11}, 16)
	samples := []testSample{
		{plain: bytes.Repeat([]byte{0x7E}, 16*40+7), subs: []subsample{{clear: 9, encrypted: 16*40 - 2}}},
		{plain: bytes.Repeat([]byte{0x5D}, 16*10), subs: nil},
	}
	cipherData := encryptCBCSPattern(key, constIV, samples, 1, 9)
	frag := buildFragment(samples, cipherData, 0, true) // ivLen 0: constant IV
	info := &InitInfo{Scheme: SchemeCBCS, PerSampleIVLen: 0, CryptByteBlock: 1, SkipByteBlock: 9, ConstantIV: constIV, Protected: true}
	if err := DecryptFragment(frag, info, key); err != nil {
		t.Fatal(err)
	}
	got := mdatOf(frag)
	want := concat(samples[0].plain, samples[1].plain)
	if !bytes.Equal(got, want) {
		n := 0
		for n < len(got) && got[n] == want[n] {
			n++
		}
		t.Fatalf("CBCS round-trip mismatch at byte %d", n)
	}
}

func seqIV(seq uint64) []byte {
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv, seq)
	return iv
}

// ---- multi-trun ----

// buildMultiTrunFragment assembles one traf holding one trun per run, each with
// its own data_offset, and a single global senc over every sample in trun
// order. Each run's cipher data is separated from the next by `gap` junk bytes
// in the mdat, so the runs occupy non-adjacent regions: a decryptor that walked
// samples contiguously from the first trun would decrypt the junk as data.
func buildMultiTrunFragment(runs [][]testSample, cipherRuns [][]byte, gap, ivLen int, useSub bool) []byte {
	var truns [][]byte
	for _, run := range runs {
		p := append(u32(uint32(len(run))), u32(0)...) // count, data_offset placeholder
		for _, s := range run {
			p = append(p, u32(uint32(len(s.plain)))...)
		}
		truns = append(truns, mkFullBox("trun", 0, 0x000001|0x000200, p))
	}

	var all []testSample
	for _, run := range runs {
		all = append(all, run...)
	}
	sencFlags := uint32(0)
	if useSub {
		sencFlags = 0x000002
	}
	sencPayload := u32(uint32(len(all)))
	for _, s := range all {
		if ivLen > 0 {
			sencPayload = append(sencPayload, s.iv[:ivLen]...)
		}
		if useSub {
			sencPayload = append(sencPayload, u16(uint16(len(s.subs)))...)
			for _, ss := range s.subs {
				sencPayload = append(sencPayload, u16(uint16(ss.clear))...)
				sencPayload = append(sencPayload, u32(ss.encrypted)...)
			}
		}
	}
	senc := mkFullBox("senc", 0, sencFlags, sencPayload)

	tfhd := mkFullBox("tfhd", 0, 0x020000, u32(1)) // default-base-is-moof
	traf := mkBox("traf", concat(append([][]byte{tfhd}, append(truns, senc)...)...))
	moof := mkBox("moof", traf)

	// mdat: run0 | gap | run1 | gap | ...  and patch each trun's data_offset.
	var mdat []byte
	junk := bytes.Repeat([]byte{0xEE}, gap)
	search := 0
	for k, c := range cipherRuns {
		if k > 0 {
			mdat = append(mdat, junk...)
		}
		off := uint32(len(moof) + 8 + len(mdat))
		ti := bytes.Index(moof[search:], []byte("trun")) + search
		binary.BigEndian.PutUint32(moof[ti+4+4+4:], off)
		search = ti + 4
		mdat = append(mdat, c...)
	}
	return concat(moof, mkBox("mdat", mdat))
}

// expectedMultiTrunMdat is the plaintext mdat: each run's plaintext with the
// untouched junk gap between runs.
func expectedMultiTrunMdat(runs [][]testSample, gap int) []byte {
	var out []byte
	for k, run := range runs {
		if k > 0 {
			out = append(out, bytes.Repeat([]byte{0xEE}, gap)...)
		}
		for _, s := range run {
			out = append(out, s.plain...)
		}
	}
	return out
}

func firstDiff(a, b []byte) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// A traf with two truns: senc IVs/subsamples must be applied by global sample
// index, and each trun's samples addressed from its own data_offset.
func TestDecryptMultiTrunCTR(t *testing.T) {
	key := []byte("0123456789abcdef")
	runs := [][]testSample{
		{
			{plain: bytes.Repeat([]byte{0xA1}, 96), subs: []subsample{{clear: 16, encrypted: 80}}, iv: seqIV(1)},
			{plain: bytes.Repeat([]byte{0xB2}, 64), subs: nil, iv: seqIV(2)},
		},
		{
			{plain: bytes.Repeat([]byte{0xC3}, 160), subs: []subsample{{clear: 5, encrypted: 32}, {clear: 3, encrypted: 120}}, iv: seqIV(3)},
			{plain: bytes.Repeat([]byte{0xD4}, 48), subs: nil, iv: seqIV(4)},
		},
	}
	cipherRuns := [][]byte{encryptCTR(key, runs[0]), encryptCTR(key, runs[1])}
	const gap = 37
	frag := buildMultiTrunFragment(runs, cipherRuns, gap, 16, true)
	info := &InitInfo{Scheme: SchemeCENC, PerSampleIVLen: 16, Protected: true}
	if err := DecryptFragment(frag, info, key); err != nil {
		t.Fatal(err)
	}
	got, want := mdatOf(frag), expectedMultiTrunMdat(runs, gap)
	if !bytes.Equal(got, want) {
		t.Fatalf("multi-trun CTR mismatch at byte %d of %d", firstDiff(got, want), len(want))
	}
}

func TestDecryptMultiTrunCBCS(t *testing.T) {
	key := []byte("fedcba9876543210")
	constIV := bytes.Repeat([]byte{0x11}, 16)
	runs := [][]testSample{
		{
			{plain: bytes.Repeat([]byte{0x7E}, 16*40+7), subs: []subsample{{clear: 9, encrypted: 16*40 - 2}}},
			{plain: bytes.Repeat([]byte{0x5D}, 16*10), subs: nil},
		},
		{
			{plain: bytes.Repeat([]byte{0x3C}, 16*21+3), subs: []subsample{{clear: 4, encrypted: 16*21 - 1}}},
		},
	}
	cipherRuns := [][]byte{
		encryptCBCSPattern(key, constIV, runs[0], 1, 9),
		encryptCBCSPattern(key, constIV, runs[1], 1, 9),
	}
	const gap = 21
	frag := buildMultiTrunFragment(runs, cipherRuns, gap, 0, true) // ivLen 0: constant IV
	info := &InitInfo{Scheme: SchemeCBCS, PerSampleIVLen: 0, CryptByteBlock: 1, SkipByteBlock: 9, ConstantIV: constIV, Protected: true}
	if err := DecryptFragment(frag, info, key); err != nil {
		t.Fatal(err)
	}
	got, want := mdatOf(frag), expectedMultiTrunMdat(runs, gap)
	if !bytes.Equal(got, want) {
		t.Fatalf("multi-trun CBCS mismatch at byte %d of %d", firstDiff(got, want), len(want))
	}
}
