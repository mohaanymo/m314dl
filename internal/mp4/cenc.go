package mp4

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

// Scheme is a CENC protection scheme (ISO/IEC 23001-7).
type Scheme string

const (
	SchemeCENC Scheme = "cenc" // AES-128 CTR, full subsample
	SchemeCBC1 Scheme = "cbc1" // AES-128 CBC, full subsample
	SchemeCENS Scheme = "cens" // AES-128 CTR, pattern
	SchemeCBCS Scheme = "cbcs" // AES-128 CBC, pattern, constant IV
)

// InitInfo is the protection metadata extracted from an init segment's tenc
// and schm boxes. One per track (audio and video carry their own).
type InitInfo struct {
	Scheme         Scheme
	DefaultKID     [16]byte
	PerSampleIVLen int    // 0, 8 or 16; 0 => constant IV (cbcs)
	CryptByteBlock int    // pattern: encrypted 16-byte blocks per run
	SkipByteBlock  int    // pattern: clear 16-byte blocks per run
	ConstantIV     []byte // used when PerSampleIVLen == 0
	Protected      bool
}

// isCTR reports whether the scheme uses AES-CTR (vs AES-CBC).
func (s Scheme) isCTR() bool { return s == SchemeCENC || s == SchemeCENS }

// ParseInit extracts CENC protection info from an fMP4 init segment.
// Returns (nil, nil) when the segment is not encrypted.
func ParseInit(initSeg []byte) (*InitInfo, error) {
	stsd, ok := find(initSeg, "moov", "trak", "mdia", "minf", "stbl", "stsd")
	if !ok {
		return nil, nil
	}
	_, _, entries, err := fullBox(stsd.payload)
	if err != nil {
		return nil, err
	}
	// stsd: entry_count(4) then sample entries
	if len(entries) < 4 {
		return nil, nil
	}
	var info *InitInfo
	walk(entries[4:], func(se box) bool {
		if se.typ != "encv" && se.typ != "enca" {
			return true
		}
		child := sampleEntryChildren(se)
		if child == nil {
			return true
		}
		sinf, ok := find(child, "sinf")
		if !ok {
			return true
		}
		ii, err := parseSinf(sinf)
		if err == nil && ii != nil {
			info = ii
			return false
		}
		return true
	})
	return info, nil
}

func parseSinf(sinf box) (*InitInfo, error) {
	info := &InitInfo{}
	var haveScheme, haveTenc bool
	sinf.children(func(b box) bool {
		switch b.typ {
		case "schm":
			_, _, rest, err := fullBox(b.payload)
			if err == nil && len(rest) >= 4 {
				info.Scheme = Scheme(rest[0:4])
				haveScheme = true
			}
		case "schi":
			b.children(func(c box) bool {
				if c.typ == "tenc" {
					if err := parseTenc(c, info); err == nil {
						haveTenc = true
					}
					return false
				}
				return true
			})
		}
		return true
	})
	if !haveTenc {
		return nil, errors.New("mp4: no tenc box")
	}
	if !haveScheme {
		info.Scheme = SchemeCENC // default per spec when schm absent
	}
	return info, nil
}

// parseTenc reads the TrackEncryptionBox (ISO 23001-7 §8.2).
func parseTenc(b box, info *InitInfo) error {
	version, _, rest, err := fullBox(b.payload)
	if err != nil {
		return err
	}
	// rest: reserved(1), {reserved(1) | crypt/skip nibbles(1)}, isProtected(1),
	//       perSampleIVSize(1), KID(16), [constIVsize(1), constIV(n)]
	if len(rest) < 20 {
		return errors.New("mp4: short tenc")
	}
	if version > 0 {
		info.CryptByteBlock = int(rest[1] >> 4)
		info.SkipByteBlock = int(rest[1] & 0x0f)
	}
	info.Protected = rest[2] == 1
	info.PerSampleIVLen = int(rest[3])
	copy(info.DefaultKID[:], rest[4:20])
	if info.Protected && info.PerSampleIVLen == 0 {
		if len(rest) < 21 {
			return errors.New("mp4: tenc missing constant IV size")
		}
		n := int(rest[20])
		if len(rest) < 21+n {
			return errors.New("mp4: tenc truncated constant IV")
		}
		info.ConstantIV = append([]byte(nil), rest[21:21+n]...)
	}
	return nil
}

