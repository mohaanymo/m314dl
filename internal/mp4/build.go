package mp4

import "encoding/binary"

// Init synthesis. A Smooth Streaming presentation ships no init segment: the
// codec setup lives in the manifest (SPS/PPS, AudioSpecificConfig) and the
// ftyp+moov a demuxer needs must be built from it. BuildInit does that for one
// track, producing the same shape a packager's init has (so ParseInit,
// SanitizeInit, ffmpeg and MSE players all read it), and NormalizeFragment
// brings the PIFF fragments in line with it (track id, tfdt).

// InitSpec describes one track for BuildInit. Exactly one of Video/Audio is set.
type InitSpec struct {
	TrackID   uint32
	Timescale uint32
	Language  string // ISO 639-2 three-letter code; anything else becomes "und"
	Video     *VideoSpec
	Audio     *AudioSpec
	// Protection, when set, types the sample entry encv/enca with a sinf
	// (frma, schm cenc, tenc) so the CENC decryptor finds the KID and IV size in
	// the init exactly as it would in a packaged one.
	Protection *ProtectionSpec
}

type VideoSpec struct {
	Width, Height uint16
	SPS, PPS      [][]byte // raw NAL units (no start codes)
	NALLengthSize int      // bytes per NAL length prefix in the samples (1, 2 or 4)
}

type AudioSpec struct {
	Channels   uint16
	SampleRate uint32
	ASC        []byte // AudioSpecificConfig (ISO 14496-3) for the esds
}

type ProtectionSpec struct {
	KID    [16]byte
	IVSize byte // per-sample IV size: 8 or 16
}

