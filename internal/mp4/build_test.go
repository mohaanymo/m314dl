package mp4

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// SPS/PPS from an x264 baseline 320x180 encode (the fixture the e2e tests
// generate); profile 0x42, compat 0xc0, level 0x0d.
var (
	testSPS, _ = hex.DecodeString("6742c00dda05067e7c0440000003004000000c83c50aa8")
	testPPS, _ = hex.DecodeString("68ce3c80")
	testKID    = [16]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
)

func videoInit(prot *ProtectionSpec) []byte {
	return BuildInit(InitSpec{
		TrackID: 1, Timescale: 10000000, Language: "eng",
		Video:      &VideoSpec{Width: 320, Height: 180, SPS: [][]byte{testSPS}, PPS: [][]byte{testPPS}, NALLengthSize: 4},
		Protection: prot,
	})
}

// sampleEntry returns the first stsd entry of an init.
func sampleEntry(t *testing.T, init []byte) box {
	t.Helper()
	stsd, ok := find(init, "moov", "trak", "mdia", "minf", "stbl", "stsd")
	if !ok {
		t.Fatal("no stsd")
	}
	entry, _, err := readBox(stsd.payload, 8)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestBuildInitVideoProtected(t *testing.T) {
	init := videoInit(&ProtectionSpec{KID: testKID, IVSize: 8})
	if string(init[4:8]) != "ftyp" {
		t.Fatalf("init does not start with ftyp: %x", init[:8])
	}
	info, err := ParseInit(init)
	if err != nil || info == nil {
		t.Fatalf("ParseInit: info=%v err=%v", info, err)
	}
	if info.Scheme != SchemeCENC || info.PerSampleIVLen != 8 || info.DefaultKID != testKID || !info.Protected {
		t.Fatalf("tenc round-trip: %+v", info)
	}
	if got := SoleTrackID(init); got != 1 {
		t.Fatalf("SoleTrackID = %d, want 1", got)
	}
	if e := sampleEntry(t, init); e.typ != "encv" {
		t.Fatalf("sample entry %q, want encv", e.typ)
	}

	// De-protecting it yields the plain avc1 entry with the avcC we built.
	clean := SanitizeInit(init)
	if info, _ := ParseInit(clean); info != nil {
		t.Fatal("sanitized init still protected")
	}
	e := sampleEntry(t, clean)
	if e.typ != "avc1" {
		t.Fatalf("sanitized entry %q, want avc1", e.typ)
	}
	avcc, ok := find(e.payload[78:], "avcC")
	if !ok {
		t.Fatal("no avcC after the 78-byte visual prefix")
	}
	want := concat([]byte{1, 0x42, 0xc0, 0x0d, 0xff, 0xe1}, u16(uint16(len(testSPS))), testSPS, []byte{1}, u16(uint16(len(testPPS))), testPPS)
	if !bytes.Equal(avcc.payload, want) {
		t.Fatalf("avcC:\n got %x\nwant %x", avcc.payload, want)
	}
	// width/height in the visual entry
	if w, h := binary.BigEndian.Uint16(e.payload[24:]), binary.BigEndian.Uint16(e.payload[26:]); w != 320 || h != 180 {
		t.Fatalf("entry size %dx%d", w, h)
	}
}

func TestBuildInitAudio(t *testing.T) {
	asc := []byte{0x11, 0x90} // AAC-LC 48 kHz stereo
	init := BuildInit(InitSpec{TrackID: 2, Timescale: 48000, Audio: &AudioSpec{Channels: 2, SampleRate: 48000, ASC: asc}})
	if info, err := ParseInit(init); err != nil || info != nil {
		t.Fatalf("clear audio init: info=%v err=%v", info, err)
	}
	if got := SoleTrackID(init); got != 2 {
		t.Fatalf("SoleTrackID = %d, want 2", got)
	}
	e := sampleEntry(t, init)
	if e.typ != "mp4a" {
		t.Fatalf("entry %q, want mp4a", e.typ)
	}
	if ch, rate := binary.BigEndian.Uint16(e.payload[16:]), binary.BigEndian.Uint32(e.payload[24:])>>16; ch != 2 || rate != 48000 {
		t.Fatalf("channels=%d rate=%d", ch, rate)
	}
	esds, ok := find(e.payload[28:], "esds")
	if !ok {
		t.Fatal("no esds after the 28-byte audio prefix")
	}
	// DecoderSpecificInfo descriptor: tag 5, length, ASC
	if !bytes.Contains(esds.payload, concat([]byte{0x05, byte(len(asc))}, asc)) {
		t.Fatalf("esds lacks the ASC: %x", esds.payload)
	}
	// DecoderConfig: 13 fixed bytes + the DSI descriptor; AAC, audio stream
	if !bytes.Contains(esds.payload, []byte{0x04, byte(13 + 2 + len(asc)), 0x40, 0x15}) {
		t.Fatalf("esds lacks the AAC decoder config: %x", esds.payload)
	}
}

// piffFragment builds a PIFF-shaped fragment: tfhd for track 2 (no tfdt) and a
// tfxd carrying the fragment time, the way a Smooth Streaming server serves it.
func piffFragment(trackID uint32, tfxdTime, tfxdDur uint64, mdat []byte) []byte {
	tfhd := mkFullBox("tfhd", 0, 0x000020, concat(u32(trackID), u32(0x01010000)))
	trun := mkFullBox("trun", 0, 0x000001|0x000200, concat(u32(1), u32(0), u32(uint32(len(mdat)))))
	tfxd := mkBox("uuid", concat(uuidTfxd, []byte{1, 0, 0, 0}, be64(tfxdTime), be64(tfxdDur)))
	moof := mkBox("moof", concat(mkFullBox("mfhd", 0, 0, u32(1)), mkBox("traf", concat(tfhd, trun, tfxd))))
	ti := bytes.Index(moof, []byte("trun"))
	binary.BigEndian.PutUint32(moof[ti+12:], uint32(len(moof)+8)) // data_offset → mdat payload
	return concat(moof, mkBox("mdat", mdat))
}

func trunDataOffset(t *testing.T, frag []byte) int64 {
	t.Helper()
	trun, ok := find(frag, "moof", "traf", "trun")
	if !ok {
		t.Fatal("no trun")
	}
	_, off, _ := parseTrun(trun)
	return off
}

func TestNormalizeFragment(t *testing.T) {
	mdat := []byte("sample-bytes")
	frag := piffFragment(2, 123456789, 10000000, mdat)

	out := NormalizeFragment(append([]byte(nil), frag...), 1)
	if bytes.Equal(out, frag) {
		t.Fatal("fragment unchanged")
	}
	tfhd, _ := find(out, "moof", "traf", "tfhd")
	if id := binary.BigEndian.Uint32(tfhd.payload[4:]); id != 1 {
		t.Fatalf("track_ID %d, want 1", id)
	}
	tfdt, ok := find(out, "moof", "traf", "tfdt")
	if !ok {
		t.Fatal("no tfdt synthesized")
	}
	if v, _, rest, _ := fullBox(tfdt.payload); v != 1 || binary.BigEndian.Uint64(rest) != 123456789 {
		t.Fatalf("tfdt v%d time %d", v, binary.BigEndian.Uint64(rest))
	}
	// the trun still points at the mdat payload after the moof grew
	moof, _ := find(out, "moof")
	moofLen := moof.hdrLen + int64(len(moof.payload))
	if off := trunDataOffset(t, out); off != moofLen+8 {
		t.Fatalf("trun data_offset %d, want %d", off, moofLen+8)
	}
	if got := out[moofLen+8:]; !bytes.Equal(got, mdat) {
		t.Fatalf("mdat moved: %q", got)
	}
	// idempotent, and a no-op returns the very same slice
	again := NormalizeFragment(out, 1)
	if !bytes.Equal(again, out) || &again[0] != &out[0] {
		t.Fatal("second pass changed the fragment")
	}
	// trackID 0 (TS / multi-track init) leaves the id alone but still adds tfdt
	out0 := NormalizeFragment(append([]byte(nil), frag...), 0)
	tfhd0, _ := find(out0, "moof", "traf", "tfhd")
	if id := binary.BigEndian.Uint32(tfhd0.payload[4:]); id != 2 {
		t.Fatalf("trackID 0 retagged to %d", id)
	}
	if _, ok := find(out0, "moof", "traf", "tfdt"); !ok {
		t.Fatal("trackID 0: no tfdt")
	}
}

// piffWrap rewrites a fragment's senc into the PIFF uuid form (dropping any
// saiz/saio), fixing the trun offset for the moof growth/shrink.
func piffWrap(frag []byte, override bool, ivSize byte) []byte {
	return rebuildSeq(frag, func(b box) ([]byte, bool) {
		if b.typ != "moof" {
			return nil, false
		}
		delta := 0
		return emitBox("moof", rebuildSeq(b.payload, func(c box) ([]byte, bool) {
			if c.typ != "traf" {
				return nil, false
			}
			var repl []byte
			walk(c.payload, func(d box) bool {
				switch d.typ {
				case "senc":
					p := d.payload
					if override {
						hdr := []byte{p[0], p[1], p[2], p[3] | 1}
						p = concat(hdr, []byte{0, 0, 1, ivSize}, testKID[:], p[4:])
					}
					repl = mkBox("uuid", concat(uuidPiffSenc, p))
					delta += len(repl) - int(d.hdrLen) - len(d.payload)
				case "saiz", "saio":
					delta -= int(d.hdrLen) + len(d.payload)
				}
				return true
			})
			return emitBox("traf", rebuildSeq(c.payload, func(d box) ([]byte, bool) {
				switch d.typ {
				case "senc":
					return repl, true
				case "saiz", "saio":
					return nil, true
				case "trun":
					return patchTrunDataOffset(d, -delta), true
				}
				return nil, false
			})), true
		})), true
	})
}

func TestDecryptPIFFSampleEncryptionBox(t *testing.T) {
	key := []byte("0123456789abcdef")
	samples := []testSample{
		{plain: bytes.Repeat([]byte("A"), 40), subs: []subsample{{clear: 8, encrypted: 32}}, iv: seqIV(1)},
		{plain: bytes.Repeat([]byte("B"), 33), subs: []subsample{{clear: 1, encrypted: 32}}, iv: seqIV(2)},
	}
	cipherData := encryptCTR(key, samples)
	plain := concat(samples[0].plain, samples[1].plain)
	for _, tc := range []struct {
		name     string
		override bool
		tencIV   int
	}{
		{"plain uuid senc", false, 8},
		{"uuid senc with IV-size override", true, 8},
		{"override wins over a wrong tenc IV size", true, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frag := piffWrap(buildFragment(samples, cipherData, 8, true), tc.override, 8)
			if _, ok := find(frag, "moof", "traf", "senc"); ok {
				t.Fatal("senc still present after the PIFF rewrite")
			}
			info := &InitInfo{Scheme: SchemeCENC, PerSampleIVLen: tc.tencIV, Protected: true}
			if err := DecryptFragment(frag, info, key); err != nil {
				t.Fatal(err)
			}
			if got := mdatOf(frag); !bytes.Equal(got, plain) {
				t.Fatalf("PIFF decrypt mismatch:\n got %q\nwant %q", got, plain)
			}
			// and the stripper removes the uuid box, leaving a clean fragment
			clean := StripFragmentProtection(frag)
			if _, ok := find(clean, "moof", "traf", "uuid"); ok {
				t.Fatal("PIFF senc uuid survived StripFragmentProtection")
			}
			if got := mdatOf(clean); !bytes.Equal(got, plain) {
				t.Fatal("mdat damaged by strip")
			}
		})
	}
}

// A synthesized init guesses the IV size; the senc's own layout corrects it.
func TestSencIVSizeAutoDetect(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv16 := func(n byte) []byte { iv := make([]byte, 16); iv[15] = n; return iv }
	samples := []testSample{
		{plain: bytes.Repeat([]byte("x"), 48), subs: []subsample{{clear: 16, encrypted: 32}}, iv: iv16(1)},
		{plain: bytes.Repeat([]byte("y"), 64), subs: []subsample{{clear: 0, encrypted: 64}}, iv: iv16(2)},
	}
	cipherData := encryptCTR(key, samples)
	frag := buildFragment(samples, cipherData, 16, true)
	info := &InitInfo{Scheme: SchemeCENC, PerSampleIVLen: 8, Protected: true} // wrong guess
	if err := DecryptFragment(frag, info, key); err != nil {
		t.Fatal(err)
	}
	if got := mdatOf(frag); !bytes.Equal(got, concat(samples[0].plain, samples[1].plain)) {
		t.Fatal("decrypt with auto-detected 16-byte IVs failed")
	}
}
