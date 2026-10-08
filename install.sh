#!/usr/bin/env bash
#
# session-share installer — downloads a prebuilt session-share binary into your PATH.
# No Node, no Go, no compiler required.
#
#   curl -fsSL https://raw.githubusercontent.com/mydevmachine/session-share/main/install.sh | bash
#
# Env overrides:
#   SESSION_SHARE_INSTALL_DIR   target dir (default: /usr/local/bin, falls back to ~/.local/bin)
#   SESSION_SHARE_VERSION       release tag to install (default: latest)

set -euo pipefail

REPO="mydevmachine/session-share"
VERSION="${SESSION_SHARE_VERSION:-latest}"

# --- detect platform ---------------------------------------------------------
os="$(uname -s)"
arch="$(uname -m)"

case "$os" in
  Darwin) goos="darwin" ;;
  Linux)  goos="linux" ;;
  *) echo "Error: unsupported OS '$os'. session-share runs on Linux and macOS, where tmux runs." >&2; exit 1 ;;
esac

case "$arch" in
  x86_64|amd64) goarch="amd64" ;;
  arm64|aarch64) goarch="arm64" ;;
  *) echo "Error: unsupported architecture '$arch'." >&2; exit 1 ;;
esac

ASSET="session-share_${goos}_${goarch}.tar.gz"
if [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/${REPO}/releases/latest/download"
else
  BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi
URL="${BASE}/${ASSET}"
CHECKSUMS_URL="${BASE}/checksums.txt"

# --- choose a writable install dir -------------------------------------------
choose_dir() {
  if [ -n "${SESSION_SHARE_INSTALL_DIR:-}" ]; then echo "$SESSION_SHARE_INSTALL_DIR"; return; fi
  if [ -w "/usr/local/bin" ]; then echo "/usr/local/bin"; return; fi
  echo "$HOME/.local/bin"
}
INSTALL_DIR="$(choose_dir)"
mkdir -p "$INSTALL_DIR"

# --- download + extract ------------------------------------------------------
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fetch() { # fetch <url> <dest>
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    echo "Error: need curl or wget." >&2; exit 1
  fi
}

echo "Downloading ${ASSET} (${VERSION})..."
fetch "$URL" "$tmp/$ASSET"

# --- verify checksum ---------------------------------------------------------
# The release ships a checksums.txt; verify the archive before trusting it.
if fetch "$CHECKSUMS_URL" "$tmp/checksums.txt" 2>/dev/null; then
  expected="$(grep " ${ASSET}\$" "$tmp/checksums.txt" | awk '{print $1}')"
  if [ -z "$expected" ]; then
    echo "Error: ${ASSET} not found in checksums.txt." >&2; exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp/$ASSET" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$tmp/$ASSET" | awk '{print $1}')"
  else
    echo "Error: need sha256sum or shasum to verify the download." >&2; exit 1
  fi
  if [ "$expected" != "$actual" ]; then
    echo "Error: checksum mismatch for ${ASSET}." >&2
    echo "  expected $expected" >&2
    echo "  actual   $actual" >&2
    exit 1
  fi
  echo "✓ Checksum verified"
else
  echo "Error: could not download checksums.txt to verify the release." >&2; exit 1
fi

tar -xzf "$tmp/$ASSET" -C "$tmp"
install -m 0755 "$tmp/session-share" "$INSTALL_DIR/session-share"

echo "✓ Installed session-share to ${INSTALL_DIR}/session-share"

# --- PATH hint ---------------------------------------------------------------
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    echo ""
    echo "⚠  ${INSTALL_DIR} is not on your PATH. Add to your shell profile:"
    echo "    export PATH=\"${INSTALL_DIR}:\$PATH\""
    ;;
esac

echo ""
echo "Try it:  session-share help"
