# Benchmarks

Reproducible, self-hosted comparison of `m314dl` against
[vsd](https://github.com/clitic/vsd) and
[N_m3u8DL-RE](https://github.com/nilaoda/N_m3u8DL-RE). Everything is served from
localhost so the numbers measure the *tools*, not a flaky CDN.

## Why a local server

A downloader's job is to hide per-request round-trip time behind concurrency.
On a zero-latency localhost that skill is invisible — so `bench/server` injects
a fixed first-byte delay (`-latency`) to simulate a real CDN's RTT. That is the
axis on which segment downloaders actually differ.

## Corpus

`gen-fixtures.sh` encodes one 5-minute 1080p clip (H.264 4 Mbps + AAC, ~150 MB,
75 four-second segments) and packages it six ways:

| Fixture | What it exercises |
|---|---|
| `hls-ts` | classic MPEG-TS HLS |
| `hls-fmp4` | fragmented-MP4 HLS (CMAF) |
| `dash-clear` | plain DASH |
| `dash-cenc` | DASH + CENC (AES-128 **CTR**) — native decryption |
| `dash-cbcs` | DASH + cbcs (AES-128 **CBC** pattern) — native decryption |

## Run it

```sh
# 1. build the tool
(cd .. && go build -o m314dl .)

# 2. build the corpus (needs ffmpeg + shaka-packager)
./gen-fixtures.sh fixtures

# 3. serve it with a simulated 50 ms RTT
go run ./server -root fixtures -port 8090 -latency 50ms &

# 4. run the suite (point at your competitor binaries)
M314=../m314dl VSD=$(which vsd) RE=$(which N_m3u8DL-RE) RUNS=4 ./bench.sh all
```

Each case runs `RUNS` times and reports the best wall-clock (standard best-of-N
to filter scheduler noise). Every output is validated with `ffprobe`; a tool
that fails or produces an unplayable file is marked `FAILED`/`INVALID` rather
than counted as fast.

## Method notes

- **Fair flags:** all tools get `-t/--thread-count 16` and mux to MP4 via
  ffmpeg. Competitor invocations are in `bench.sh` — inspect and adjust freely.
- **Warm cache:** fixtures are served from RAM, so disk isn't a variable.
- **Correctness is separate:** decryption is proven byte-exact against
  `mp4decrypt` and by hermetic round-trip in `internal/mp4` tests
  (`go test ./internal/mp4/`), not asserted by this timing harness.
- Numbers depend on CPU, core count and the chosen latency. Re-run locally
  rather than trusting a copied table.