// Box wraps the concatenated parts in a box header.
func Box(typ string, parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 8, 8+n)
	binary.BigEndian.PutUint32(out, uint32(8+n))
	copy(out[4:], typ)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func fullBoxHdr(version byte, flags uint32) []byte {
	return []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// unity is the identity transformation matrix every tkhd/mvhd carries.
var unity = func() []byte {
	m := make([]byte, 36)
	binary.BigEndian.PutUint32(m[0:], 0x00010000)
	binary.BigEndian.PutUint32(m[16:], 0x00010000)
	binary.BigEndian.PutUint32(m[32:], 0x40000000)
	return m
}()

// BuildInit synthesizes a fragmented-MP4 initialization segment (ftyp + moov)
// for one track. The moov has zero duration and an mvex/trex, as any fMP4 init.
func BuildInit(s InitSpec) []byte {
	ftyp := Box("ftyp", []byte("isom"), be32(0), []byte("isomiso6dash"))
	mvhd := Box("mvhd", fullBoxHdr(0, 0),
		be32(0), be32(0), be32(s.Timescale), be32(0), // times, timescale, duration
		be32(0x00010000), be16(0x0100), make([]byte, 10), // rate, volume, reserved
		unity, make([]byte, 24), be32(s.TrackID+1))
	trex := Box("trex", fullBoxHdr(0, 0), be32(s.TrackID), be32(1), be32(0), be32(0), be32(0))
	return append(ftyp, Box("moov", mvhd, buildTrak(s), Box("mvex", trex))...)
}

func buildTrak(s InitSpec) []byte {
	var width, height uint32
	var volume uint16
	var handler, mhd, entry []byte
	if s.Video != nil {
		width, height = uint32(s.Video.Width)<<16, uint32(s.Video.Height)<<16
		handler = []byte("vide")
		mhd = Box("vmhd", fullBoxHdr(0, 1), make([]byte, 8))
		entry = visualEntry(s.Video, s.Protection)
	} else {
		volume = 0x0100
		handler = []byte("soun")
		mhd = Box("smhd", fullBoxHdr(0, 0), make([]byte, 4))
		entry = audioEntry(s.Audio, s.Protection)
	}
	tkhd := Box("tkhd", fullBoxHdr(0, 3),
		be32(0), be32(0), be32(s.TrackID), be32(0), be32(0), // times, id, reserved, duration
		make([]byte, 8), be16(0), be16(0), be16(volume), be16(0),
		unity, be32(width), be32(height))
	mdhd := Box("mdhd", fullBoxHdr(0, 0), be32(0), be32(0), be32(s.Timescale), be32(0),
		be16(packLanguage(s.Language)), be16(0))
	hdlr := Box("hdlr", fullBoxHdr(0, 0), be32(0), handler, make([]byte, 12), []byte{0})
	dinf := Box("dinf", Box("dref", fullBoxHdr(0, 0), be32(1), Box("url ", fullBoxHdr(0, 1))))
	stbl := Box("stbl",
		Box("stsd", fullBoxHdr(0, 0), be32(1), entry),
		Box("stts", fullBoxHdr(0, 0), be32(0)),
		Box("stsc", fullBoxHdr(0, 0), be32(0)),
		Box("stsz", fullBoxHdr(0, 0), be32(0), be32(0)),
		Box("stco", fullBoxHdr(0, 0), be32(0)))
	minf := Box("minf", mhd, dinf, stbl)
	return Box("trak", tkhd, Box("mdia", mdhd, hdlr, minf))
}

// visualEntry builds an avc1 (or encv) VisualSampleEntry with its avcC.
func visualEntry(v *VideoSpec, prot *ProtectionSpec) []byte {
	// SampleEntry(8) + VisualSampleEntry fields(70) = the 78-byte prefix
	// sampleEntryChildren expects before child boxes.
	prefix := concatBytes(make([]byte, 6), be16(1),
		make([]byte, 16), be16(v.Width), be16(v.Height),
		be32(0x00480000), be32(0x00480000), be32(0), be16(1),
		make([]byte, 32), be16(0x0018), be16(0xffff))
	typ := "avc1"
	children := avcC(v)
	if prot != nil {
		typ = "encv"
		children = append(children, sinf("avc1", prot)...)
	}
	return Box(typ, prefix, children)
}

func avcC(v *VideoSpec) []byte {
	var profile, compat, level byte
	if len(v.SPS) > 0 && len(v.SPS[0]) >= 4 {
		profile, compat, level = v.SPS[0][1], v.SPS[0][2], v.SPS[0][3]
	}
	nalLen := v.NALLengthSize
	if nalLen != 1 && nalLen != 2 {
		nalLen = 4
	}
	p := []byte{1, profile, compat, level, 0xfc | byte(nalLen-1), 0xe0 | byte(len(v.SPS))}
	for _, s := range v.SPS {
		p = append(p, be16(uint16(len(s)))...)
		p = append(p, s...)
	}
	p = append(p, byte(len(v.PPS)))
	for _, s := range v.PPS {
		p = append(p, be16(uint16(len(s)))...)
		p = append(p, s...)
	}
	return Box("avcC", p)
}

// audioEntry builds an mp4a (or enca) AudioSampleEntry with its esds.
func audioEntry(a *AudioSpec, prot *ProtectionSpec) []byte {
	// SampleEntry(8) + AudioSampleEntry v0 fields(20) = the 28-byte prefix.
	prefix := concatBytes(make([]byte, 6), be16(1), make([]byte, 8),
		be16(a.Channels), be16(16), be16(0), be16(0), be32(a.SampleRate<<16))
	typ := "mp4a"
	children := esds(a.ASC)
	if prot != nil {
		typ = "enca"
		children = append(children, sinf("mp4a", prot)...)
	}
	return Box(typ, prefix, children)
}

// esds wraps an AudioSpecificConfig in the MPEG-4 descriptor chain
// (ES → DecoderConfig(AAC) → DecoderSpecificInfo, then SLConfig).
func esds(asc []byte) []byte {
	desc := func(tag byte, body []byte) []byte {
		return append([]byte{tag, byte(len(body))}, body...)
	}
	dsi := desc(0x05, asc)
	dcd := desc(0x04, concatBytes([]byte{0x40, 0x15}, make([]byte, 11), dsi)) // AAC, audio stream
	es := desc(0x03, concatBytes(be16(0), []byte{0}, dcd, desc(0x06, []byte{0x02})))
	return Box("esds", fullBoxHdr(0, 0), es)
}

// sinf is the protection scheme info for an encv/enca entry: original format,
// cenc scheme, and the track encryption defaults.
func sinf(orig string, prot *ProtectionSpec) []byte {
	tenc := Box("tenc", fullBoxHdr(0, 0), []byte{0, 0, 1, prot.IVSize}, prot.KID[:])
	return Box("sinf",
		Box("frma", []byte(orig)),
		Box("schm", fullBoxHdr(0, 0), []byte("cenc"), be32(0x00010000)),
		Box("schi", tenc))
}

// packLanguage encodes an ISO 639-2 code into the 15-bit mdhd form.
func packLanguage(lang string) uint16 {
	if len(lang) != 3 {
		lang = "und"
	}
	var v uint16
	for i := 0; i < 3; i++ {
		c := lang[i] | 0x20 // lower-case
		if c < 'a' || c > 'z' {
			return packLanguage("und")
		}
		v = v<<5 | uint16(c-0x60)
	}
	return v
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// SoleTrackID returns the track_ID of an init segment that carries exactly one
// track, or 0 (no moov, or a muxed multi-track init).
func SoleTrackID(init []byte) uint32 {
	moov, ok := find(init, "moov")
	if !ok {
		return 0
	}
	var id uint32
	n := 0
	moov.children(func(b box) bool {
		if b.typ != "trak" {
			return true
		}
		n++
		if tkhd, ok := find(b.payload, "tkhd"); ok {
			version, _, rest, err := fullBox(tkhd.payload)
			off := 8 // v0: creation(4) modification(4)
			if version == 1 {
				off = 16
			}
			if err == nil && len(rest) >= off+4 {
				id = binary.BigEndian.Uint32(rest[off:])
			}
		}
		return true
	})
	if n != 1 {
		return 0
	}
	return id
}

// PIFF extended box types (Smooth Streaming fragments).
var (
	uuidTfxd     = []byte{0x6d, 0x1d, 0x9b, 0x05, 0x42, 0xd5, 0x44, 0xe6, 0x80, 0xe2, 0x14, 0x1d, 0xaf, 0xf7, 0x57, 0xb2}
	uuidPiffSenc = []byte{0xa2, 0x39, 0x4f, 0x52, 0x5a, 0x9b, 0x4f, 0x14, 0xa2, 0x44, 0x6c, 0x42, 0x7c, 0x64, 0x8d, 0xf4}
)

func isUUID(b box, ext []byte) bool {
	return b.typ == "uuid" && len(b.payload) >= 16 && string(b.payload[:16]) == string(ext)
}

// NormalizeFragment makes a media fragment consistent with a single-track init:
// every tfhd's track_ID becomes trackID, and a traf that has no tfdt but a PIFF
// tfxd gets a tfdt built from it (players and MSE need the decode time; ffmpeg
// tolerates its absence, browsers do not). A fragment that already matches is
// returned unchanged. Only single-traf moofs are retagged: a muxed fragment
// against a one-track init is broken either way and is left alone.
//
// Inserting a tfdt grows the moof, so every trun's data_offset (measured from
// the moof start) is bumped by the same amount, as StripFragmentProtection does
// in the other direction.
func NormalizeFragment(frag []byte, trackID uint32) []byte {
	changed := false
	out := rebuildSeq(frag, func(b box) ([]byte, bool) {
		if b.typ != "moof" {
			return nil, false
		}
		trafs, added := 0, 0
		walk(b.payload, func(c box) bool {
			if c.typ == "traf" {
				trafs++
				if needsTfdt(c) {
					added += 8 + 4 + 8 // tfdt v1 box
				}
			}
			return true
		})
		if added == 0 && (trackID == 0 || trafs != 1 || trafTrackID(b) == trackID) {
			return nil, false
		}
		changed = true
		return Box("moof", rebuildSeq(b.payload, func(c box) ([]byte, bool) {
			if c.typ != "traf" {
				return nil, false
			}
			var tfxdTime uint64
			var haveTfxd bool
			c.children(func(d box) bool {
				if isUUID(d, uuidTfxd) {
					tfxdTime, _, haveTfxd = piffTime(d)
					return false
				}
				return true
			})
			insertTfdt := needsTfdt(c) && haveTfxd
			return Box("traf", rebuildSeq(c.payload, func(d box) ([]byte, bool) {
				switch d.typ {
				case "tfhd":
					p := append([]byte(nil), d.payload...)
					if trackID != 0 && trafs == 1 && len(p) >= 8 {
						binary.BigEndian.PutUint32(p[4:], trackID)
					}
					out := Box("tfhd", p)
					if insertTfdt {
						out = append(out, Box("tfdt", fullBoxHdr(1, 0), be64(tfxdTime))...)
					}
					return out, true
				case "trun":
					if added != 0 {
						return patchTrunDataOffset(d, -added), true
					}
				}
				return nil, false
			})), true
		})), true
	})
	if !changed {
		return frag // the packaged-fragment norm: no copy on the hot path
	}
	return out
}

// needsTfdt reports whether a traf lacks tfdt but carries a PIFF tfxd to build
// one from.
func needsTfdt(traf box) bool {
	hasTfdt, hasTfxd := false, false
	traf.children(func(d box) bool {
		switch {
		case d.typ == "tfdt":
			hasTfdt = true
		case isUUID(d, uuidTfxd):
			hasTfxd = true
		}
		return true
	})
	return !hasTfdt && hasTfxd
}

func trafTrackID(moof box) uint32 {
	tfhd, ok := find(moof.payload, "traf", "tfhd")
	if !ok || len(tfhd.payload) < 8 {
		return 0
	}
	return binary.BigEndian.Uint32(tfhd.payload[4:])
}

// piffTime reads a tfxd box: fragment absolute time and duration in the track
// timescale (32-bit in version 0, 64-bit in version 1).
func piffTime(b box) (t, d uint64, ok bool) {
	version, _, rest, err := fullBox(b.payload[16:])
	if err != nil {
		return 0, 0, false
	}
	if version == 1 {
		if len(rest) < 16 {
			return 0, 0, false
		}
		return binary.BigEndian.Uint64(rest), binary.BigEndian.Uint64(rest[8:]), true
	}
	if len(rest) < 8 {
		return 0, 0, false
	}
	return uint64(binary.BigEndian.Uint32(rest)), uint64(binary.BigEndian.Uint32(rest[4:])), true
}
