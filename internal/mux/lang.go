package mux

import "strings"

// iso639_2 maps 2-letter codes to the 3-letter codes MP4 requires
// (2-letter tags are silently dropped by ffmpeg's mp4 muxer).
var iso639_2 = map[string]string{
	"aa": "aar", "ab": "abk", "af": "afr", "am": "amh", "ar": "ara",
	"as": "asm", "az": "aze", "be": "bel", "bg": "bul", "bn": "ben",
	"bs": "bos", "ca": "cat", "cs": "ces", "cy": "cym", "da": "dan",
	"de": "deu", "el": "ell", "en": "eng", "eo": "epo", "es": "spa",
	"et": "est", "eu": "eus", "fa": "fas", "fi": "fin", "fil": "fil",
	"fr": "fra", "ga": "gle", "gl": "glg", "gu": "guj", "he": "heb",
	"hi": "hin", "hr": "hrv", "hu": "hun", "hy": "hye", "id": "ind",
	"is": "isl", "it": "ita", "iw": "heb", "ja": "jpn", "ka": "kat",
	"kk": "kaz", "km": "khm", "kn": "kan", "ko": "kor", "ku": "kur",
	"ky": "kir", "lo": "lao", "lt": "lit", "lv": "lav", "mk": "mkd",
	"ml": "mal", "mn": "mon", "mr": "mar", "ms": "msa", "mt": "mlt",
	"my": "mya", "ne": "nep", "nl": "nld", "no": "nor", "pa": "pan",
	"pl": "pol", "ps": "pus", "pt": "por", "ro": "ron", "ru": "rus",
	"si": "sin", "sk": "slk", "sl": "slv", "sq": "sqi", "sr": "srp",
	"sv": "swe", "sw": "swa", "ta": "tam", "te": "tel", "th": "tha",
	"tl": "tgl", "tr": "tur", "uk": "ukr", "ur": "urd", "uz": "uzb",
	"vi": "vie", "zh": "zho", "zu": "zul",
}

// lang639 converts a BCP-47 tag ("en-US", "es") to ISO 639-2 ("eng", "spa").
// Unknown values pass through unchanged.
func lang639(tag string) string {
	if tag == "" {
		return ""
	}
	base := strings.ToLower(tag)
	if i := strings.IndexAny(base, "-_"); i > 0 {
		base = base[:i]
	}
	if len(base) == 3 {
		return base
	}
	if v, ok := iso639_2[base]; ok {
		return v
	}
	return tag
}
