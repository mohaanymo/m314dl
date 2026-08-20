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
segments/track), served from localhost with a simulated 50 ms CDN round-trip,
best-of-3, all outputs verified full-length (155 MB). Lower is better.

| Case | **m314dl** | vsd 0.5.0 | N_m3u8DL-RE 0.6.0 |
|---|---|---|---|
| HLS TS (VOD) | **0.95 s** | 1.60 s | 1.12 s |
| HLS fMP4 (VOD) | **0.96 s** | 1.62 s | 1.08 s |
| DASH clear (VOD) | **0.91 s** | 1.85 s | 1.45 s |
| DASH CENC — AES-CTR | **0.98 s** | 1.95 s | 2.69 s |
| DASH cbcs — AES-CBC pattern | **0.99 s** | 1.88 s | 1.82 s |

Three things to notice:

- **m314dl is flat.** Encryption adds ~0.07 s, because segments are decrypted
  in memory *as they download* — the decrypt overlaps the next fetch and never
  touches disk. Competitors pay ~1 s or more for the same content because they
  decrypt in a separate pass (N_m3u8DL-RE shells out to `mp4decrypt` after the
  merge; see below), or download streams strictly one at a time (vsd).
- **Concurrency auto-tunes.** With no `-t` flag, m314dl ramps its in-flight
  request count to fit the network (AIMD, 4→64) and backs off from rate-limited
  servers (honoring `Retry-After`) — hiding round-trip time without a knob to
  guess. `-t N` uses exactly N and holds it (matching `--thread-count`), backing
  off only on real rate-limit pressure, then climbing straight back.
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
- **Adaptive concurrency**: with no `-t`, in-flight requests auto-tune to the network (AIMD, 4→64) and back off from 429/5xx (honoring `Retry-After`) — no thread count to guess, and no hammering rate-limited CDNs; `-t N` pins exactly N (held, still backs off on rate limits)
- **One TCP connection per in-flight request (HTTP/1.1)**: a CDN offering HTTP/2 would make Go multiplex every concurrent segment onto a single TCP connection, and one flow can't fill a real network's pipe (RTT- and loss-bound) — so downloads run over many independent HTTP/1.1 connections, the way `N_m3u8DL-RE`/`aria2`/`yt-dlp` do, for full aggregate throughput
- **One download pipeline** for VOD and live (feeder → worker pool → ordered writer): no duplicated code paths; segments stream to disk (one part file per in-flight segment, decrypted and appended in order) so peak memory is one segment, not one per thread — even for huge segments
- **Resume that survives a kill, not just a clean exit**: segments stream to part files with HTTP byte-range resume, and the checkpoint is written on every commit — so an interrupted (or hard-killed, e.g. a TUI where Ctrl-C is a keypress) VOD download continues byte-exact, losing at most the one segment being written. Works even when a whole movie is a handful of tens-of-MB segments; a failed segment is never silently skipped
- **Retries with exponential backoff + jitter** that cover mid-body read failures, not just connection setup; status-aware (404 fails fast, 5xx/429 retry)
- **Subtitles**: WebVTT (concatenated segments deduped), TTML→SRT (lenient regex parsing — survives non-compliant XML), stpp-in-fMP4 extracted natively (no ffmpeg TTML gap), muxed with correct ISO 639-2 language tags — or written as sidecar files with `-sub-external`
- **Flexible input**: an HTTP(S) URL (first *or* last argument), a local `.m3u8`/`.mpd` file or `file://` path (for manifests signed per request and never published), or a web page to scrape
- **Page scraping**: point it at a web page; it finds `.m3u8`/`.mpd` URLs (inline JSON and one iframe level included)
- **Automation-friendly**: plain-line progress on non-TTY (no ANSI garbage in logs), real exit codes, quiet machine-readable output
- Ad-segment skipping by regex (`-ad-keyword`, applied on live refreshes too), custom headers (sent verbatim), Netscape `cookies.txt`, HTTP/SOCKS proxy with auth

