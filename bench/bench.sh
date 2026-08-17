#!/usr/bin/env bash
# bench.sh — reproducible wall-clock comparison of m314dl vs vsd vs N_m3u8DL-RE
# over the fixtures served by bench/server. Each case runs RUNS times; the best
# (min) time is reported. Output is validated with ffprobe so a tool that
# errors out cannot appear to "win".
#
# Prereqs: fixtures generated (./gen-fixtures.sh), server running
# (go run ./server -root fixtures -latency 50ms), and the three binaries built.
#
#   M314=../m314dl VSD=$(which vsd) RE=$(which N_m3u8DL-RE) ./bench.sh all
set -u

BASE=${BASE:-http://127.0.0.1:8090}
M314=${M314:-../m314dl}
VSD=${VSD:-vsd}
RE=${RE:-N_m3u8DL-RE}
WORK=${WORK:-$(mktemp -d)}
KID=00112233445566778899aabbccddeeff
KEY=0123456789abcdef0123456789abcdef
RUNS=${RUNS:-4}
THREADS=${THREADS:-16}

mkdir -p "$WORK"; cd "$WORK" || exit 1
valid() { ffprobe -v error -show_entries format=duration -of csv=p=0 "$1" 2>/dev/null | grep -qE '[0-9]'; }

timeit() { # timeit <label> <outfile> -- cmd...
  local label="$1" out="$2"; shift 2; shift
  local best="" status="ok"
  for _ in $(seq 1 "$RUNS"); do
    rm -rf "$WORK"/* 2>/dev/null
    local t0 t1; t0=$(date +%s.%N)
    "$@" >/dev/null 2>&1 || status="FAILED"
    t1=$(date +%s.%N)
    { [ -f "$out" ] && valid "$out"; } || status="INVALID"
    local dt; dt=$(echo "$t1 - $t0" | bc)
    if [ -z "$best" ] || (( $(echo "$dt < $best" | bc -l) )); then best="$dt"; fi
  done
  printf '  %-14s %6.2fs  %s\n' "$label" "$best" "$status"
}

hls_ts()    { echo "HLS TS (VOD)";      timeit m314dl out.mp4 -- "$M314" -t $THREADS -o out.mp4 "$BASE/hls-ts/media.m3u8";     timeit vsd v.mp4 -- "$VSD" save -t $THREADS -o v.mp4 "$BASE/hls-ts/media.m3u8";     timeit N_m3u8DL-RE re.mp4 -- "$RE" "$BASE/hls-ts/media.m3u8" --save-dir "$WORK" --tmp-dir "$WORK/t" --save-name re --thread-count $THREADS -M format=mp4 --no-log; }
hls_fmp4()  { echo "HLS fMP4 (VOD)";    timeit m314dl out.mp4 -- "$M314" -t $THREADS -o out.mp4 "$BASE/hls-fmp4/media.m3u8";   timeit vsd v.mp4 -- "$VSD" save -t $THREADS -o v.mp4 "$BASE/hls-fmp4/media.m3u8";   timeit N_m3u8DL-RE re.mp4 -- "$RE" "$BASE/hls-fmp4/media.m3u8" --save-dir "$WORK" --tmp-dir "$WORK/t" --save-name re --thread-count $THREADS -M format=mp4 --no-log; }
dash_clear(){ echo "DASH clear (VOD)";  timeit m314dl out.mp4 -- "$M314" -t $THREADS -o out.mp4 "$BASE/dash-clear/manifest.mpd"; timeit vsd v.mp4 -- "$VSD" save -t $THREADS -o v.mp4 "$BASE/dash-clear/manifest.mpd"; timeit N_m3u8DL-RE re.mp4 -- "$RE" "$BASE/dash-clear/manifest.mpd" --save-dir "$WORK" --tmp-dir "$WORK/t" --save-name re --thread-count $THREADS -M format=mp4 --auto-select --no-log; }
dash_cenc() { echo "DASH CENC (decrypt)"; timeit m314dl out.mp4 -- "$M314" -t $THREADS -key "$KID:$KEY" -o out.mp4 "$BASE/dash-cenc/manifest.mpd"; timeit vsd v.mp4 -- "$VSD" save -t $THREADS --keys "$KID:$KEY" -o v.mp4 "$BASE/dash-cenc/manifest.mpd"; timeit N_m3u8DL-RE re.mp4 -- "$RE" "$BASE/dash-cenc/manifest.mpd" --save-dir "$WORK" --tmp-dir "$WORK/t" --save-name re --thread-count $THREADS --key "$KID:$KEY" -M format=mp4 --auto-select --no-log; }
dash_cbcs() { echo "DASH cbcs (decrypt)"; timeit m314dl out.mp4 -- "$M314" -t $THREADS -key "$KID:$KEY" -o out.mp4 "$BASE/dash-cbcs/manifest.mpd"; timeit vsd v.mp4 -- "$VSD" save -t $THREADS --keys "$KID:$KEY" -o v.mp4 "$BASE/dash-cbcs/manifest.mpd"; timeit N_m3u8DL-RE re.mp4 -- "$RE" "$BASE/dash-cbcs/manifest.mpd" --save-dir "$WORK" --tmp-dir "$WORK/t" --save-name re --thread-count $THREADS --key "$KID:$KEY" -M format=mp4 --auto-select --no-log; }

case "${1:-all}" in
  hls-ts) hls_ts;; hls-fmp4) hls_fmp4;; dash-clear) dash_clear;;
  dash-cenc) dash_cenc;; dash-cbcs) dash_cbcs;;
  all) hls_ts; hls_fmp4; dash_clear; dash_cenc; dash_cbcs;;
  *) echo "usage: bench.sh [hls-ts|hls-fmp4|dash-clear|dash-cenc|dash-cbcs|all]"; exit 1;;
esac
