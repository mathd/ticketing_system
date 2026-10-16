#!/usr/bin/env bash
# Install the pinned golangci-lint release binary (replaces `go install`,
# which compiled the linter from source: ~1min+). The expected sha256 per
# platform is pinned IN THIS FILE — independent of the download source —
# so a replaced release asset cannot pass. Update the digests together with
# GOLANGCI_VERSION in the Makefile (source: the release checksums.txt).
# Usage: install-golangci-lint.sh <version, e.g. v2.14.0> <dest dir> [installed name]
# The caller names the binary after the version so that bumping the pin cannot
# silently reuse an already-installed older one — make sees a path that does
# not exist yet and actually installs.
set -euo pipefail

VERSION="${1:?usage: install-golangci-lint.sh <version> <dest dir>}"
DEST="${2:?usage: install-golangci-lint.sh <version> <dest dir> [installed name]}"
OUT="${3:-golangci-lint}"
V="${VERSION#v}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

# Pinned digests for v2.14.0 (golangci-lint-2.14.0-checksums.txt)
case "$V-$OS-$ARCH" in
  2.14.0-linux-amd64)  SHA256=ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab ;;
  2.14.0-linux-arm64)  SHA256=ee7ec5f3453d15ddf106fae5a4d6c71737712348a979d1fe9cd52ec7ea299bae ;;
  2.14.0-darwin-amd64) SHA256=a5667c1c3536be1740133213e1e822bfb8f0d98ea12903174d6d5f635e4ed68d ;;
  2.14.0-darwin-arm64) SHA256=5ef5f36a7147e91dc58ef9ef4d11bb7bad5ead0c76eb6c01327a73c641d1dcc3 ;;
  *) echo "no pinned digest for golangci-lint $V on $OS/$ARCH — add it here from the release checksums.txt" >&2; exit 1 ;;
esac

NAME="golangci-lint-${V}-${OS}-${ARCH}"
BASE="https://github.com/golangci/golangci-lint/releases/download/${VERSION}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

curl -fsSL -o "$TMP/$NAME.tar.gz" "$BASE/$NAME.tar.gz"
echo "$SHA256  $TMP/$NAME.tar.gz" | sha256sum -c - >/dev/null

tar -xzf "$TMP/$NAME.tar.gz" -C "$TMP"
mkdir -p "$DEST"
install -m 0755 "$TMP/$NAME/golangci-lint" "$DEST/$OUT"
echo "golangci-lint $VERSION installed to $DEST/$OUT (pinned sha256 verified)"
