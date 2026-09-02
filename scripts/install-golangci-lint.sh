#!/usr/bin/env bash
# Install the pinned golangci-lint release binary (replaces `go install`,
# which compiled the linter from source: ~1min+). The expected sha256 per
# platform is pinned IN THIS FILE — independent of the download source —
# so a replaced release asset cannot pass. Update the digests together with
# GOLANGCI_VERSION in the Makefile (source: the release checksums.txt).
# Usage: install-golangci-lint.sh <version, e.g. v2.13.2> <dest dir> [installed name]
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

# Pinned digests for v2.13.2 (golangci-lint-2.13.2-checksums.txt)
case "$V-$OS-$ARCH" in
  2.13.2-linux-amd64)  SHA256=2277d43b98ec0054280f2ac26b53268bae97682444678a59a657dd565da021d6 ;;
  2.13.2-linux-arm64)  SHA256=a2a4e0065aa41be71f7c5ac90f271b61751331e5d04314e62afe4027855f0893 ;;
  2.13.2-darwin-amd64) SHA256=8a13aaf9cbbb1dee52824e862cf0d0720e5bb97c1f4260d1e51623a09492b57b ;;
  2.13.2-darwin-arm64) SHA256=f4bf83f0b64f055c42b28fc9a38861839f69c096e61c788e72dfaae412011789 ;;
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
