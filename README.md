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
- **Claude Code relay** — tracks tmux-backed sessions and pushes the session
  list (`session_list`), tool-permission prompts, and project events to the iOS
  app over WebSocket. Claude Code itself runs as a TUI inside tmux; its output
  flows over the terminal subsystem as raw VT100 bytes (the old
  `claude --output-format stream-json` multiplexing was removed in 2026-05).
- **Bedrock chat proxy** — forwards Ollama-shaped requests to AWS Bedrock,
  used by the in-app chat feature.
- **Peers CLI** — `continuum-relay peers add|list|remove` rewrites
  `/etc/wireguard/wg0.conf` and reloads the live tunnel without a restart.
- **Status CLI** — `continuum-relay status` and `continuum-relay sessions`
  query the *already-running* relay over HTTP (`/health`, `/api/sessions`).
  They never start a second server, so they are safe to run alongside the
  systemd service.
- **Status detector** — polls tmux session activity and emits `session_status`
  (working/idle) over the WebSocket, also driving Live Activity updates.
- **APNs push** (optional, needs `APNS_*` configured) — Live Activity
  background updates, and an alert when an Ollama tool-permission prompt is
  waiting for approval. No-ops when APNs is unconfigured.

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

### In-place upgrade (already deployed)

To upgrade an existing install to the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/kalleeh/continuum-relay/main/install.sh | sudo sh
```

The script detects OS/arch, verifies the SHA-256 against the release's
`checksums.txt`, replaces `/usr/local/bin/continuum-relay`, and restarts
the systemd unit (or LaunchDaemon on macOS). Pin a specific release with
`CONTINUUM_VERSION=v0.3.1`, or skip the restart with `CONTINUUM_NO_RESTART=1`.

This script does *not* provision a server, configure WireGuard, or set up
systemd — use `deploy.sh` for first-time installs.

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

## Commands

```
continuum-relay [command]

  serve              Run the relay server (default when no command is given)
  status             Show relay health and session count
  sessions           List sessions known to the running relay
  peers <subcommand> Manage WireGuard peers (list, add, remove)
  help               Show this help
```

Only `serve` (or no command at all) starts the server. `status`, `sessions`,
and `peers` are thin clients that talk to the already-running relay over HTTP —
they do **not** boot a second instance. An unrecognized command exits non-zero
with usage rather than falling through to a server start (which would otherwise
fail trying to grab the `wg0` TUN device the systemd service already owns).

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

`install.sh` and `deploy.sh` always pull from
`https://github.com/kalleeh/continuum-relay/releases/latest`. **Pushing
to `main` does not ship a fix** — users keep getting the previously-
tagged binary until you cut a new tag. If a server reports "already on
the right version" after you pushed a fix, that's why.

To ship: pick the next semver after `gh release view --json tagName`
and push the tag.

```bash
# 1. Push your fix to main first
git push origin main

# 2. Tag the commit you want released and push the tag
git tag v0.4.1
git push origin v0.4.1

# 3. Watch the workflow finish (~1m20s)
gh run watch "$(gh run list --workflow release.yml --limit 1 --json databaseId -q '.[0].databaseId')"
```

The `.github/workflows/release.yml` workflow fires on `v*` tag push,
runs in this repo (so the per-job `GITHUB_TOKEN` is enough — no cross-
repo PAT), cross-builds the four binaries via `make release`, refreshes
`checksums.txt` over binaries + `deploy.sh` + `cloud-init.yaml`, and
publishes a GitHub Release with everything attached.

After the release lands, the iOS submodule pin should be bumped — see
[CLAUDE.md "Release Process"](https://github.com/kalleeh/continuum-ios)
in the iOS repo for that step.

## License

MIT