**Scope of decryption.** m314dl decrypts when you provide the key. It does not
run a license/CDM handshake, and HLS SAMPLE-AES and HLS-CMAF CENC key parsing
are not wired up yet (DASH CENC/cbcs is). TLS verification is on by default and
only skipped with an explicit `-insecure`.

## Install

```sh
go build -o m314dl .
```

Requires `ffmpeg` on PATH (or next to the binary, or passed with `-ffmpeg <path>`) for muxing.

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

## RPC server (remote downloads)

Run m314dl on a server and submit jobs over HTTP/JSON. Each job is a child
m314dl process, so every flag, resume, and graceful-stop behavior works
exactly as it does locally.

```bash
# on the server (a secret is required on non-loopback binds)
m314dl -rpc 0.0.0.0:8314 -rpc-secret mytoken

# submit a job (args = normal CLI flags)
curl -H 'Authorization: Bearer mytoken' -X POST http://server:8314/add \
  -d '{"url":"https://example.com/master.m3u8","args":["-o","movie.mp4","-key","KID:KEY"]}'
# → {"id":1}

# watch: state, latest progress line, error if any
curl -H 'Authorization: Bearer mytoken' http://server:8314/jobs
curl -H 'Authorization: Bearer mytoken' http://server:8314/jobs/1   # + full log

# stop gracefully (live: mux what's recorded; VOD: save resume state);
# call twice to abort — same semantics as Ctrl-C
curl -H 'Authorization: Bearer mytoken' -X POST http://server:8314/jobs/1/stop
```

Output files land in the server's working directory unless `-o` gives a path.
An authenticated client has the full power of the CLI on the server — treat
the secret like an SSH key.

## Restream: live HLS out (`-serve`)

Instead of writing a file, m314dl can republish what it downloads as a **live
HLS stream** over HTTP — decrypt a DRM source and re-serve it in the clear, pull
one feed and fan it out to many viewers, or turn a live recording into a
watchable endpoint.

```bash
# pull a stream and re-serve it as live HLS
m314dl -serve :8314 https://example.com/master.m3u8
# → http://localhost:8314/live.m3u8

# decrypt a DRM DASH source and restream it in the clear
m314dl -serve :8314 -key KID:KEY https://example.com/manifest.mpd

# re-serve as one continuous MPEG-TS instead (VLC, set-top boxes)
m314dl -serve :8314 -serve-format ts https://example.com/live.m3u8
# → http://localhost:8314/live.ts

# re-serve as live DASH (dash.js, Shaka, ExoPlayer)
m314dl -serve :8314 -serve-format dash https://example.com/manifest.mpd
# → http://localhost:8314/live.mpd
```

Open `http://<host>:8314/live.m3u8` (HLS), `/live.ts` (MPEG-TS), or `/live.mpd`
(DASH) in VLC, mpv, hls.js, dash.js, or any player.

What it serves:

HLS mode serves:

- `GET /live.m3u8` — multivariant (master) playlist
- `GET /{track}/index.m3u8` — a track's media playlist (`video`, `audio-en`, …)
- `GET /{track}/init.mp4` — fMP4 init segment (EXT-X-MAP target)
- `GET /{track}/000123.ts` — a media segment

MPEG-TS mode serves `GET /live.ts` — one never-ending transport stream fanned
out to every viewer. The fan-out is non-blocking: a viewer that falls a full
buffer behind is dropped, and no other viewer ever waits on it (unlike a naive
broadcaster that stalls everyone on the slowest client).

- **TS source → pure Go, no FFmpeg.** The segments are already MPEG-TS, so they
  concatenate into a valid continuous stream with no re-mux; each viewer joins on
  a segment boundary (a clean, decodable PAT/PMT + keyframe start). Continuity
  counters are renumbered into one seamless per-PID sequence across boundaries —
  the one good idea from FFmpeg restreamers, done in-process with no reconnect
  seam to bridge.
