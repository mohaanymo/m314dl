#!/usr/bin/env bash
# gen-fixtures.sh — build the benchmark corpus: a 5-minute 1080p clip encoded
# once, then segmented into every packaging the benchmark exercises.
#
# Requires ffmpeg and shaka-packager on PATH.
#   ./gen-fixtures.sh [OUTDIR]   (default: ./fixtures)
set -euo pipefail

OUT=${1:-fixtures}
DUR=${DUR:-300}
KID=00112233445566778899aabbccddeeff
KEY=0123456789abcdef0123456789abcdef
mkdir -p "$OUT"/src
cd "$OUT"

if [ ! -f src/source.mp4 ]; then
  echo "== encoding ${DUR}s 1080p source =="
  ffmpeg -y -loglevel error \
    -f lavfi -i "testsrc2=size=1920x1080:rate=30:duration=$DUR" \
    -f lavfi -i "sine=frequency=440:duration=$DUR" \
    -c:v libx264 -preset veryfast -b:v 4M -g 120 -keyint_min 120 -sc_threshold 0 \
    -c:a aac -b:a 128k src/source.mp4
fi

echo "== HLS TS =="
mkdir -p hls-ts
ffmpeg -y -loglevel error -i src/source.mp4 -c copy -f hls -hls_time 4 \
  -hls_playlist_type vod -hls_flags independent_segments \
  -hls_segment_filename 'hls-ts/seg%04d.ts' hls-ts/media.m3u8

echo "== HLS fMP4 =="
mkdir -p hls-fmp4
ffmpeg -y -loglevel error -i src/source.mp4 -c copy -f hls -hls_time 4 \
  -hls_playlist_type vod -hls_segment_type fmp4 \
  -hls_segment_filename 'hls-fmp4/seg%04d.m4s' hls-fmp4/media.m3u8

pkg() { # pkg <dir> [extra shaka args...]
  local dir=$1; shift
  mkdir -p "$dir"
  shaka-packager \
    "in=src/source.mp4,stream=video,init_segment=$dir/v_init.mp4,segment_template=$dir/v_\$Number\$.m4s" \
    "in=src/source.mp4,stream=audio,init_segment=$dir/a_init.mp4,segment_template=$dir/a_\$Number\$.m4s" \
    --segment_duration 4 --generate_static_live_mpd "$@" --mpd_output "$dir/manifest.mpd"
}

echo "== DASH clear =="
pkg dash-clear

echo "== DASH CENC (AES-CTR) =="
pkg dash-cenc --enable_raw_key_encryption \
  --keys "label=:key_id=$KID:key=$KEY" --protection_scheme cenc --clear_lead 0

echo "== DASH cbcs (AES-CBC pattern) =="
pkg dash-cbcs --enable_raw_key_encryption \
  --keys "label=:key_id=$KID:key=$KEY" --protection_scheme cbcs --clear_lead 0

echo "done: $OUT"
