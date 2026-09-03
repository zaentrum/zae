#!/bin/sh
# Install zae (the zaentrum CLI) from the latest GitHub release.
# Inspect me first — piping a script into sh should always earn that.
set -eu
REPO=zaentrum/zae
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
[ -n "$TAG" ] || { echo "could not resolve the latest release" >&2; exit 1; }
V=${TAG#v}
FILE="zae_${V}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$REPO/releases/download/$TAG/$FILE"
DEST=${ZAE_INSTALL_DIR:-/usr/local/bin}
echo "installing zae $TAG for $OS/$ARCH into $DEST"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
# Keep the release's own filename — the checksum manifest names it, and a
# checksum you cannot match against the file you downloaded verifies nothing.
curl -fsSL "$URL" -o "$TMP/$FILE"
curl -fsSL "https://github.com/$REPO/releases/download/$TAG/checksums.txt" -o "$TMP/checksums.txt"
(
  cd "$TMP"
  if command -v sha256sum >/dev/null 2>&1; then
    grep " $FILE\$" checksums.txt | sha256sum -c - >/dev/null
  else
    grep " $FILE\$" checksums.txt | shasum -a 256 -c - >/dev/null
  fi
) || { echo "checksum verification FAILED for $FILE" >&2; exit 1; }
tar -xzf "$TMP/$FILE" -C "$TMP" zae
install -m 0755 "$TMP/zae" "$DEST/zae" 2>/dev/null || sudo install -m 0755 "$TMP/zae" "$DEST/zae"
echo "done: $("$DEST"/zae version)"
