# continuum-relay

Server component for the [Continuum iOS app](https://kalleeh.github.io/continuum/).
Provides persistent Claude Code, Kiro, and terminal sessions accessible over a
WireGuard VPN.

This is the canonical source. The iOS app's repo references this one as a git
submodule at `server/continuum-relay/`.

## What it does

- **Embedded WireGuard** — manages the VPN tunnel directly via wireguard-go;
  no `wireguard-tools` or `wg-quick` needed. Falls back to userspace netstack
  on macOS where TUN devices need extra entitlements.
- **Terminal server** — replaces `ttyd`; speaks the xterm.js binary WebSocket
  protocol that the iOS app's `TerminalAdapter` connects to.
- **Claude Code relay** — multiplexes `claude --output-format stream-json`
  sessions so the iOS app can reconnect mid-session and replay output.
- **Bedrock chat proxy** — forwards Ollama-shaped requests to AWS Bedrock,
  used by the in-app chat feature.
- **Peers CLI** — `continuum-relay peers add|list|remove` rewrites
  `/etc/wireguard/wg0.conf` and reloads the live tunnel without a restart.
- **APNs push** — optional push notifications when a Claude Code session
  finishes its turn.

## Requirements

- Linux (amd64 or arm64) or macOS (Apple Silicon or Intel)
- `tmux` — for terminal session persistence
- Root or `CAP_NET_ADMIN` — needed once at startup to create the WireGuard
  TUN device. The systemd unit shipped here drops to `User=ubuntu` and
  `AmbientCapabilities=CAP_NET_ADMIN`, so the relay process itself does
  not run as root.

## Installation

The supported install path is the bundled `deploy.sh`:

```bash
# Local machine (macOS or Linux)
./deploy.sh create local

# Any server you can SSH into
./deploy.sh create any

# AWS Lightsail (provisions the VM too)
./deploy.sh create lightsail
```

`deploy.sh` and `cloud-init.yaml` ship as release assets alongside the
binaries — grab them from
<https://github.com/kalleeh/continuum-relay/releases/latest>.

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

Verify the binary against the published checksum before installing:

```bash
curl -fsSL https://github.com/kalleeh/continuum-relay/releases/latest/download/checksums.txt \
  | grep continuum-relay-linux-amd64 \
  | sha256sum -c -
```

Then create `/etc/continuum/env` (mode `0600`, owned by the user the service
runs as):

```bash
CONTINUUM_TOKEN=<64-hex-char token, e.g. `openssl rand -hex 32`>
CONTINUUM_WG_CONFIG=/etc/wireguard/wg0.conf
```

And run:

```bash
sudo continuum-relay   # needs root or CAP_NET_ADMIN to create the TUN
```

## Configuration

All configuration is via environment variables, typically loaded from
`/etc/continuum/env`:

| Variable | Default | Description |
|----------|---------|-------------|
| `CONTINUUM_TOKEN` | required | 64-char hex auth token; used as Basic-auth password (terminal) and Bearer token (relay). Treat as a server-wide secret. |
| `CONTINUUM_RELAY_ADDR` | `10.100.0.1:7682` | Claude Code relay listen address. Bound to the WireGuard interface, never a public IP. |
| `CONTINUUM_RELAY_LOG` | `/var/log/continuum/relay.log` | Log file path. |
| `CONTINUUM_WG_CONFIG` | `/etc/wireguard/wg0.conf` | WireGuard config file. |
| `CONTINUUM_WG_DISABLED` | — | Set to `1` to skip starting the tunnel (for dev environments running an external one). |
| `CONTINUUM_TERMINAL_ADDR` | `10.100.0.1:7681` | Terminal WebSocket listen address. |
| `CONTINUUM_TERMINAL_CMD` | `tmux new-session -A -s main` | Command run in each PTY session. |
| `OLLAMA_HOST` | — | Where to forward `/api/chat` requests for the chat tab. |
| `OLLAMA_API_KEY` | — | Optional API key for ollama.com web search/fetch tools. |
| `BEDROCK_API_KEY` | — | If set, the chat proxy authenticates to AWS Bedrock with this key; otherwise IAM role is used. |
| `APNS_KEY_PATH` | — | Path to APNs `.p8` key (optional). |
| `APNS_KEY_ID` | — | APNs key ID (optional). |
| `APNS_TEAM_ID` | — | Apple team ID (optional). |
| `APNS_BUNDLE_ID` | — | App bundle ID (optional). |

## PTY environment contract

The terminal server starts each PTY with two extra env vars set, so user
shell rc files can detect a relay-launched session and behave differently
from an interactive SSH login:

- `TERM_PROGRAM=Continuum` — the standard convention used by VS Code,
  iTerm2, Warp, etc. Other tools that special-case `TERM_PROGRAM` will
  treat the relay session as a third-party terminal.
- `CONTINUUM_RELAY=1` — an unambiguous secondary marker that survives
  even if the user spawns another terminal program inside the session
  (which would clobber `TERM_PROGRAM`).

The intended use is opting *out* of automatic tmux attach, since the iOS
adapter manages its own per-session tmux windows:

```sh
# in ~/.zshrc / ~/.bashrc
if [[ -z "$TMUX" && -z "$CONTINUUM_RELAY" ]]; then
  tmux new -A -s main
fi
```

Without the `CONTINUUM_RELAY` guard, the rc-level tmux would intercept
the iOS adapter's `tmux new-session` keystrokes and mangle them into
the user's existing session.

## Security model

- The token in `/etc/continuum/env` is a server-wide secret. Anyone who
  has it can open terminal sessions and run AI tools as the configured
  user. Distribute it only over WireGuard, never log or echo it.
- All WebSocket and HTTP listeners bind to the WireGuard interface
  (`10.100.0.1`). Only UDP/51820 (WireGuard) is reachable from the
  public internet. A direct connection to `7681`/`7682` is impossible
  without a valid WireGuard peer key.
- The `peers` CLI rewrites `/etc/wireguard/wg0.conf` atomically. The
  systemd unit grants the service write access to `/etc/wireguard` so
  peer add/remove works at runtime.
- `internal/tools/files.go` blocks AI tool reads/writes against
  sensitive paths including `/etc/continuum/env`, `/etc/shadow`,
  WireGuard keys, and `~/.ssh/`.

## Building from source

```bash
go build -o continuum-relay .

# All platforms (release flags: -trimpath -ldflags='-s -w')
make release
```

Requires Go 1.22+. Built binaries match what CI publishes — same flags,
same `CGO_ENABLED=0`.

## Releases

Releases are cut by `.github/workflows/release.yml`, triggered on `v*`
tag pushes. The workflow:

1. Cross-builds all four binaries via `make release`.
2. Refreshes `checksums.txt` to cover binaries + `deploy.sh` + `cloud-init.yaml`.
3. Publishes a GitHub Release with all assets attached.

The workflow runs in this same repo, so it uses the per-job
`GITHUB_TOKEN` and needs no cross-repo PAT.

```bash
git tag v0.4.0
git push origin v0.4.0
```

## License

MIT
