#!/usr/bin/env bash
# Install the pinned golangci-lint release binary with sha256 verification
# (replaces `go install`, which compiled the linter from source: ~1min+).
# Usage: install-golangci-lint.sh <version, e.g. v2.12.2> <dest dir>
set -euo pipefail

VERSION="${1:?usage: install-golangci-lint.sh <version> <dest dir>}"
DEST="${2:?usage: install-golangci-lint.sh <version> <dest dir>}"
V="${VERSION#v}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

NAME="golangci-lint-${V}-${OS}-${ARCH}"
BASE="https://github.com/golangci/golangci-lint/releases/download/${VERSION}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

curl -fsSL -o "$TMP/$NAME.tar.gz" "$BASE/$NAME.tar.gz"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/golangci-lint-${V}-checksums.txt"
(cd "$TMP" && grep "  $NAME.tar.gz\$" checksums.txt | sha256sum -c - >/dev/null)

tar -xzf "$TMP/$NAME.tar.gz" -C "$TMP"
mkdir -p "$DEST"
install -m 0755 "$TMP/$NAME/golangci-lint" "$DEST/golangci-lint"
echo "golangci-lint $VERSION installed to $DEST (checksum verified)"
