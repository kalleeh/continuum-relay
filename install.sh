#!/bin/sh
# Continuum relay — installer / upgrader.
#
# Auto-detects mode:
#   - If /etc/continuum/env exists → UPGRADE (binary swap only, no prompts,
#     no config touched). Safe to run repeatedly on a deployed server.
#   - Otherwise → FRESH INSTALL (auth token, WireGuard server + first peer,
#     service unit, firewall rules; prints the Continuum QR payload).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/kalleeh/continuum-relay/main/install.sh | sudo sh
#
# Env vars (all optional):
#   CONTINUUM_VERSION       Pin a release, e.g. v0.4.0. Default: latest.
#   CONTINUUM_NO_RESTART=1  Upgrade only — skip the service restart.
#   CONTINUUM_USER          Fresh install — system user the relay runs as.
#                           Default: ubuntu | $SUDO_USER | console user.
#   CONTINUUM_PUBLIC_IP     Fresh install — override IP detection.
#
# Supported:
#   - Linux: Debian/Ubuntu (apt), RHEL/Fedora (dnf/yum), Alpine (apk), Arch (pacman)
#   - macOS: Apple Silicon and Intel (LaunchDaemon)
#
# This script does NOT provision a cloud VM. For end-to-end VM creation
# (Lightsail/Hetzner/DO/EC2), use deploy.sh from your laptop.

set -eu

REPO="kalleeh/continuum-relay"
ENV_FILE="/etc/continuum/env"
WG_CONF="/etc/wireguard/wg0.conf"
LOG_DIR="/var/log/continuum"
VERSION="${CONTINUUM_VERSION:-latest}"

# ── Platform detection ─────────────────────────────────────────────────────

if [ "$(id -u)" -ne 0 ]; then
  echo "install.sh must run as root (try: curl ... | sudo sh)" >&2
  exit 1
fi

case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "Unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
ASSET="continuum-relay-${OS}-${ARCH}"

# Install destination differs on macOS Apple Silicon (Homebrew prefix) vs Intel/Linux.
if [ "$OS" = "darwin" ] && [ -d /opt/homebrew/bin ]; then
  DEST="/opt/homebrew/bin/continuum-relay"
elif [ "$OS" = "darwin" ]; then
  DEST="/usr/local/bin/continuum-relay"
else
  DEST="/usr/local/bin/continuum-relay"
fi

