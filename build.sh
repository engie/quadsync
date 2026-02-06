#!/usr/bin/env bash
set -euo pipefail

echo "Building quadsync-linux-amd64..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags='-s -w' -o quadsync-linux-amd64 .

echo "Building quadsync-linux-arm64..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags='-s -w' -o quadsync-linux-arm64 .

echo "SHA256:"
sha256sum quadsync-linux-amd64 quadsync-linux-arm64