- **fMP4 source, or separate video+audio → one FFmpeg remux.** When the source
  isn't already a muxed TS, m314dl runs a single long-lived FFmpeg that
  copy-remuxes the decrypted tracks into TS (separate audio is muxed in). One
  process for the whole stream — not one per segment — and no `-re` pacing
  (segment arrival paces it). Because m314dl decrypts upstream, FFmpeg only sees
  a clear copy, so the DRM-demuxer memory leak that plagues FFmpeg restreamers
  never applies. Pass `-serve-transcode '<ffmpeg args>'` to re-encode instead of
  copy (e.g. `-c:v libx264 -preset veryfast -c:a aac`).

DASH mode serves `GET /live.mpd` — a SegmentTemplate + SegmentTimeline manifest
plus the same in-memory fMP4 segments (one AdaptationSet per media type, one
Representation per track, each with its own timeline built from real segment
durations). A live source produces a `dynamic` MPD with `minimumUpdatePeriod`; a
finite one produces a `static` MPD with `mediaPresentationDuration`. Needs an
fMP4 source (a TS source must be remuxed first — a later phase).

How it's built — and why it's different from an FFmpeg restreamer:

- **No FFmpeg on the copy path.** Segments arrive already decrypted and in
  playback order from the normal download engine; the packager just keeps a
  rolling in-memory window and rewrites the playlist. No subprocess, no `-re`
  pacing (the source's own segment cadence paces it), no TS continuity-counter
  surgery (segments are copied whole, never re-muxed). Output segments are
  **byte-identical** to the source.
- **One copy feeds every viewer.** Segments are held once in a shared window and
  served with Range/ETag support, not buffered per connection.
- **Correct playlists.** Right `EXT-X-VERSION` for fMP4 vs TS, `EXT-X-MAP` for
  fMP4, real `BANDWIDTH`/`CODECS`/`RESOLUTION` in the master (measured, not
  guessed), source discontinuities passed through, and an output media sequence
  that can't be wedged by a source that rewinds its own numbering.
- **Live and VOD.** A live source rolls a window; a finite source publishes the
  whole thing and caps it with `EXT-X-ENDLIST`, then keeps serving until Ctrl-C.

Scope today: HLS and DASH output are copy-only (same container family, no
FFmpeg); MPEG-TS output copies a TS source in pure Go and remuxes an fMP4 or
separate-audio source through one FFmpeg (with optional transcode). TS→fMP4 for
DASH/HLS-fMP4 output, and subtitle restreaming, are not wired up yet.

## Selector syntax

`-sv`, `-sa`, `-ss` accept:

| Value | Meaning |
|---|---|
| `best`, `worst`, `all`, `best3` | positional over sorted streams |
| `key=regex[:key=regex...]` | filter; keeps **all** matches |
| `key!=regex` | negate — keep streams that do **not** match (RE2 has no negative lookahead, so this is how you express a deny-list) |
| `...:for=bestN` | take N best from the filtered set |
| `none` | drop this type |

Regex keys: `id`, `lang`, `name`, `codecs`, `res`, `range`, `channel`.
Numeric keys: `bwmin`/`bwmax` (kbps), `segsmin`/`segsmax` (segment count),
`plistdurmin`/`plistdurmax` (seconds, or `20m`). Numeric filters treat an
unknown value as passing, so `segsmin`/`plistdurmin` drop short ad/bumper
periods in DASH without touching HLS tracks whose length isn't known yet.
Sorting: video by height→bandwidth, audio by default-flag→channels→bandwidth.

```sh
# drop 1-segment SSAI bumpers, keep ≥720p AVC content
m314dl -sv 'segsmin=2:plistdurmin=20m:res=x720$:codecs=avc1|avc3:for=best' URL
# drop two mislabeled subtitle tracks by id (deny-list via negation)
m314dl -ss 'id!=^(a|b)$:for=all' URL
```

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

## License

MIT — see [LICENSE](LICENSE).