if [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/${REPO}/releases/latest/download"
else
  BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi

# ── Common helpers ─────────────────────────────────────────────────────────

download_binary() {
  tmp="$(mktemp)"
  echo "==> Downloading ${ASSET} (${VERSION})"
  curl -fsSL -o "$tmp" "${BASE}/${ASSET}"

  echo "==> Verifying checksum"
  expected="$(curl -fsSL "${BASE}/checksums.txt" | awk -v f="$ASSET" '$2==f || $2=="./"f {print $1; exit}')"
  if [ -z "$expected" ]; then
    echo "ERROR: ${ASSET} not listed in checksums.txt" >&2
    rm -f "$tmp"; exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp" | awk '{print $1}')"
  else
    actual="$(shasum -a 256 "$tmp" | awk '{print $1}')"
  fi
  if [ "$expected" != "$actual" ]; then
    echo "ERROR: checksum mismatch (expected $expected, got $actual)" >&2
    rm -f "$tmp"; exit 1
  fi

  mkdir -p "$(dirname "$DEST")"
  echo "==> Installing to ${DEST}"
  install -m 0755 "$tmp" "$DEST"
  rm -f "$tmp"

  # macOS Gatekeeper: clear quarantine attribute on curl-downloaded unsigned binaries.
  if [ "$OS" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
    xattr -d com.apple.quarantine "$DEST" 2>/dev/null || true
  fi
}

restart_service() {
  if [ "${CONTINUUM_NO_RESTART:-0}" = "1" ]; then
    echo "==> Skipping restart (CONTINUUM_NO_RESTART=1)"
    return
  fi
  if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files continuum-relay.service >/dev/null 2>&1; then
    echo "==> Restarting continuum-relay.service"
    systemctl restart continuum-relay
    systemctl is-active continuum-relay
  elif command -v launchctl >/dev/null 2>&1 && [ -f /Library/LaunchDaemons/com.kalleh.continuum-relay.plist ]; then
    echo "==> Restarting LaunchDaemon"
    launchctl kickstart -k system/com.kalleh.continuum-relay
  elif command -v rc-service >/dev/null 2>&1 && rc-service -e continuum-relay 2>/dev/null; then
    echo "==> Restarting OpenRC service"
    rc-service continuum-relay restart
  else
    echo "==> No service manager unit found — restart manually"
  fi
}

# ── Upgrade mode ───────────────────────────────────────────────────────────

if [ -f "$ENV_FILE" ]; then
  echo "==> Existing install detected ($ENV_FILE) — upgrade only"
  download_binary
  restart_service
  echo "==> Upgrade complete"
  exit 0
fi

# ── Fresh install: dependencies ────────────────────────────────────────────

echo "==> No existing install — provisioning fresh"

install_linux_deps() {
  if command -v apt-get >/dev/null 2>&1; then
    echo "==> Installing dependencies (apt)"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      tmux qrencode wireguard-tools openssl curl ca-certificates ufw
  elif command -v dnf >/dev/null 2>&1; then
    echo "==> Installing dependencies (dnf)"
    dnf install -y -q tmux qrencode wireguard-tools openssl curl ca-certificates
  elif command -v yum >/dev/null 2>&1; then
    echo "==> Installing dependencies (yum)"
    yum install -y -q tmux qrencode wireguard-tools openssl curl ca-certificates
  elif command -v pacman >/dev/null 2>&1; then
    echo "==> Installing dependencies (pacman)"
    pacman -Sy --noconfirm --needed tmux qrencode wireguard-tools openssl curl ca-certificates
  elif command -v apk >/dev/null 2>&1; then
    echo "==> Installing dependencies (apk)"
    apk add --quiet tmux libqrencode-tools wireguard-tools openssl curl ca-certificates
  else
    echo "WARNING: unrecognized package manager — install tmux, qrencode," >&2
    echo "  wireguard-tools, openssl, curl manually." >&2
  fi
}

install_macos_deps() {
  if ! command -v brew >/dev/null 2>&1; then
    echo "ERROR: Homebrew required on macOS. Install: https://brew.sh" >&2
    exit 1
  fi
  # Run brew as the invoking user, not root (brew refuses to run as root).
  brew_user="${SUDO_USER:-$(stat -f '%Su' /dev/console 2>/dev/null || echo root)}"
  if [ "$brew_user" = "root" ]; then
    echo "ERROR: cannot run Homebrew as root — set SUDO_USER" >&2
    exit 1
  fi
  echo "==> Installing dependencies via brew (as $brew_user)"
  sudo -u "$brew_user" brew install tmux qrencode wireguard-tools >/dev/null
}

# ── Common provisioning helpers ────────────────────────────────────────────

detect_relay_user() {
  if [ -n "${CONTINUUM_USER:-}" ]; then
    RELAY_USER="$CONTINUUM_USER"
    return
  fi
  if [ "$OS" = "darwin" ]; then
    # On macOS the daemon runs as root (PTY needs it under launchd), but the
    # PTY child drops to the console user via syscall.Credential.
    RELAY_USER="$(stat -f '%Su' /dev/console 2>/dev/null || echo "${SUDO_USER:-}")"
    if [ -z "$RELAY_USER" ] || [ "$RELAY_USER" = "root" ]; then
      echo "ERROR: could not determine console user — set CONTINUUM_USER" >&2
      exit 1
    fi
    return
  fi
  if id ubuntu >/dev/null 2>&1; then
    RELAY_USER=ubuntu
  elif [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
    RELAY_USER="$SUDO_USER"
  else
    echo "ERROR: could not determine relay user — set CONTINUUM_USER" >&2
    exit 1
  fi
}

detect_public_ip() {
  PUBLIC_IP="${CONTINUUM_PUBLIC_IP:-}"
  [ -n "$PUBLIC_IP" ] && return
  PUBLIC_IP="$(curl -fsSL --max-time 5 https://api.ipify.org 2>/dev/null || true)"
  if [ -z "$PUBLIC_IP" ]; then
    PUBLIC_IP="$(curl -fsSL --max-time 5 https://ifconfig.me 2>/dev/null || true)"
  fi
  if [ -z "$PUBLIC_IP" ]; then
    echo "ERROR: public IP detection failed — set CONTINUUM_PUBLIC_IP" >&2
    exit 1
  fi
}

generate_secrets() {
  echo "==> Generating auth token + WireGuard keys"
  TOKEN="$(openssl rand -hex 32)"
  SERVER_PRIV="$(wg genkey)"
  SERVER_PUB="$(printf '%s' "$SERVER_PRIV" | wg pubkey)"
  PEER_PRIV="$(wg genkey)"
  PEER_PUB="$(printf '%s' "$PEER_PRIV" | wg pubkey)"
  PEER_IP="10.100.0.2"
}

write_env_and_wg() {
  mkdir -p /etc/continuum "$LOG_DIR" /etc/wireguard
  umask 077
  cat > "$ENV_FILE" <<EOF
CONTINUUM_TOKEN=$TOKEN
CONTINUUM_RELAY_ADDR=10.100.0.1:7682
CONTINUUM_RELAY_LOG=$LOG_DIR/relay.log
EOF
  cat > "$WG_CONF" <<EOF
[Interface]
PrivateKey = $SERVER_PRIV
Address = 10.100.0.1/24
ListenPort = 51820

[Peer]
# device-1
PublicKey = $PEER_PUB
AllowedIPs = $PEER_IP/32
EOF
  umask 022
  chmod 600 "$ENV_FILE" "$WG_CONF"

  if [ "$OS" = "linux" ]; then
    chown "$RELAY_USER:$RELAY_USER" "$ENV_FILE" "$WG_CONF" 2>/dev/null || true
    chown -R "$RELAY_USER:$RELAY_USER" "$LOG_DIR" 2>/dev/null || true
  fi
}

# ── Linux: systemd / OpenRC service ────────────────────────────────────────

install_linux_service() {
  # Disable wg-quick — the relay creates its own TUN; both would fight.
  systemctl disable --now "wg-quick@wg0" 2>/dev/null || true

  if command -v systemctl >/dev/null 2>&1; then
    cat > /etc/systemd/system/continuum-relay.service <<EOF
[Unit]
Description=Continuum Relay
After=network.target

[Service]
Type=simple
User=$RELAY_USER
EnvironmentFile=$ENV_FILE
ExecStart=$DEST
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=$LOG_DIR /etc/wireguard
ReadOnlyPaths=/etc/continuum
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN

[Install]
WantedBy=multi-user.target
EOF

    cat > /etc/logrotate.d/continuum <<'EOF'
/var/log/continuum/*.log {
    daily
    rotate 7
    compress
    missingok
    notifempty
    postrotate
        systemctl kill -s HUP continuum-relay 2>/dev/null || true
    endscript
}
EOF
    systemctl daemon-reload
    systemctl enable --now continuum-relay
    return
  fi

  # OpenRC (Alpine, etc.) — set CAP_NET_ADMIN via setcap on the binary instead
  # of a systemd capability directive. Service runs as root, drops via su-exec.
  if command -v rc-service >/dev/null 2>&1; then
    if command -v setcap >/dev/null 2>&1; then
      setcap cap_net_admin=eip "$DEST" || true
    fi
    cat > /etc/init.d/continuum-relay <<EOF
#!/sbin/openrc-run
name="continuum-relay"
command="$DEST"
command_user="$RELAY_USER"
command_background=true
pidfile="/run/\${RC_SVCNAME}.pid"
output_log="$LOG_DIR/relay.log"
error_log="$LOG_DIR/relay.log"

depend() {
    need net
}

start_pre() {
    set -a; . "$ENV_FILE"; set +a
}
EOF
    chmod +x /etc/init.d/continuum-relay
    rc-update add continuum-relay default
    rc-service continuum-relay start
    return
  fi

  echo "WARNING: no supported init system found — start $DEST manually" >&2
}

configure_linux_firewall() {
  if command -v ufw >/dev/null 2>&1; then
    echo "==> Configuring UFW (SSH + WireGuard public, relay VPN-only)"
    ufw --force enable >/dev/null 2>&1 || true
    ufw allow 22/tcp >/dev/null 2>&1 || true
    ufw allow 51820/udp comment 'Continuum WireGuard' >/dev/null 2>&1 || true
    ufw allow from 10.100.0.0/24 to any port 7682 proto tcp comment 'Continuum relay (VPN only)' >/dev/null 2>&1 || true
  elif command -v firewall-cmd >/dev/null 2>&1; then
    echo "==> Configuring firewalld"
    firewall-cmd --permanent --add-port=51820/udp >/dev/null 2>&1 || true
    firewall-cmd --permanent --add-port=22/tcp >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
  else
    echo "==> No supported firewall found — open UDP/51820 manually if needed"
  fi
}

# ── macOS: LaunchDaemon ────────────────────────────────────────────────────

install_macos_service() {
  PLIST=/Library/LaunchDaemons/com.kalleh.continuum-relay.plist
  USER_HOME="$(eval echo "~$RELAY_USER")"
  USER_LOG="$USER_HOME/.continuum"
  mkdir -p "$USER_LOG"
  chown "$RELAY_USER" "$USER_LOG" 2>/dev/null || true

  PATH_ENTRY="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.kalleh.continuum-relay</string>
    <key>ProgramArguments</key>
    <array><string>$DEST</string></array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>$USER_LOG/stdout.log</string>
    <key>StandardErrorPath</key><string>$USER_LOG/stderr.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>$PATH_ENTRY</string>
        <key>HOME</key><string>$USER_HOME</string>
        <key>CONTINUUM_USER</key><string>$RELAY_USER</string>
    </dict>
</dict>
</plist>
EOF
  chown root:wheel "$PLIST"
  chmod 644 "$PLIST"

  # bootstrap if not loaded; otherwise restart
  if launchctl print system/com.kalleh.continuum-relay >/dev/null 2>&1; then
    launchctl kickstart -k system/com.kalleh.continuum-relay
  else
    launchctl bootstrap system "$PLIST"
  fi

  echo ""
  echo "NOTE: macOS Gatekeeper may flag the unsigned binary on first run."
  echo "  If launchd reports 'killed: 9', open System Settings → Privacy"
  echo "  & Security and click 'Allow Anyway' for continuum-relay."
}

# ── Run fresh install ──────────────────────────────────────────────────────

if [ "$OS" = "linux" ]; then
  install_linux_deps
else
  install_macos_deps
fi

detect_relay_user
echo "==> Relay user: $RELAY_USER"
detect_public_ip
echo "==> Public IP: $PUBLIC_IP"
generate_secrets
write_env_and_wg
download_binary

if [ "$OS" = "linux" ]; then
  install_linux_service
  configure_linux_firewall
else
  install_macos_service
fi

# ── Output: QR + next steps ────────────────────────────────────────────────

HOSTNAME_SHORT="$(hostname -s 2>/dev/null || hostname)"
QR_PAYLOAD="{\"v\":1,\"serverName\":\"$HOSTNAME_SHORT\",\"serverPublicIP\":\"$PUBLIC_IP\",\"wgServerEndpoint\":\"$PUBLIC_IP:51820\",\"wgServerPublicKey\":\"$SERVER_PUB\",\"wgClientPrivateKey\":\"$PEER_PRIV\",\"wgClientAddress\":\"$PEER_IP/24\",\"wgDNS\":\"1.1.1.1\",\"wgKeepalive\":25,\"authToken\":\"$TOKEN\",\"ttydPort\":7681,\"relayPort\":7682}"

cat <<EOF

==================================================================
  Continuum relay installed.
==================================================================

  Public IP:  $PUBLIC_IP
  Run as:     $RELAY_USER
  Config:     $ENV_FILE  (mode 0600)
  WG config:  $WG_CONF
  Binary:     $DEST

EOF

if command -v qrencode >/dev/null 2>&1; then
  echo "Scan this QR with the Continuum iOS app:"
  echo ""
  printf '%s' "$QR_PAYLOAD" | qrencode -t ansiutf8
  echo ""
fi

cat <<EOF
Manual JSON (paste into the iOS app if QR fails):

$QR_PAYLOAD

WARNING: contains the WireGuard private key + auth token.
Do not photograph or paste into a cloud-synced clipboard.

Next steps:
  - Add more devices:    sudo continuum-relay peers add <name>
  - List enrolled:       sudo continuum-relay peers list
  - Upgrade in place:    curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | sudo sh

EOF
