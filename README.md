# continuum-relay

Server component for the [Continuum iOS app](https://kalleeh.github.io/continuum/) — provides persistent Claude Code, Kiro, and terminal sessions accessible over WireGuard VPN.

## What it does

- **Embedded WireGuard** — manages the VPN tunnel directly; no `wireguard-tools` or `wg-quick` needed
- **Terminal server** — replaces `ttyd`; speaks the xterm.js WebSocket protocol that the Continuum iOS app connects to
- **Claude Code relay** — multiplexes `claude --output-format stream-json` sessions to multiple clients with reconnect support
- **APNs push** — optionally sends push notifications when Claude Code sessions finish

## Requirements

- Linux (amd64 or arm64) or macOS (Apple Silicon or Intel)
- `tmux` — for terminal session persistence
- Root / `CAP_NET_ADMIN` — for WireGuard TUN device creation

## Installation

The easiest way is via the [Continuum deploy script](https://github.com/kalleeh/continuum-ios/tree/main/server):

```bash
./deploy.sh create local --mobile-app   # this machine (macOS or Linux)
./deploy.sh create any  --mobile-app   # any server you can SSH into
./deploy.sh create lightsail --mobile-app  # AWS Lightsail
```

### Manual install

```bash
# Linux (amd64)
curl -fsSL -o /usr/local/bin/continuum-relay \
  https://github.com/kalleeh/continuum-relay/releases/latest/download/continuum-relay-linux-amd64
chmod +x /usr/local/bin/continuum-relay

# macOS (Apple Silicon)
curl -fsSL -o /usr/local/bin/continuum-relay \
  https://github.com/kalleeh/continuum-relay/releases/latest/download/continuum-relay-darwin-arm64
chmod +x /usr/local/bin/continuum-relay
```

Then create `/etc/continuum/env`:

```bash
CONTINUUM_TOKEN=<64-hex-char token>
CONTINUUM_WG_CONFIG=/etc/wireguard/wg0.conf
```

And run:

```bash
sudo continuum-relay  # needs root/CAP_NET_ADMIN for WireGuard TUN
```

## Configuration

All configuration is via environment variables (typically in `/etc/continuum/env`):

| Variable | Default | Description |
|----------|---------|-------------|
| `CONTINUUM_TOKEN` | required | 64-char hex auth token |
| `CONTINUUM_RELAY_ADDR` | `10.100.0.1:7682` | Claude Code relay listen address |
| `CONTINUUM_RELAY_LOG` | `/var/log/continuum/relay.log` | Log file path |
| `CONTINUUM_WG_CONFIG` | `/etc/wireguard/wg0.conf` | WireGuard config file |
| `CONTINUUM_WG_DISABLED` | — | Set to `1` to skip WireGuard |
| `CONTINUUM_TERMINAL_ADDR` | `10.100.0.1:7681` | Terminal server listen address |
| `CONTINUUM_TERMINAL_CMD` | `tmux new-session -A -s main` | Command run in each PTY |
| `APNS_KEY_PATH` | — | Path to APNs .p8 key (optional) |
| `APNS_KEY_ID` | — | APNs key ID (optional) |
| `APNS_TEAM_ID` | — | Apple team ID (optional) |
| `APNS_BUNDLE_ID` | — | App bundle ID (optional) |

## Building from source

```bash
go build -o continuum-relay .

# All platforms
make
```

Requires Go 1.22+.

## License

MIT
