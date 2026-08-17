# m314dl — fast HLS & DASH video downloader with native CENC decryption

**m314dl** is a fast, reliable **HLS (`.m3u8`) and DASH (`.mpd`) video stream
downloader** in a single static Go binary — with **native, in-process CENC /
Widevine-style decryption**. No `mp4decrypt`, no `shaka-packager`, no external
tools on the download path.

If you have reached for [N_m3u8DL-RE](https://github.com/nilaoda/N_m3u8DL-RE),
[vsd](https://github.com/clitic/vsd), `youtube-dl`/`yt-dlp` or `ffmpeg` to grab
an `m3u8` or `mpd` stream, m314dl is a single-binary alternative that is
faster on every benchmark below and decrypts DRM-protected fMP4 in the same pass
it downloads it — no second `mp4decrypt` step.

```sh
m314dl -o out.mp4 https://example.com/master.m3u8            # download an HLS stream
m314dl -key KID:KEY -o out.mp4 https://example.com/video.mpd # download + decrypt a DASH stream
```

## Benchmarks

Wall-clock to download **+ decrypt + mux** a 5-minute 1080p stream (~150 MB, 75
segments), served from localhost with a simulated 50 ms CDN round-trip, 16
threads, best-of-4. Lower is better; every output was validated with `ffprobe`.

| Case | **m314dl** | vsd 0.5.0 | N_m3u8DL-RE 0.6.0 |
|---|---|---|---|
| HLS TS (VOD) | **1.30 s** | 2.19 s | 1.37 s |
| HLS fMP4 (VOD) | **1.33 s** | 2.23 s | 1.40 s |
| DASH clear (VOD) | **1.32 s** | 2.68 s | 1.82 s |
| DASH CENC — AES-CTR | **1.39 s** | 2.91 s | 3.26 s |
| DASH cbcs — AES-CBC pattern | **1.39 s** | 2.91 s | 2.25 s |

Two things to notice:

- **m314dl is flat.** Encryption adds ~0.07 s, because segments are decrypted
  in memory *as they download* — the decrypt overlaps the next fetch and never
  touches disk. Competitors pay 1–2 s for the same content because they decrypt
  in a separate pass (N_m3u8DL-RE shells out to `mp4decrypt` after the merge;
  see below), or download streams strictly one at a time (vsd).
- **The numbers are reproducible.** The corpus generator, the throttling
  server, and the exact commands live in [`bench/`](bench/). Re-run them on your
  own hardware — don't trust a copied table.

Decryption throughput on its own is **1.4 GB/s (CTR)** / **1.05 GB/s (CBC)**
single-threaded (`go test -bench . ./internal/mp4/`) — ~100× typical network
bandwidth, across every worker thread. Decryption is free; the network is the
only bottleneck.

## Decryption is in-process and on the fly

Most tools treat DRM as a post-processing step: download the whole encrypted
file, then run `mp4decrypt`/`shaka-packager` over it. m314dl decrypts each
fragment in the download worker, in RAM, the moment it arrives — the same place
it handles AES-128. Consequences:

- no encrypted temp file, no second full pass over the data, no subprocess spawn
  per segment;
- decryption is concurrent across all threads and overlapped with I/O, so it
  costs effectively nothing (see the flat benchmark row);
- **CENC** (`cenc` AES-CTR), **cbcs** (AES-CBC + crypt/skip pattern, constant
  IV), plus `cens`/`cbc1` are implemented from the ISO/IEC 23001-7 boxes
  (`tenc`, `senc`, `saiz`/`saio`, `trun`) — verified **byte-exact against
  `mp4decrypt`** and by hermetic round-trip tests.

You supply the content key; m314dl does not include a Widevine/PlayReady CDM
(neither does vsd's or N_m3u8DL-RE's default download path — key extraction is a
separate concern):

```sh
# KID:KEY in hex (KID dashes optional); repeatable for multi-key manifests
m314dl -key 00112233445566778899aabbccddeeff:0123456789abcdef0123456789abcdef \
       -o out.mp4 https://example.com/manifest.mpd
```

## Features

- **HLS** (TS + fMP4): master/media playlists, rendition groups (`AUDIO=`/`SUBTITLES=` honored — best audio belongs to the picked variant), byte ranges, `EXT-X-MAP` changes mid-stream, AES-128 (in-process, `data:` key URIs supported), audio-only variant detection
- **DASH**: SegmentTemplate (`$Number$`/`$Time$` + `%0Nd`), SegmentTimeline (negative `@r`), SegmentList, SegmentBase, multi-period (merged with discontinuity markers), namespace-agnostic lenient XML, `cenc:default_KID`
- **Native CENC / cbcs / cens / cbc1 decryption** — no `mp4decrypt`, no `shaka-packager`; per-fragment, in memory, byte-exact vs the reference tools
- **Live recording** for both protocols: starts at the live edge (`-live-from-start` for the whole DVR window), refresh failures retried forever, segment dedupe across refreshes, `-live-duration` limit, **Ctrl-C finishes the recording gracefully and muxes** — you never lose what you already recorded
- **One download pipeline** for VOD and live (feeder → worker pool → ordered writer): no duplicated code paths, no fd-per-segment merge ("too many open files" is structurally impossible — output is a single streaming append)
- **Resume**: interrupted VOD downloads continue exactly where they stopped (byte-exact; a failed segment can never be silently skipped)
- **Retries with exponential backoff + jitter** that cover mid-body read failures, not just connection setup; status-aware (404 fails fast, 5xx/429 retry)
- **Subtitles**: WebVTT (concatenated segments deduped), TTML→SRT (lenient regex parsing — survives non-compliant XML), stpp-in-fMP4 extracted natively (no ffmpeg TTML gap), muxed with correct ISO 639-2 language tags
- **Page scraping**: point it at a web page; it finds `.m3u8`/`.mpd` URLs (inline JSON and one iframe level included)
- **Automation-friendly**: plain-line progress on non-TTY (no ANSI garbage in logs), real exit codes, quiet machine-readable output
- Ad-segment skipping by regex (`-ad-keyword`, applied on live refreshes too), custom headers (sent verbatim), Netscape `cookies.txt`, HTTP/SOCKS proxy with auth, HTTP/2

**Scope of decryption.** m314dl decrypts when you provide the key. It does not
run a license/CDM handshake, and HLS SAMPLE-AES and HLS-CMAF CENC key parsing
are not wired up yet (DASH CENC/cbcs is). TLS verification is on by default and
only skipped with an explicit `-insecure`.

## Install

```sh
go build -o m314dl .
```

Requires `ffmpeg` on PATH (or next to the binary) for muxing.

## Usage

```sh
# best video + best audio per language + all subtitles → out.mp4
m314dl -o out.mp4 https://example.com/master.m3u8

# decrypt a CENC/cbcs DASH stream with a known key (native, no mp4decrypt)
m314dl -key KID:KEY -o out.mp4 https://example.com/manifest.mpd

# list streams
m314dl -list https://example.com/manifest.mpd

# pick streams: filters are regexes, colon-separated; for= picks from matches
m314dl -sv 'res=1080' -sa 'lang=^en' -ss 'lang=en|de' -o out.mkv URL
m314dl -sv best -sa 'for=best2' URL

# record a live stream for 1 hour (or Ctrl-C anytime — file still muxed)
m314dl -live-duration 1h -o show.mp4 https://example.com/live.m3u8

# scrape a page for the stream
m314dl -o video.mp4 https://example.com/watch/12345

# resume an interrupted VOD download: just rerun the same command
```

Run `m314dl -h` for all flags.

## Selector syntax

`-sv`, `-sa`, `-ss` accept:

| Value | Meaning |
|---|---|
| `best`, `worst`, `all`, `best3` | positional over sorted streams |
| `key=regex[:key=regex...]` | filter; keeps **all** matches |
| `...:for=bestN` | take N best from the filtered set |
| `none` | drop this type |

Keys: `id`, `lang`, `name`, `codecs`, `res`, `channel`, `bwmin` (kbps), `bwmax`.
Sorting: video by height→bandwidth, audio by default-flag→channels→bandwidth.

## FAQ

### How do I download an HLS (`.m3u8`) stream?

`m314dl -o out.mp4 https://host/master.m3u8`. m314dl fetches the master
playlist, picks the best video + audio + all subtitles by default, downloads
the segments in parallel and muxes to MP4 with ffmpeg.

### How do I download a DASH (`.mpd`) stream?

Same command, with the MPD URL: `m314dl -o out.mp4 https://host/manifest.mpd`.
SegmentTemplate, SegmentTimeline, SegmentList, SegmentBase and multi-period
manifests are all supported.

### How do I decrypt a CENC / DRM-protected stream without mp4decrypt?

Pass the content key: `m314dl -key KID:KEY -o out.mp4 https://host/manifest.mpd`.
Decryption (`cenc` AES-CTR and `cbcs` AES-CBC pattern) runs in-process, per
fragment, as the stream downloads — there is no `mp4decrypt` or `shaka-packager`
step. m314dl does not perform the license/CDM handshake, so you supply the key.

### How do I record a live stream?

`m314dl -live-duration 1h -o show.mp4 https://host/live.m3u8`. Recording starts
at the live edge; press Ctrl-C anytime and the file is still finalized and muxed.

### Is it faster than N_m3u8DL-RE or vsd?

On the reproducible benchmark above, yes — across HLS, DASH and encrypted
content. See [`bench/`](bench/) to run it yourself.

## Tests

```sh
go test ./...                      # unit + integration
go test -bench . ./internal/mp4/   # decryption throughput
```

The CENC test suite proves decryption two independent ways: **byte-exact
parity** with `mp4decrypt` on real shaka-packaged fixtures (skips if the fixtures
or `mp4decrypt` are absent), and **hermetic round-trips** that build fragments
in-code and recover the plaintext with no external dependencies.
