#!/usr/bin/env bash
# Install the pinned golangci-lint release binary (replaces `go install`,
# which compiled the linter from source: ~1min+). The expected sha256 per
# platform is pinned IN THIS FILE — independent of the download source —
# so a replaced release asset cannot pass. Update the digests together with
# GOLANGCI_VERSION in the Makefile (source: the release checksums.txt).
# Usage: install-golangci-lint.sh <version, e.g. v2.15.0> <dest dir> [installed name]
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

# Pinned digests for v2.15.0 (golangci-lint-2.15.0-checksums.txt)
case "$V-$OS-$ARCH" in
  2.15.0-linux-amd64)  SHA256=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea ;;
  2.15.0-linux-arm64)  SHA256=b4d8f17068b35ee6060cc2f69a16fb48740d706fbba81661c318649c114a6998 ;;
  2.15.0-darwin-amd64) SHA256=767b899a28285e88ded2823d5dcc4bc5ef681173eb2d80f029a7b17ee4ece3b8 ;;
  2.15.0-darwin-arm64) SHA256=d1e409481a8ca846c4bc97e781eb5cb1bd5c23353a0b3977a1cbda1b1b6447d5 ;;
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
