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
URL="https://github.com/$REPO/releases/download/$TAG/zae_${V}_${OS}_${ARCH}.tar.gz"
DEST=${ZAE_INSTALL_DIR:-/usr/local/bin}
echo "installing zae $TAG for $OS/$ARCH into $DEST"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "$URL" -o "$TMP/zae.tgz"
curl -fsSL "https://github.com/$REPO/releases/download/$TAG/checksums.txt" -o "$TMP/checksums.txt"
( cd "$TMP" && grep "zae_${V}_${OS}_${ARCH}.tar.gz" checksums.txt | sha256sum -c - >/dev/null 2>&1 \
  || grep "zae_${V}_${OS}_${ARCH}.tar.gz" checksums.txt | shasum -a 256 -c - >/dev/null )
tar -xzf "$TMP/zae.tgz" -C "$TMP" zae
install -m 0755 "$TMP/zae" "$DEST/zae" 2>/dev/null || sudo install -m 0755 "$TMP/zae" "$DEST/zae"
echo "done: $("$DEST"/zae version)"
