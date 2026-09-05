#!/usr/bin/env sh
# Cross-compile bch for every platform you own. Requires Go 1.24+.
#   ./build.sh            → dist/
#   OUT=../builds ./build.sh
set -e
cd "$(dirname "$0")"
OUT="${OUT:-dist}"
mkdir -p "$OUT"
LD="-s -w"
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$LD" -o "$OUT/bch-darwin-arm64" .
GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$LD" -o "$OUT/bch-darwin-amd64" .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$LD" -o "$OUT/bch-windows-amd64.exe" .
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$LD" -o "$OUT/bch-windows-arm64.exe" .
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$LD" -o "$OUT/bch-linux-amd64" .
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$LD" -o "$OUT/bch-linux-arm64" .
ls -lh "$OUT"
