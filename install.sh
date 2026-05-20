#!/bin/sh
# Continuum relay — in-place upgrade installer.
#
# Usage (on a server that already has continuum-relay deployed):
#   curl -fsSL https://raw.githubusercontent.com/kalleeh/continuum-relay/main/install.sh | sudo sh
#
# Optional env vars:
#   CONTINUUM_VERSION   — pin to a specific release (e.g. v0.3.1). Defaults to "latest".
#   CONTINUUM_NO_RESTART=1 — skip the systemctl restart at the end.
#
# This script ONLY upgrades the binary on an existing install. To provision a
# fresh server from zero (cloud VM, WireGuard keys, QR onboarding), use
# deploy.sh from your laptop instead.

set -eu

REPO="kalleeh/continuum-relay"
VERSION="${CONTINUUM_VERSION:-latest}"
DEST="/usr/local/bin/continuum-relay"

if [ "$(id -u)" -ne 0 ]; then
  echo "install.sh must run as root (try: curl ... | sudo sh)" >&2
  exit 1
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux|darwin) ;;
  *) echo "Unsupported OS: $os" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "Unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

asset="continuum-relay-${os}-${arch}"

if [ "$VERSION" = "latest" ]; then
  base="https://github.com/${REPO}/releases/latest/download"
else
  base="https://github.com/${REPO}/releases/download/${VERSION}"
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

echo "==> Downloading ${asset} (${VERSION})"
curl -fsSL -o "$tmp" "${base}/${asset}"

echo "==> Verifying checksum"
expected="$(curl -fsSL "${base}/checksums.txt" | awk -v f="$asset" '$2==f || $2=="./"f {print $1; exit}')"
if [ -z "$expected" ]; then
  echo "ERROR: ${asset} not listed in checksums.txt" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$tmp" | awk '{print $1}')"
fi

if [ "$expected" != "$actual" ]; then
  echo "ERROR: checksum mismatch (expected $expected, got $actual)" >&2
  exit 1
fi

echo "==> Installing to ${DEST}"
install -m 0755 "$tmp" "$DEST"

if [ "${CONTINUUM_NO_RESTART:-0}" = "1" ]; then
  echo "==> Skipping restart (CONTINUUM_NO_RESTART=1)"
elif command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files continuum-relay.service >/dev/null 2>&1; then
  echo "==> Restarting continuum-relay.service"
  systemctl restart continuum-relay
  systemctl is-active continuum-relay
elif command -v launchctl >/dev/null 2>&1 && [ -f /Library/LaunchDaemons/com.kalleh.continuum-relay.plist ]; then
  echo "==> Restarting LaunchDaemon"
  launchctl kickstart -k system/com.kalleh.continuum-relay
else
  echo "==> No service manager unit found — restart manually"
fi

echo "==> Installed: $("$DEST" --version 2>/dev/null || echo "$VERSION")"
