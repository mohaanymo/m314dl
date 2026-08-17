#!/usr/bin/env bash
# build-release.sh — cross-compile m314dl for all platforms and package each
# into a tar.gz (Unix) or zip (Windows), plus a SHA256SUMS file.
#
#   ./build-release.sh [VERSION]     (default: version const in main.go)
set -euo pipefail

VER=${1:-$(grep -oP 'version = "\K[^"]+' main.go)}
OUT=dist
rm -rf "$OUT"; mkdir -p "$OUT"

# os/arch/goarm — the platforms competitors ship, plus arm.
TARGETS=(
  "linux amd64 ."
  "linux arm64 ."
  "linux arm 7"
  "darwin amd64 ."
  "darwin arm64 ."
  "windows amd64 ."
  "windows arm64 ."
)

echo "== building m314dl v$VER =="
for t in "${TARGETS[@]}"; do
  read -r GOOS GOARCH GOARM <<<"$t"
  name="m314dl"; [ "$GOOS" = windows ] && name="m314dl.exe"
  stage="$OUT/m314dl_v${VER}_${GOOS}_${GOARCH}"
  mkdir -p "$stage"
  env CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" ${GOARM:+GOARM=$GOARM} \
    go build -trimpath -ldflags "-s -w" -o "$stage/$name" .
  cp README.md "$stage/"
  base="m314dl_v${VER}_${GOOS}_${GOARCH}"
  if [ "$GOOS" = windows ]; then
    (cd "$OUT" && zip -qr "${base}.zip" "$base") && rm -rf "$stage"
    echo "  $base.zip"
  else
    tar -C "$OUT" -czf "$OUT/${base}.tar.gz" "$base" && rm -rf "$stage"
    echo "  $base.tar.gz"
  fi
done

(cd "$OUT" && sha256sum m314dl_v${VER}_* > SHA256SUMS)
echo "== done: $(ls "$OUT" | grep -c -E 'tar.gz|zip') archives in $OUT/ =="
ls -lh "$OUT"
