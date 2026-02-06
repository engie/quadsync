#!/usr/bin/env bash
set -euo pipefail

VERSION=${1:?Usage: release.sh <version>}

echo "Building quadlet-deploy-linux-arm64..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags='-s -w' -o quadlet-deploy-linux-arm64 .

echo "SHA256:"
sha256sum quadlet-deploy-linux-arm64

echo "Creating release v${VERSION}..."
gh release create "v${VERSION}" quadlet-deploy-linux-arm64 --title "v${VERSION}"
