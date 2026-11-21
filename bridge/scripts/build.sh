#!/bin/bash
# Build the darwin/arm64 binary (pure Go, no cgo) into dist/ and give it a
# stable ad-hoc signing identifier. Run on any Mac with Go installed.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:-0.1.0}"
mkdir -p dist
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o dist/mailbridge ./cmd/mailbridge
if command -v codesign >/dev/null; then
  codesign -f -s - -i com.geekychris.mailbridge dist/mailbridge
fi
echo "built dist/mailbridge ($VERSION)"