// subsample is one clear/encrypted split within a sample.
type subsample struct {
	clear     uint32
	encrypted uint32
}

// DecryptFragment decrypts a fragmented-MP4 media segment (styp?/moof/mdat)
// in place using key. info comes from the track's init segment. The buffer is
// mutated: encrypted sample bytes are replaced with plaintext.
func DecryptFragment(frag []byte, info *InitInfo, key []byte) error {
	if len(key) != 16 {
		return fmt.Errorf("mp4: CENC key is %d bytes, want 16", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	// Walk top-level boxes, remembering each moof and its following mdat.
	var moofOff int64 = -1
	var moof box
	var perr error
	walk(frag, func(b box) bool {
		switch b.typ {
		case "moof":
			moof = b
			moofOff = b.off
		case "mdat":
			if moofOff < 0 {
				return true // stray mdat
			}
			mdatData := frag[b.off+b.hdrLen : b.off+b.hdrLen+int64(len(b.payload))]
			if err := decryptTrafs(frag, moof, moofOff, b.off, mdatData, info, block); err != nil {
				perr = err
				return false
			}
			moofOff = -1
		}
		return true
	})
	return perr
}

// decryptTrafs handles every traf in one moof, decrypting into mdatData.
func decryptTrafs(frag []byte, moof box, moofOff, mdatOff int64, mdatData []byte, info *InitInfo, block cipher.Block) error {
	var ferr error
	moof.children(func(tf box) bool {
		if tf.typ != "traf" {
			return true
		}
		if err := decryptOneTraf(frag, tf, moofOff, mdatOff, mdatData, info, block); err != nil {
			ferr = err
			return false
		}
		return true
	})
	return ferr
}

// trunRun is one trun's contribution to a traf: its sample sizes and where its
// (contiguous) sample data begins. A traf may carry several truns; their sample
// data may sit in different, non-adjacent regions of the mdat, while senc/saiz
// describe every sample of the traf as one global in-order list.
type trunRun struct {
	sizes   []uint32
	dataOff int64
	hasOff  bool // data_offset flag (0x000001) present
}

func decryptOneTraf(frag []byte, traf box, moofOff, mdatOff int64, mdatData []byte, info *InitInfo, block cipher.Block) error {
	var (
		truns               []trunRun
		sencIVs             [][]byte
		sencSubs            [][]subsample
		haveSenc            bool
		saizDefault         uint8
		saizSizes           []uint8
		haveSaiz            bool
		saioOffsets         []int64
		baseIsMoof          bool
		tfhdBaseDataOff     int64
		haveTfhdBaseDataOff bool
	)
	traf.children(func(b box) bool {
		switch b.typ {
		case "tfhd":
			baseIsMoof, tfhdBaseDataOff, haveTfhdBaseDataOff = parseTfhd(b)
		case "trun":
			// Collect every trun in order (not just the last): senc/saiz index
			// samples globally across all of them.
			if sizes, off, ok := parseTrun(b); ok {
				hasOff := len(b.payload) >= 4 && b.payload[3]&0x01 != 0
				truns = append(truns, trunRun{sizes: sizes, dataOff: off, hasOff: hasOff})
			}
		case "senc":
			sencIVs, sencSubs, haveSenc = parseSencAuto(b.payload, info.PerSampleIVLen)
		case "uuid":
			// PIFF 1.1 SampleEncryptionBox: senc's ancestor, same layout after the
			// 16-byte extended type, plus optional AlgorithmID/IV_size/KID overrides.
			if isUUID(b, uuidPiffSenc) {
				sencIVs, sencSubs, haveSenc = parsePiffSenc(b, info.PerSampleIVLen)
			}
		case "saiz":
			saizDefault, saizSizes, haveSaiz = parseSaiz(b)
		case "saio":
			saioOffsets = parseSaio(b)
		}
		return true
	})
	if len(truns) == 0 {
		return nil // no samples described; nothing to do
	}

	// Resolve where sample data begins, relative to the whole fragment buffer.
	var base int64
	switch {
	case baseIsMoof:
		base = moofOff
	case haveTfhdBaseDataOff:
		base = tfhdBaseDataOff
	default:
		base = moofOff // default-base-is-moof is the fragmented norm
	}
	mdatBase := mdatOff + mdatHeaderLen(frag, mdatOff)

	// senc/saiz describe the traf's samples as one global list, in trun order.
	total := 0
	for _, t := range truns {
		total += len(t.sizes)
	}

	// Recover per-sample IVs + subsamples: prefer senc, else saiz/saio.
	ivs := sencIVs
	subs := sencSubs
	if !haveSenc {
		var err error
		ivs, subs, err = readAuxInfo(frag, moofOff, saioOffsets, saizDefault, saizSizes, haveSaiz, total, info)
		if err != nil {
			return err
		}
	}

	// Walk samples globally: trun order, then sample order. Each trun's data
	// begins at base + its data_offset (its samples are contiguous from there); a
	// trun that omits data_offset continues where the previous one ended.
	i := 0
	var pos int64
	for k, t := range truns {
		if k == 0 || t.hasOff {
			pos = base + t.dataOff - mdatBase
		}
		if pos < 0 || pos > int64(len(mdatData)) {
			return fmt.Errorf("mp4: sample data offset %d outside mdat (len %d)", pos, len(mdatData))
		}
		for _, sz := range t.sizes {
			end := pos + int64(sz)
			if end > int64(len(mdatData)) {
				return fmt.Errorf("mp4: sample %d overruns mdat", i)
			}
			sample := mdatData[pos:end]
			iv := sampleIV(ivs, i, info)
			var ss []subsample
			if i < len(subs) {
				ss = subs[i]
			}
			if err := decryptSample(sample, iv, ss, info, block); err != nil {
				return fmt.Errorf("mp4: sample %d: %w", i, err)
			}
			pos = end
			i++
		}
	}
	return nil
}

// sampleIV returns the 16-byte IV/counter for sample i.
func sampleIV(ivs [][]byte, i int, info *InitInfo) []byte {
	iv := make([]byte, 16)
	if i < len(ivs) && len(ivs[i]) > 0 {
		copy(iv, ivs[i]) // 8-byte IVs left-align, low bytes stay zero
		return iv
	}
	if len(info.ConstantIV) > 0 {
		copy(iv, info.ConstantIV)
	}
	return iv
}

// decryptSample decrypts one sample in place per the active scheme.
func decryptSample(sample, iv []byte, subs []subsample, info *InitInfo, block cipher.Block) error {
	if info.Scheme.isCTR() {
		return decryptCTR(sample, iv, subs, info, block)
	}
	return decryptCBC(sample, iv, subs, info, block)
}

// decryptCTR handles cenc (no pattern) and cens (pattern). Only encrypted
// subsample bytes consume keystream; clear bytes pass through.
func decryptCTR(sample, iv []byte, subs []subsample, info *InitInfo, block cipher.Block) error {
	stream := cipher.NewCTR(block, iv)
	patterned := info.Scheme == SchemeCENS && (info.CryptByteBlock > 0 || info.SkipByteBlock > 0)
	crypt := func(buf []byte) {
		if !patterned {
			stream.XORKeyStream(buf, buf)
			return
		}
		applyPatternCTR(buf, stream, info.CryptByteBlock, info.SkipByteBlock)
	}
	if len(subs) == 0 {
		crypt(sample)
		return nil
	}
	pos := 0
	for _, s := range subs {
		pos += int(s.clear) // skip clear bytes (no keystream consumed)
		e := pos + int(s.encrypted)
		if e > len(sample) {
			return errors.New("subsample exceeds sample")
		}
		crypt(sample[pos:e])
		pos = e
	}
	return nil
}

// decryptCBC handles cbc1 (full) and cbcs (pattern, constant IV per subsample).
func decryptCBC(sample, iv []byte, subs []subsample, info *InitInfo, block cipher.Block) error {
	patterned := info.Scheme == SchemeCBCS && (info.CryptByteBlock > 0 || info.SkipByteBlock > 0)
	decRange := func(buf []byte) {
		if !patterned {
			decryptCBCFull(buf, iv, block)
			return
		}
		applyPatternCBC(buf, iv, block, info.CryptByteBlock, info.SkipByteBlock)
	}
	if len(subs) == 0 {
		decRange(sample)
		return nil
	}
	pos := 0
	for _, s := range subs {
		pos += int(s.clear)
		e := pos + int(s.encrypted)
		if e > len(sample) {
			return errors.New("subsample exceeds sample")
		}
		decRange(sample[pos:e])
		pos = e
	}
	return nil
}

// decryptCBCFull decrypts whole 16-byte blocks (trailing partial left clear).
func decryptCBCFull(buf, iv []byte, block cipher.Block) {
	n := len(buf) - len(buf)%16
	if n == 0 {
		return
	}
	cipher.NewCBCDecrypter(block, iv[:16]).CryptBlocks(buf[:n], buf[:n])
}

// applyPatternCBC decrypts with the cbcs crypt:skip block pattern. CBC chains
// continuously across the encrypted blocks; skipped blocks are untouched. IV
// starts at the given (constant) IV for this protected range.
func applyPatternCBC(buf, iv []byte, block cipher.Block, cryptBlocks, skipBlocks int) {
	dec := cipher.NewCBCDecrypter(block, iv[:16])
	unit := (cryptBlocks + skipBlocks) * 16
	pos := 0
	for pos+16 <= len(buf) {
		cn := cryptBlocks * 16
		if pos+cn > len(buf) {
			cn = (len(buf) - pos) / 16 * 16
		}
		if cn > 0 {
			dec.CryptBlocks(buf[pos:pos+cn], buf[pos:pos+cn])
		}
		pos += unit
		if skipBlocks == 0 {
			break // pure CBC already handled the whole run
		}
	}
}

// applyPatternCTR decrypts with the cens crypt:skip pattern under one CTR
// stream. Skipped blocks advance neither ciphertext position via keystream.
func applyPatternCTR(buf []byte, stream cipher.Stream, cryptBlocks, skipBlocks int) {
	unit := (cryptBlocks + skipBlocks) * 16
	pos := 0
	for pos < len(buf) {
		cn := cryptBlocks * 16
		if pos+cn > len(buf) {
			cn = len(buf) - pos
		}
		if cn > 0 {
			stream.XORKeyStream(buf[pos:pos+cn], buf[pos:pos+cn])
		}
		pos += unit
	}
}

// ---- box field parsers ----

func parseTfhd(b box) (baseIsMoof bool, baseDataOff int64, haveBaseDataOff bool) {
	_, flags, rest, err := fullBox(b.payload)
	if err != nil || len(rest) < 4 {
		return true, 0, false
	}
	baseIsMoof = flags&0x020000 != 0
	rest = rest[4:] // skip track_ID
	if flags&0x000001 != 0 {
		if len(rest) < 8 {
			return baseIsMoof, 0, false
		}
		baseDataOff = int64(binary.BigEndian.Uint64(rest))
		haveBaseDataOff = true
	}
	return baseIsMoof, baseDataOff, haveBaseDataOff
}

func parseTrun(b box) (sizes []uint32, dataOff int64, ok bool) {
	version, flags, rest, err := fullBox(b.payload)
	if err != nil || len(rest) < 4 {
		return nil, 0, false
	}
	count := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	if flags&0x000001 != 0 { // data-offset present
		if len(rest) < 4 {
			return nil, 0, false
		}
		dataOff = int64(int32(binary.BigEndian.Uint32(rest)))
		rest = rest[4:]
	}
	if flags&0x000004 != 0 { // first-sample-flags present
		if len(rest) < 4 {
			return nil, 0, false
		}
		rest = rest[4:]
	}
	durPresent := flags&0x000100 != 0
	sizePresent := flags&0x000200 != 0
	flagsPresent := flags&0x000400 != 0
	ctsPresent := flags&0x000800 != 0
	sizes = make([]uint32, 0, count)
	for i := uint32(0); i < count; i++ {
		if durPresent {
			if len(rest) < 4 {
				return nil, 0, false
			}
			rest = rest[4:]
		}
		var sz uint32
		if sizePresent {
			if len(rest) < 4 {
				return nil, 0, false
			}
			sz = binary.BigEndian.Uint32(rest)
			rest = rest[4:]
		}
		if flagsPresent {
			if len(rest) < 4 {
				return nil, 0, false
			}
			rest = rest[4:]
		}
		if ctsPresent {
			if len(rest) < 4 {
				return nil, 0, false
			}
			rest = rest[4:]
		}
		sizes = append(sizes, sz)
	}
	_ = version
	return sizes, dataOff, true
}

// parseSencAuto parses a senc payload with the init's IV size, falling back to
// the other common size (8 ↔ 16) when the entries don't tile the box with the
// declared one. A Smooth Streaming init is synthesized and its tenc IV size is a
// guess; a wrong guess would otherwise decrypt to silent garbage. The exact-fit
// check runs first; a box that fits neither size keeps the lenient parse.
func parseSencAuto(payload []byte, ivLen int) (ivs [][]byte, subs [][]subsample, ok bool) {
	if ivs, subs, ok = parseSencPayload(payload, ivLen, true); ok {
		return ivs, subs, true
	}
	if alt := map[int]int{8: 16, 16: 8}[ivLen]; alt != 0 {
		if ivs, subs, ok = parseSencPayload(payload, alt, true); ok {
			return ivs, subs, true
		}
	}
	return parseSencPayload(payload, ivLen, false)
}

// parsePiffSenc parses the PIFF SampleEncryptionBox (uuid A2394F52-…): after
// the extended type it is a senc, except that flag 0x1 prefixes an
// AlgorithmID(3)/IV_size(1)/KID(16) override whose IV size wins.
func parsePiffSenc(b box, ivLen int) ([][]byte, [][]subsample, bool) {
	p := b.payload[16:]
	_, flags, rest, err := fullBox(p)
	if err != nil {
		return nil, nil, false
	}
	if flags&0x000001 != 0 {
		if len(rest) < 20 {
			return nil, nil, false
		}
		ivLen = int(rest[3])
		// re-emit as a plain senc payload: version/flags then the entries
		p = append(append([]byte(nil), p[:4]...), rest[20:]...)
	}
	return parseSencAuto(p, ivLen)
}

// parseSencPayload reads senc entries. With exact set, the entries must consume
// the payload precisely (used to validate an IV-size guess).
func parseSencPayload(payload []byte, ivLen int, exact bool) (ivs [][]byte, subs [][]subsample, ok bool) {
	_, flags, rest, err := fullBox(payload)
	if err != nil || len(rest) < 4 {
		return nil, nil, false
	}
	count := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	useSub := flags&0x000002 != 0
	// ivLen == 0 means constant-IV (cbcs): senc entries carry no per-sample IV.
	ivs = make([][]byte, 0, count)
	subs = make([][]subsample, 0, count)
	for i := uint32(0); i < count; i++ {
		if len(rest) < ivLen {
			return nil, nil, false
		}
		ivs = append(ivs, append([]byte(nil), rest[:ivLen]...))
		rest = rest[ivLen:]
		var ss []subsample
		if useSub {
			if len(rest) < 2 {
				return nil, nil, false
			}
			sc := binary.BigEndian.Uint16(rest)
			rest = rest[2:]
			for j := uint16(0); j < sc; j++ {
				if len(rest) < 6 {
					return nil, nil, false
				}
				ss = append(ss, subsample{
					clear:     uint32(binary.BigEndian.Uint16(rest)),
					encrypted: binary.BigEndian.Uint32(rest[2:]),
				})
				rest = rest[6:]
			}
		}
		subs = append(subs, ss)
	}
	if exact && len(rest) != 0 {
		return nil, nil, false
	}
	return ivs, subs, true
}

func parseSaiz(b box) (defaultSize uint8, sizes []uint8, ok bool) {
	_, flags, rest, err := fullBox(b.payload)
	if err != nil {
		return 0, nil, false
	}
	if flags&0x000001 != 0 {
		if len(rest) < 8 {
			return 0, nil, false
		}
		rest = rest[8:] // aux_info_type + parameter
	}
	if len(rest) < 5 {
		return 0, nil, false
	}
	defaultSize = rest[0]
	count := binary.BigEndian.Uint32(rest[1:])
	rest = rest[5:]
	if defaultSize == 0 {
		if len(rest) < int(count) {
			return 0, nil, false
		}
		sizes = append([]byte(nil), rest[:count]...)
	}
	return defaultSize, sizes, true
}

func parseSaio(b box) []int64 {
	version, flags, rest, err := fullBox(b.payload)
	if err != nil {
		return nil
	}
	if flags&0x000001 != 0 {
		if len(rest) < 8 {
			return nil
		}
		rest = rest[8:]
	}
	if len(rest) < 4 {
		return nil
	}
	count := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	offs := make([]int64, 0, count)
	for i := uint32(0); i < count; i++ {
		if version == 0 {
			if len(rest) < 4 {
				return offs
			}
			offs = append(offs, int64(binary.BigEndian.Uint32(rest)))
			rest = rest[4:]
		} else {
			if len(rest) < 8 {
				return offs
			}
			offs = append(offs, int64(binary.BigEndian.Uint64(rest)))
			rest = rest[8:]
		}
	}
	return offs
}

// readAuxInfo recovers per-sample IVs and subsamples from the saiz/saio-located
// CENC sample auxiliary information (used when there is no senc box).
func readAuxInfo(frag []byte, moofOff int64, saioOffsets []int64, saizDefault uint8, saizSizes []uint8, haveSaiz bool, nSamples int, info *InitInfo) ([][]byte, [][]subsample, error) {
	if len(saioOffsets) == 0 || !haveSaiz {
		return nil, nil, nil // no aux info; samples fully encrypted, IV from tenc
	}
	// saio offset is relative to the moof (default-base-is-moof / typical).
	auxBase := moofOff + saioOffsets[0]
	perEntry := len(saioOffsets) == nSamples
	ivLen := info.PerSampleIVLen // 0 for constant-IV (cbcs): entry is pure subsample data
	ivs := make([][]byte, 0, nSamples)
	subs := make([][]subsample, 0, nSamples)
	cur := auxBase
	for i := 0; i < nSamples; i++ {
		if perEntry {
			cur = moofOff + saioOffsets[i]
		}
		size := int(saizDefault)
		if saizDefault == 0 {
			if i >= len(saizSizes) {
				return nil, nil, errors.New("mp4: saiz shorter than sample count")
			}
			size = int(saizSizes[i])
		}
		if cur < 0 || cur+int64(size) > int64(len(frag)) {
			return nil, nil, fmt.Errorf("mp4: aux info for sample %d out of range", i)
		}
		entry := frag[cur : cur+int64(size)]
		if len(entry) < ivLen {
			return nil, nil, fmt.Errorf("mp4: aux entry %d shorter than IV", i)
		}
		ivs = append(ivs, append([]byte(nil), entry[:ivLen]...))
		var ss []subsample
		if len(entry) > ivLen {
			r := entry[ivLen:]
			if len(r) >= 2 {
				sc := binary.BigEndian.Uint16(r)
				r = r[2:]
				for j := 0; j < int(sc) && len(r) >= 6; j++ {
					ss = append(ss, subsample{
						clear:     uint32(binary.BigEndian.Uint16(r)),
						encrypted: binary.BigEndian.Uint32(r[2:]),
					})
					r = r[6:]
				}
			}
		}
		subs = append(subs, ss)
		if !perEntry {
			cur += int64(size)
		}
	}
	return ivs, subs, nil
}

// mdatHeaderLen returns the header length (8 or 16) of the mdat box at off.
func mdatHeaderLen(frag []byte, off int64) int64 {
	if off+8 > int64(len(frag)) {
		return 8
	}
	if binary.BigEndian.Uint32(frag[off:]) == 1 {
		return 16
	}
	return 8
}
