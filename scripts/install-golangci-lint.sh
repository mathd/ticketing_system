#!/usr/bin/env bash
# Install the pinned golangci-lint release binary (replaces `go install`,
# which compiled the linter from source: ~1min+). The expected sha256 per
# platform is pinned IN THIS FILE — independent of the download source —
# so a replaced release asset cannot pass. Update the digests together with
# GOLANGCI_VERSION in the Makefile (source: the release checksums.txt).
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

# Pinned digests for v2.12.2 (golangci-lint-2.12.2-checksums.txt)
case "$V-$OS-$ARCH" in
  2.12.2-linux-amd64)  SHA256=8df580d2670fed8fa984aac0507099af8df275e665215f5c7a2ae3943893a553 ;;
  2.12.2-linux-arm64)  SHA256=44cd40a8c76c86755375adfeea52cfd3533cb43d7bd647771e0ae065e166df3a ;;
  2.12.2-darwin-amd64) SHA256=f6f06d94b6241521c53d15450c5209b028270bf966f842afb11c030c79f5bc16 ;;
  2.12.2-darwin-arm64) SHA256=a9c54498731b3128f79e090be6110f3e5fffccc617b08142ed244d4126c73f29 ;;
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
install -m 0755 "$TMP/$NAME/golangci-lint" "$DEST/golangci-lint"
echo "golangci-lint $VERSION installed to $DEST (pinned sha256 verified)"
