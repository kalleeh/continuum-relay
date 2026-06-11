#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLOUD_INIT="${SCRIPT_DIR}/cloud-init.yaml"

# ── Helpers ───────────────────────────────────────────────────────────────────

die() { gum style --foreground 1 --bold "ERROR: $*"; exit 1; }

require_gum() {
  command -v gum &>/dev/null && return
  echo "gum is required but not installed."
  echo "  brew install gum          # macOS"
  echo "  sudo apt install gum      # Debian/Ubuntu"
  echo "  go install github.com/charmbracelet/gum@latest"
  echo "See https://github.com/charmbracelet/gum"
  exit 1
}

require_cli() {
  local cmd=$1 name=$2
  command -v "$cmd" &>/dev/null && return
  gum style --foreground 1 --bold "$name CLI ('$cmd') is not installed."
  case "$cmd" in
    limactl)   gum style "  brew install lima" ;;
    aws)       gum style "  brew install awscli   # or pip install awscli" ;;
    doctl)     gum style "  brew install doctl" ;;
    hcloud)    gum style "  brew install hcloud" ;;
    wg)        gum style "  brew install wireguard-tools  # for WireGuard key generation" ;;
    qrencode)  gum style "  brew install qrencode" ;;
  esac
  exit 1
}

styled_box() { gum style --border double --padding "1 2" --border-foreground 6 "$@"; }

# ── Argument parsing ──────────────────────────────────────────────────────────
#
# Usage: ./deploy.sh {create|destroy|status} [provider] [flags]
#
# Providers: lightsail, ec2, do, hetzner, lima
#
# Flags:
#   --region REGION      Cloud region (e.g. eu-north-1)
#   --name NAME          Instance name (default: dev-box)
#   --bundle BUNDLE      Lightsail bundle (e.g. small_3_0)
#   --type TYPE          EC2 instance type (e.g. t3.small) or Hetzner server type
#   --size SIZE          DigitalOcean size (e.g. s-1vcpu-2gb)
#   --location LOC       Hetzner location (e.g. nbg1)
#   --cpus N             Lima CPUs
#   --memory MEM         Lima memory (e.g. 4GiB)
#   --peers N            WireGuard device count (default: 1)
#   --ssh-key PATH       SSH private key path, or 'new' to always generate
#   --key KEYPAIR        Provider SSH key pair name (optional fallback)
#   --bedrock-region R   Enable Bedrock with this region
#   --bedrock-key KEY    Bedrock API key (optional; uses IAM if omitted)
#   --yes, -y, --force   Skip all confirmation prompts
#   --ollama             Install Ollama on the server for local AI models

declare -A FLAGS=()
POSITIONAL_ARGS=()

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --yes|-y|--force)
        FLAGS[yes]="1"; shift ;;
      --*)
        local key="${1#--}"
        if [[ $# -gt 1 && "${2:-}" != --* ]]; then
          FLAGS["$key"]="$2"; shift 2
        else
          FLAGS["$key"]="1"; shift
        fi
        ;;
      *) POSITIONAL_ARGS+=("$1"); shift ;;
    esac
  done
}

# Load .env from $SCRIPT_DIR if present, mapping vars to FLAGS as defaults.
# CLI flags always take precedence over .env values.
load_env() {
  local env_file="$SCRIPT_DIR/.env"
  [[ -f "$env_file" ]] || return 0

  # Source the file in a subshell to extract values safely
  local region bundle name peers bedrock_region bedrock_key
  region=$(        grep -E '^DEPLOY_REGION='   "$env_file" | cut -d= -f2- | tr -d '"' || true)
  bundle=$(        grep -E '^DEPLOY_BUNDLE='   "$env_file" | cut -d= -f2- | tr -d '"' || true)
  name=$(          grep -E '^DEPLOY_NAME='     "$env_file" | cut -d= -f2- | tr -d '"' || true)
  peers=$(         grep -E '^DEPLOY_PEERS='    "$env_file" | cut -d= -f2- | tr -d '"' || true)
  bedrock_region=$(grep -E '^BEDROCK_REGION='  "$env_file" | cut -d= -f2- | tr -d '"' || true)
  bedrock_key=$(   grep -E '^BEDROCK_API_KEY=' "$env_file" | cut -d= -f2- | tr -d '"' || true)

  [[ -z "${FLAGS[region]:-}"         && -n "$region"         ]] && FLAGS[region]="$region"
  [[ -z "${FLAGS[bundle]:-}"         && -n "$bundle"         ]] && FLAGS[bundle]="$bundle"
  [[ -z "${FLAGS[name]:-}"           && -n "$name"           ]] && FLAGS[name]="$name"
  [[ -z "${FLAGS[peers]:-}"          && -n "$peers"          ]] && FLAGS[peers]="$peers"
  local bedrock_model bedrock_small_model
  bedrock_model=$(      grep -E '^BEDROCK_MODEL='       "$env_file" | cut -d= -f2- | tr -d '"' || true)
  bedrock_small_model=$(grep -E '^BEDROCK_SMALL_MODEL=' "$env_file" | cut -d= -f2- | tr -d '"' || true)

  [[ -z "${FLAGS[bedrock-region]:-}"      && -n "$bedrock_region"       ]] && FLAGS[bedrock-region]="$bedrock_region"
  [[ -z "${FLAGS[bedrock-key]:-}"         && -n "$bedrock_key"          ]] && FLAGS[bedrock-key]="$bedrock_key"
  [[ -z "${FLAGS[bedrock-model]:-}"       && -n "$bedrock_model"        ]] && FLAGS[bedrock-model]="$bedrock_model"
  [[ -z "${FLAGS[bedrock-small-model]:-}" && -n "$bedrock_small_model"  ]] && FLAGS[bedrock-small-model]="$bedrock_small_model"
}

# Use flag value if set, else run the gum fallback command.
# Usage: pick VAR_NAME FLAG_NAME gum choose/input ...
pick() {
  local _var=$1 _flag=$2
  shift 2
  if [[ -n "${FLAGS[$_flag]:-}" ]]; then
    printf -v "$_var" '%s' "${FLAGS[$_flag]}"
  else
    printf -v "$_var" '%s' "$("$@")"
  fi
}

# Auto-confirm if --yes, else prompt with gum.
yesno() {
  [[ "${FLAGS[yes]:-}" == "1" ]] && return 0
  gum confirm "$@"
}

# ── WireGuard VPN + SSH key + AI credentials helpers ─────────────────────────

WG_TMPDIR=""
WG_PEER_COUNT=1
SSH_PUBKEY=""
SSH_PRIVKEY_FILE=""
BEDROCK_API_KEY=""
BEDROCK_REGION=""
BEDROCK_MODEL=""
BEDROCK_SMALL_MODEL=""
MOBILE_APP="1"
CONTINUUM_TOKEN=""
OLLAMA_API_KEY=""
SERVER_NAME=""

# Arrays for multi-peer support
declare -a WG_PHONE_PRIVKEYS=()
declare -a WG_PHONE_PUBKEYS=()

setup_wireguard() {
  local peer_count
  pick peer_count "peers" gum choose --header "How many devices to configure?" "1" "2" "3"
  WG_PEER_COUNT="$peer_count"

  WG_TMPDIR=$(mktemp -d)

  # SSH key: --ssh-key flag, or interactive selection
  if [[ -n "${FLAGS[ssh-key]:-}" ]]; then
    local sk="${FLAGS[ssh-key]}"
    if [[ "$sk" == "new" ]]; then
      ssh-keygen -t ed25519 -f "$WG_TMPDIR/ssh_key" -N "" -C "deploy-$(date +%Y%m%d)" -q
      SSH_PUBKEY=$(cat "$WG_TMPDIR/ssh_key.pub")
      local key_name="dev-box-$(date +%Y%m%d-%H%M%S)"
      SSH_PRIVKEY_FILE="${SCRIPT_DIR}/${key_name}"
      install -m 600 "$WG_TMPDIR/ssh_key" "$SSH_PRIVKEY_FILE"
    elif [[ -f "$sk" ]]; then
      SSH_PRIVKEY_FILE="$sk"
      SSH_PUBKEY=$(ssh-keygen -y -f "$SSH_PRIVKEY_FILE")
    else
      die "SSH key not found: $sk"
    fi
  else
    # Interactive: reuse existing or generate new
    local existing_keys
    existing_keys=$(find "$SCRIPT_DIR" -maxdepth 1 -name "dev-box-*" ! -name "*.pub" -type f 2>/dev/null || true)

    # With --yes and no --ssh-key: auto-pick the most recent existing key or generate new
    if [[ "${FLAGS[yes]:-}" == "1" ]]; then
      local latest_key
      latest_key=$(echo "$existing_keys" | sort | tail -1)
      if [[ -n "$latest_key" ]]; then
        SSH_PRIVKEY_FILE="$latest_key"
        SSH_PUBKEY=$(ssh-keygen -y -f "$SSH_PRIVKEY_FILE")
      else
        ssh-keygen -t ed25519 -f "$WG_TMPDIR/ssh_key" -N "" -C "deploy-$(date +%Y%m%d)" -q
        SSH_PUBKEY=$(cat "$WG_TMPDIR/ssh_key.pub")
        local key_name="dev-box-$(date +%Y%m%d-%H%M%S)"
        SSH_PRIVKEY_FILE="${SCRIPT_DIR}/${key_name}"
        install -m 600 "$WG_TMPDIR/ssh_key" "$SSH_PRIVKEY_FILE"
      fi
    else
      local key_choice="Generate new key"
      if [[ -n "$existing_keys" ]]; then
        key_choice=$({ echo "$existing_keys"; echo "Generate new key"; echo "Enter custom path"; } | gum choose --header "SSH key")
      else
        key_choice=$(printf "Generate new key\nEnter custom path\n" | gum choose --header "SSH key")
      fi

      if [[ "$key_choice" == "Generate new key" ]]; then
        ssh-keygen -t ed25519 -f "$WG_TMPDIR/ssh_key" -N "" -C "deploy-$(date +%Y%m%d)" -q
        SSH_PUBKEY=$(cat "$WG_TMPDIR/ssh_key.pub")
        local key_name="dev-box-$(date +%Y%m%d-%H%M%S)"
        SSH_PRIVKEY_FILE="${SCRIPT_DIR}/${key_name}"
        install -m 600 "$WG_TMPDIR/ssh_key" "$SSH_PRIVKEY_FILE"
      elif [[ "$key_choice" == "Enter custom path" ]]; then
        SSH_PRIVKEY_FILE=$(gum input --header "Path to SSH private key")
        [[ ! -f "$SSH_PRIVKEY_FILE" ]] && die "Key not found: $SSH_PRIVKEY_FILE"
        SSH_PUBKEY=$(ssh-keygen -y -f "$SSH_PRIVKEY_FILE")
      else
        SSH_PRIVKEY_FILE="$key_choice"
        SSH_PUBKEY=$(ssh-keygen -y -f "$SSH_PRIVKEY_FILE")
      fi
    fi
  fi

  # Bedrock credentials: --bedrock-region flag, or interactive (skipped with --yes)
  if [[ -n "${FLAGS[bedrock-region]:-}" ]]; then
    BEDROCK_REGION="${FLAGS[bedrock-region]}"
    BEDROCK_API_KEY="${FLAGS[bedrock-key]:-}"
    BEDROCK_MODEL="${FLAGS[bedrock-model]:-eu.anthropic.claude-sonnet-4-6[1m]}"
    BEDROCK_SMALL_MODEL="${FLAGS[bedrock-small-model]:-eu.anthropic.claude-haiku-4-5-20251001-v1:0}"
  elif [[ "${FLAGS[yes]:-}" != "1" ]]; then
    if gum confirm "Configure AWS Bedrock for Claude Code / Kiro CLI?"; then
      BEDROCK_REGION=$(gum input --header "AWS Bedrock region" --value "eu-north-1")
      BEDROCK_API_KEY=$(gum input --header "Bedrock API key (or leave empty for IAM role)" --password)
    fi
  fi

  # Mobile app support is always enabled — Continuum relay + Ollama are core features.

  # Generate Continuum auth token
  gum style --bold "Generating Continuum auth token..."
  CONTINUUM_TOKEN=$(openssl rand -hex 32)

  # Ollama API key for web search/fetch tools
  if gum confirm "Configure Ollama API key for web search? (free from ollama.com/settings/keys)"; then
    OLLAMA_API_KEY=$(gum input --header "Ollama API key" --password)
  fi

  # Generate WireGuard server key pair
  wg genkey | tee "$WG_TMPDIR/server.key" | wg pubkey > "$WG_TMPDIR/server.pub"

  WG_SERVER_PRIVKEY=$(cat "$WG_TMPDIR/server.key")
  WG_SERVER_PUBKEY=$(cat "$WG_TMPDIR/server.pub")

  # Generate WireGuard key pairs for each device/peer
  WG_PHONE_PRIVKEYS=()
  WG_PHONE_PUBKEYS=()
  for i in $(seq 1 "$peer_count"); do
    wg genkey | tee "$WG_TMPDIR/phone${i}.key" | wg pubkey > "$WG_TMPDIR/phone${i}.pub"
    WG_PHONE_PRIVKEYS+=("$(cat "$WG_TMPDIR/phone${i}.key")")
    WG_PHONE_PUBKEYS+=("$(cat "$WG_TMPDIR/phone${i}.pub")")
  done
}

inject_mobile_app_to_cloud_init() {
  local yaml_file="$1"
  local token="$2"
  local ollama_key="${3:-}"

  python3 - "$yaml_file" "$token" "$ollama_key" <<'PYEOF'
import sys, yaml, shlex

yaml_file = sys.argv[1]
token = sys.argv[2]
ollama_key = sys.argv[3] if len(sys.argv) > 3 else ""

with open(yaml_file) as f:
    config = yaml.safe_load(f)

if 'write_files' not in config:
    config['write_files'] = []

config['write_files'].extend([
    {
        'path': '/etc/systemd/system/continuum-relay.service',
        'owner': 'root:root',
        'permissions': '0644',
        'content': (
            '[Unit]\n'
            'Description=Continuum Relay\n'
            'After=network.target\n'
            '\n'
            '[Service]\n'
            'Type=simple\n'
            'User=ubuntu\n'
            'EnvironmentFile=/etc/continuum/env\n'
            'ExecStart=/usr/local/bin/continuum-relay\n'
            'Restart=always\n'
            'RestartSec=5\n'
            'NoNewPrivileges=true\n'
            'PrivateTmp=true\n'
            'ProtectSystem=strict\n'
            '# /etc/wireguard must be writable: the relay updates wg0.conf\n'
            '# when peers are added/removed via `continuum-relay peers add|remove`.\n'
            'ReadWritePaths=/var/log/continuum /etc/wireguard\n'
            'ReadOnlyPaths=/etc/continuum\n'
            'AmbientCapabilities=CAP_NET_ADMIN\n'
            'CapabilityBoundingSet=CAP_NET_ADMIN\n'
            'StandardOutput=append:/var/log/continuum/relay.log\n'
            'StandardError=append:/var/log/continuum/relay.log\n'
            '\n'
            '[Install]\n'
            'WantedBy=multi-user.target\n'
        ),
    },
    {
        'path': '/etc/logrotate.d/continuum',
        'owner': 'root:root',
        'permissions': '0644',
        'content': (
            '/var/log/continuum/*.log {\n'
            '    daily\n'
            '    rotate 7\n'
            '    compress\n'
            '    missingok\n'
            '    notifempty\n'
            '    # copytruncate: systemd holds the append: log fd open and will\n'
            '    # not reopen on HUP, so truncate in place rather than rename.\n'
            '    copytruncate\n'
            '}\n'
        ),
    },
])

if 'runcmd' not in config:
    config['runcmd'] = []

config['runcmd'].extend([
    # Write /etc/continuum/env (auth token + relay config). install.sh detects
    # this file and takes its upgrade-only branch (download + verify + restart),
    # so we keep the unit + env in write_files above and let install.sh own the
    # binary install + checksum verification.
    'mkdir -p /etc/continuum /var/log/continuum',
    f"printf 'CONTINUUM_TOKEN=%s\\nCONTINUUM_RELAY_ADDR=10.100.0.1:7682\\nCONTINUUM_RELAY_LOG=/var/log/continuum/relay.log\\nOLLAMA_HOST=http://10.100.0.1:11434\\n' {shlex.quote(token)} > /etc/continuum/env",
    'chmod 600 /etc/continuum/env && chown ubuntu:ubuntu /etc/continuum/env',
    *([ f"sed -i '$ a OLLAMA_API_KEY={shlex.quote(ollama_key)}' /etc/continuum/env" ] if ollama_key else []),
    # Reload systemd so it sees the unit written by write_files, enable for
    # boot, then run install.sh which downloads + verifies + restarts.
    'systemctl daemon-reload',
    'systemctl enable continuum-relay',
    'curl -fsSL https://raw.githubusercontent.com/kalleeh/continuum-relay/main/install.sh | sh',
    # Install Ollama and bind to WireGuard interface
    'curl -fsSL https://ollama.com/install.sh | sh',
    'mkdir -p /etc/systemd/system/ollama.service.d',
    "printf '[Service]\\nEnvironment=OLLAMA_HOST=10.100.0.1:11434\\n' > /etc/systemd/system/ollama.service.d/override.conf",
    'systemctl daemon-reload && systemctl restart ollama',
    # Open ports for WireGuard peers only
    'ufw allow from 10.100.0.0/24 to any port 7682 proto tcp comment "Continuum relay (WireGuard only)"',
    'ufw allow from 10.100.0.0/24 to any port 11434 proto tcp comment "Ollama (WireGuard only)"',
])

with open(yaml_file, 'w') as f:
    yaml.dump(config, f, default_flow_style=False, allow_unicode=True)

print("Injected mobile-app config into cloud-init.yaml")
PYEOF
}

build_cloud_init() {
  [[ -f "$CLOUD_INIT" ]] || die "cloud-init.yaml not found at $CLOUD_INIT (not needed for 'any' provider)"
  local user="${1:-ubuntu}"
  local provider="${2:-}"
  local tmp_ci="$WG_TMPDIR/cloud-init-wg.yaml"
  cp "$CLOUD_INIT" "$tmp_ci"

  # Inject generated SSH public key into cloud-init as a top-level directive.
  # Must be inserted near the top (after #cloud-config), not appended at the end,
  # because appending would place it inside or after the runcmd block.
  local tmp_key="$WG_TMPDIR/cloud-init-key.yaml"
  {
    head -1 "$tmp_ci"  # #cloud-config line
    echo ""
    echo "ssh_authorized_keys:"
    echo "  - ${SSH_PUBKEY}"
    echo ""
    tail -n +2 "$tmp_ci"  # rest of the file
  } > "$tmp_key"
  mv "$tmp_key" "$tmp_ci"

  # Fix user paths per provider (#1)
  # DigitalOcean and Hetzner use root as default user
  if [[ "$user" == "root" ]]; then
    sed "s|/home/ubuntu|/root|g; s|ubuntu:ubuntu|root:root|g; s|chsh -s /usr/bin/zsh ubuntu|chsh -s /usr/bin/zsh root|g; s|chown -R ubuntu:ubuntu|chown -R root:root|g" "$tmp_ci" > "${tmp_ci}.tmp"
    mv "${tmp_ci}.tmp" "$tmp_ci"
  fi

  # Build WireGuard peer blocks for all devices
  local peers_block=""
  for i in $(seq 1 "$WG_PEER_COUNT"); do
    local idx=$((i - 1))
    local peer_ip="10.100.0.$((i + 1))"
    peers_block+="
      [Peer]  # Device ${i}
      PublicKey = ${WG_PHONE_PUBKEYS[$idx]}
      AllowedIPs = ${peer_ip}/32
"
  done

  # Append WireGuard server config to write_files section.
  # We insert it just before the "runcmd:" line so it lands inside write_files.
  local wg_write_files
  wg_write_files=$(cat <<WGEOF

  # -- WireGuard server config --
  - path: /etc/wireguard/wg0.conf
    # Owned by the relay's runtime user (ubuntu) so it can read at startup
    # and append/remove peers via `continuum-relay peers add|remove`.
    owner: ubuntu:ubuntu
    permissions: "0600"
    content: |
      [Interface]
      PrivateKey = ${WG_SERVER_PRIVKEY}
      Address = 10.100.0.1/24
      ListenPort = 51820
${peers_block}
WGEOF
  )

  # Insert WireGuard write_files entry before the runcmd section.
  # Uses a temp file approach instead of awk -v (BSD awk on macOS
  # cannot handle multi-line strings in -v assignments).
  local wg_block_file="$WG_TMPDIR/wg-block.yaml"
  echo "$wg_write_files" > "$wg_block_file"
  local tmp_insert="$WG_TMPDIR/cloud-init-insert.yaml"
  local runcmd_line
  runcmd_line=$(grep -n '^runcmd:' "$tmp_ci" | head -1 | cut -d: -f1)
  if [[ -n "$runcmd_line" ]]; then
    head -n "$((runcmd_line - 1))" "$tmp_ci" > "$tmp_insert"
    cat "$wg_block_file" >> "$tmp_insert"
    echo "" >> "$tmp_insert"

    # Add AI credentials file if configured
    if [[ -n "$BEDROCK_REGION" ]]; then
      local ai_home="/home/ubuntu"
      [[ "$user" == "root" ]] && ai_home="/root"
      local ai_content="export CLAUDE_CODE_USE_BEDROCK=1\nexport AWS_REGION=${BEDROCK_REGION}"
      [[ -n "$BEDROCK_API_KEY" ]] && ai_content="${ai_content}\nexport AWS_BEARER_TOKEN_BEDROCK=${BEDROCK_API_KEY}"
      {
        echo "  # -- AI credentials (Claude Code / Kiro CLI) --"
        echo "  - path: ${ai_home}/.ai-credentials"
        echo "    owner: ${user}:${user}"
        echo '    permissions: "0600"'
        echo "    content: |"
        echo "      export CLAUDE_CODE_USE_BEDROCK=1"
        echo "      export AWS_REGION=${BEDROCK_REGION}"
        echo "      export ANTHROPIC_MODEL='${BEDROCK_MODEL}'"
        echo "      export ANTHROPIC_SMALL_FAST_MODEL='${BEDROCK_SMALL_MODEL}'"
        if [[ -n "$BEDROCK_API_KEY" ]]; then
          echo "      export AWS_BEARER_TOKEN_BEDROCK=${BEDROCK_API_KEY}"
        fi
        echo ""
      } >> "$tmp_insert"
    fi
    tail -n +"$runcmd_line" "$tmp_ci" >> "$tmp_insert"
    mv "$tmp_insert" "$tmp_ci"
  fi

  # Inject mobile-app services (ttyd + continuum-relay) when is set.
  # This modifies the temp copy only; the base cloud-init.yaml is never touched.
  if [[ -n "$MOBILE_APP" ]]; then
    inject_mobile_app_to_cloud_init "$tmp_ci" "$CONTINUUM_TOKEN" "$OLLAMA_API_KEY"
  fi

  # Append WireGuard runcmd entries and firewall rules at the end.
  # Providers with their own firewall (Lightsail, EC2) allow SSH from
  # anywhere in ufw since the provider firewall is the outer security layer.
  # Providers without a firewall (DO, Hetzner) restrict SSH to WireGuard only.
  if [[ "$provider" == "lightsail" || "$provider" == "ec2" ]]; then
    cat >> "$tmp_ci" <<RUNCMD

  # -- SSH key injection (Lightsail overrides ssh_authorized_keys) --
  - echo "${SSH_PUBKEY}" >> /home/${user}/.ssh/authorized_keys

  # -- Firewall --
  - ufw default deny incoming
  - ufw allow 51820/udp
  - ufw allow 22/tcp
  - ufw --force enable
RUNCMD
  else
    cat >> "$tmp_ci" <<RUNCMD

  # -- SSH key injection --
  - echo "${SSH_PUBKEY}" >> /home/${user}/.ssh/authorized_keys

  # -- Firewall --
  - ufw default deny incoming
  - ufw allow 51820/udp
  - ufw allow from 10.100.0.0/24 to any port 22
  - ufw --force enable
RUNCMD
  fi

  # Cloud-init completion marker (#8)
  echo "  - touch /var/lib/cloud/.cloud-init-complete" >> "$tmp_ci"

  # Lightsail doesn't support #cloud-config YAML natively.
  # Its --user-data only accepts shell scripts. We wrap the complete
  # cloud-init YAML (already built above with all injections) in a
  # thin bash script that writes it to disk and runs cloud-init's
  # own modules to process it. No custom parser needed.
  if [[ "$provider" == "lightsail" ]]; then
    local tmp_wrapper="$WG_TMPDIR/cloud-init-wrapper.sh"
    {
      echo "#!/bin/bash"
      echo "# Write the complete cloud-init config"
      echo "cat > /etc/cloud/cloud.cfg.d/99-devbox.cfg << 'ENDOFCLOUDINIT'"
      # Paste the fully-built cloud-init YAML (minus the #cloud-config header)
      tail -n +2 "$tmp_ci"
      echo "ENDOFCLOUDINIT"
      echo ""
      echo "# Process using cloud-init's own modules"
      echo "# --frequency always forces execution even if modules already ran"
      echo "cloud-init single --name write_files --frequency always"
      echo "cloud-init single --name package_update_upgrade_install --frequency always"
      echo "cloud-init single --name runcmd --frequency always"
      echo ""
      echo "# Execute runcmd script (cloud-init single doesn't actually run it)"
      echo "bash /var/lib/cloud/instances/*/scripts/runcmd || true"
      echo ""
      echo "# Scrub secrets from user-data cache"
      echo "rm -f /var/lib/cloud/instance/scripts/part-001"
      echo "rm -f /var/lib/cloud/instance/user-data.txt"
      echo "rm -f /var/lib/cloud/instances/*/user-data.txt 2>/dev/null"
      echo "iptables -A OUTPUT -d 169.254.169.254 -p tcp --dport 80 -m string --string \"user-data\" --algo bm -j DROP 2>/dev/null || true"
    } > "$tmp_wrapper"
    echo "$tmp_wrapper"
    return
  fi

  echo "$tmp_ci"
}

show_qr() {
  local server_ip="$1" ssh_user="$2"

  echo ""
  gum style --bold --foreground 2 "✓ Deployment complete!"
  echo ""
  gum style --bold --foreground 6 "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  gum style --bold --foreground 6 "  WireGuard VPN Setup"
  gum style --bold --foreground 6 "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo ""

  # Show QR code for each device
  for i in $(seq 1 "$WG_PEER_COUNT"); do
    local idx=$((i - 1))
    local peer_ip="10.100.0.$((i + 1))"

    local phone_conf
    phone_conf=$(cat <<PHONEEOF
[Interface]
PrivateKey = ${WG_PHONE_PRIVKEYS[$idx]}
Address = ${peer_ip}/24
DNS = 1.1.1.1

[Peer]
PublicKey = ${WG_SERVER_PUBKEY}
Endpoint = ${server_ip}:51820
AllowedIPs = 10.100.0.1/32
PersistentKeepalive = 25
PHONEEOF
    )

    if [[ "$WG_PEER_COUNT" -gt 1 ]]; then
      gum style --bold --foreground 6 "── Device ${i} of ${WG_PEER_COUNT} ──"
      echo ""
    fi

    gum style --bold "📱 Step 1: Scan QR code with WireGuard app"
    gum style --bold "🔌 Step 2: Enable the tunnel"
    gum style --bold "🔐 Step 3: SSH via VPN tunnel"
    echo ""

    if [[ -n "$SSH_PRIVKEY_FILE" ]]; then
      gum style --foreground 2 "   ssh -i ${SSH_PRIVKEY_FILE} ${ssh_user}@10.100.0.1"
    else
      gum style --foreground 2 "   ssh ${ssh_user}@10.100.0.1"
    fi
    echo ""

    echo "$phone_conf" | qrencode -t ansiutf8
    echo ""

    gum style --foreground 8 "Manual config (if QR doesn't scan):"
    gum style --foreground 8 "$phone_conf"
    echo ""

    # Save config locally so QR can be regenerated later
    local conf_file="${SCRIPT_DIR}/wireguard-device${i}.conf"
    echo "$phone_conf" > "$conf_file"
    chmod 600 "$conf_file"
  done

  gum style --foreground 6 "WireGuard configs saved to ${SCRIPT_DIR}/wireguard-device*.conf"
  gum style --foreground 6 "Regenerate QR anytime: qrencode -t ansiutf8 < wireguard-device1.conf"
  echo ""

  styled_box \
    "SSH over WireGuard:" \
    "  ssh -i ${SSH_PRIVKEY_FILE} ${ssh_user}@10.100.0.1" \
    "" \
    "SSH private key saved to:" \
    "  ${SSH_PRIVKEY_FILE}" \
    "" \
    "To use from iOS (Termius):" \
    "  AirDrop ${SSH_PRIVKEY_FILE} to your phone"

  # Continuum iOS app QR code (only when was passed)
  if [[ -n "$MOBILE_APP" ]]; then
    local wg_client_privkey="${WG_PHONE_PRIVKEYS[0]}"
    local wg_server_pubkey="$WG_SERVER_PUBKEY"
    local continuum_payload
    # JSON-escape user-controlled string fields
    local server_name_json server_ip_json
    server_name_json=$(python3 -c "import sys,json; print(json.dumps(sys.argv[1]))" "${SERVER_NAME:-dev-box}")
    server_ip_json=$(python3 -c "import sys,json; print(json.dumps(sys.argv[1]))" "${server_ip}")
    continuum_payload=$(cat <<CJSON
{
  "v": 1,
  "serverName": ${server_name_json},
  "serverPublicIP": ${server_ip_json},
  "wgServerEndpoint": "${server_ip}:51820",
  "wgServerPublicKey": "${wg_server_pubkey}",
  "wgClientPrivateKey": "${wg_client_privkey}",
  "wgClientAddress": "10.100.0.2/24",
  "wgDNS": "1.1.1.1",
  "wgKeepalive": 25,
  "authToken": "${CONTINUUM_TOKEN}",
  "ttydPort": 7681,
  "relayPort": 7682
}
CJSON
    )
    echo ""
    gum style --bold --foreground 6 "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    gum style --bold --foreground 6 "  Continuum iOS App Setup"
    gum style --bold --foreground 6 "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    gum style --bold "Scan this QR code with the Continuum app:"
    echo ""
    echo "$continuum_payload" | qrencode -t UTF8
    echo ""
    gum style --foreground 8 "Or copy this JSON manually into the app:"
    gum style --foreground 8 "$continuum_payload"
    echo ""
    gum style --foreground 1 --bold "This QR code contains your private key. Do not photograph or share it."
    echo ""
  fi

  # Cloud-init progress note (#8)
  echo ""
  gum style --foreground 3 "Cloud-init is still configuring the server. Wait 2-3 minutes after"
  gum style --foreground 3 "connecting before all tools are available."
  gum style --foreground 3 "Check progress with: tail -f /var/log/cloud-init-output.log"
}

cleanup_wireguard() {
  [[ -n "$WG_TMPDIR" && -d "$WG_TMPDIR" ]] && rm -rf "$WG_TMPDIR"
}

# ── Local IP helpers ──────────────────────────────────────────────────────────

get_brew_prefix() {
  [[ -d /opt/homebrew ]] && echo "/opt/homebrew" || echo "/usr/local"
}

detect_public_ip() {
  curl -sf --max-time 5 https://api.ipify.org 2>/dev/null \
    || curl -sf --max-time 5 https://icanhazip.com 2>/dev/null \
    || true
}

detect_lan_ip() {
  case "$(uname -s)" in
    Darwin)
      for iface in en0 en1 en2 en3; do
        local ip; ip=$(ipconfig getifaddr "$iface" 2>/dev/null) && { echo "$ip"; return; }
      done ;;
    Linux)
      ip route get 8.8.8.8 2>/dev/null | awk '/src/ {print $7}' | head -1 ;;
  esac
}

# Present detected public + LAN IPs and let user choose
pick_server_ip() {
  local public lan
  public=$(detect_public_ip)
  lan=$(detect_lan_ip)

  local -a choices=()
  local ip_re='^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$'
  [[ -n "$public" && "$public" =~ $ip_re ]] && choices+=("${public}  (public — any network; needs UDP 51820 forwarded to this machine)")
  [[ -n "$lan"    && "$lan"    =~ $ip_re ]] && choices+=("${lan}  (local — same WiFi/LAN only)")
  choices+=("Enter custom hostname or IP...")

  local choice
  choice=$(printf '%s\n' "${choices[@]}" | gum choose --header "Which address should the iPhone use to connect?")

  if [[ "$choice" == "Enter custom"* ]]; then
    local input
    while true; do
      input=$(gum input --header "IP address or hostname (e.g. 1.2.3.4 or myserver.com)")
      # Basic validation: allow IPs and hostnames (letters, digits, dots, hyphens)
      if [[ "$input" =~ ^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$ ]] && [[ -n "$input" ]]; then
        echo "$input"
        break
      fi
      gum style --foreground 1 "Invalid address. Use an IP address (1.2.3.4) or hostname (myserver.com)."
    done
  else
    echo "${choice%%  *}"   # strip annotation, keep the IP
  fi
}

# ── Provider: Lima (local) ────────────────────────────────────────────────────

lima_create() {
  local cpus memory name
  pick cpus   "cpus"   gum choose --header "CPUs" 1 2 4
  pick memory "memory" gum choose --header "Memory" "2GiB" "4GiB" "8GiB"
  pick name   "name"   gum input  --header "Instance name" --value "dev-box"

  gum style --bold "Summary"
  styled_box "Provider: Lima (local)" "CPUs:     $cpus" "Memory:   $memory" "Name:     $name"
  yesno "Create this VM?" || exit 0

  local tmpfile
  tmpfile=$(mktemp /tmp/lima-XXXXXX.yaml)
  trap 'rm -f "$tmpfile"' EXIT

  # Build Lima YAML with embedded cloud-init runcmd as provision script
  cat > "$tmpfile" <<YAML
images:
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-arm64.img"
    arch: "aarch64"
  - location: "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
    arch: "x86_64"
cpus: ${cpus}
memory: "${memory}"
vmType: "vz"
mountType: "virtiofs"
YAML

  # Append cloud-init provision block -- embed the full cloud-init YAML
  {
    echo "provision:"
    echo "  - mode: cloud-init"
    echo "    script: |"
    sed 's/^/      /' "$CLOUD_INIT"
  } >> "$tmpfile"

  gum spin --title "Creating Lima VM '$name'..." -- limactl create --name "$name" "$tmpfile" --tty=false
  gum spin --title "Starting Lima VM '$name'..." -- limactl start "$name"

  styled_box "Lima VM '$name' is running!" "" "Connect with:" "  limactl shell $name"
}

lima_destroy() {
  local instances
  instances=$(limactl list --format '{{.Name}}' 2>/dev/null | grep -v '^$' || true)
  [[ -z "$instances" ]] && die "No Lima instances found."

  local target
  if [[ -n "${FLAGS[name]:-}" ]]; then
    target="${FLAGS[name]}"
  else
    target=$(echo "$instances" | gum choose --header "Select instance to destroy")
  fi
  yesno "Destroy Lima VM '$target'? This is irreversible." || exit 0
  gum spin --title "Destroying '$target'..." -- limactl delete --force "$target"
  gum style --foreground 2 --bold "Destroyed '$target'."
  gum style --foreground 3 "Remember to remove the WireGuard tunnel from your phone/device."
}

lima_status() {
  limactl list
}

# ── Provider: AWS Lightsail ───────────────────────────────────────────────────

lightsail_create() {
  local region bundle key_name name ssh_user="ubuntu"
  pick region "region" gum choose --header "Region" us-east-1 eu-west-1 eu-north-1 ap-southeast-1
  pick bundle "bundle" gum choose --header "Bundle" small_3_0 medium_3_0 large_3_0

  # SSH key pair from provider API (optional fallback)
  local available_keys key_name=""
  if [[ -n "${FLAGS[key]:-}" ]]; then
    key_name="${FLAGS[key]}"
  else
    available_keys=$(aws lightsail get-key-pairs --region "$region" --query 'keyPairs[].name' --output text | tr '\t' '\n' || true)
    if [[ -n "$available_keys" ]]; then
      key_name=$(echo "$available_keys" "$(gum style --faint '(skip - use generated key only)')" | gum choose --header "SSH key (optional)")
      [[ "$key_name" == *"skip"* ]] && key_name=""
    fi
  fi

  pick name "name" gum input --header "Instance name" --value "dev-box"

  local key_display="${key_name:-generated (via cloud-init)}"
  gum style --bold "Summary"
  styled_box "Provider: AWS Lightsail" "Region:   $region" "Bundle:   $bundle" "SSH key:  $key_display" "Name:     $name"

  # Check if instance already exists
  if aws lightsail get-instance --instance-name "$name" --region "$region" &>/dev/null; then
    gum style --foreground 3 "⚠ Instance '$name' already exists in $region"
    if ! yesno "Delete existing instance and recreate?"; then
      exit 0
    fi
    gum spin --title "Deleting existing instance..." -- \
      aws lightsail delete-instance --instance-name "$name" --region "$region"
    # Release static IP so it can be reallocated with the same name
    aws lightsail release-static-ip --static-ip-name "${name}-ip" --region "$region" 2>/dev/null || true
    sleep 5
  fi

  yesno "Create this instance?" || exit 0

  # Set up WireGuard keys and build modified cloud-init
  trap 'cleanup_wireguard' EXIT
  setup_wireguard
  local wg_cloud_init
  wg_cloud_init=$(build_cloud_init "$ssh_user" "lightsail")

  local key_flag=()
  [[ -n "$key_name" ]] && key_flag=(--key-pair-name "$key_name")

  if ! gum spin --title "Creating Lightsail instance '$name'..." -- \
    aws lightsail create-instances \
      --instance-names "$name" \
      --availability-zone "${region}a" \
      --blueprint-id ubuntu_24_04 \
      --bundle-id "$bundle" \
      "${key_flag[@]}" \
      --user-data "$(cat "$wg_cloud_init")" \
      --region "$region" 2>&1; then
    die "Failed to create instance. Instance '$name' may already exist in region $region."
  fi

  gum spin --title "Waiting for instance to be running..." -- \
    aws lightsail wait instance-running --instance-name "$name" --region "$region" 2>/dev/null || sleep 15

  # Update Lightsail firewall: replace default rules (SSH 22) with WireGuard only
  gum spin --title "Configuring firewall (WireGuard only)..." -- \
    aws lightsail put-instance-public-ports \
      --instance-name "$name" \
      --port-infos "fromPort=51820,toPort=51820,protocol=udp" \
      --region "$region"

  # Allocate and attach a static IP (#5)
  local static_ip_name="${name}-ip"
  gum spin --title "Allocating static IP..." -- \
    aws lightsail allocate-static-ip --static-ip-name "$static_ip_name" --region "$region"
  gum spin --title "Attaching static IP..." -- \
    aws lightsail attach-static-ip --static-ip-name "$static_ip_name" --instance-name "$name" --region "$region"

  local ip
  ip=$(aws lightsail get-static-ip --static-ip-name "$static_ip_name" --region "$region" \
       --query 'staticIp.ipAddress' --output text)

  gum style --foreground 6 "⏳ Waiting for cloud-init to complete (2-5 minutes)..."
  gum style --foreground 8 "   Installing packages, configuring WireGuard, setting up dev tools..."
  echo ""

  SERVER_NAME="$name"
  show_qr "$ip" "$ssh_user"

  # WireGuard verify: skipped with --yes
  echo ""
  if [[ "${FLAGS[yes]:-}" != "1" ]] && gum confirm "Verify WireGuard is running? (requires WireGuard tunnel active)"; then
    gum style --foreground 6 "Testing WireGuard connection..."
    if timeout 5 ping -c 1 10.100.0.1 &>/dev/null; then
      gum style --foreground 2 "✓ WireGuard tunnel is working!"
    else
      gum style --foreground 3 "⚠ Cannot reach 10.100.0.1 - make sure WireGuard tunnel is active on your device"
    fi
  fi
}

lightsail_destroy() {
  local region instances target
  pick region "region" gum choose --header "Region" us-east-1 eu-west-1 eu-north-1 ap-southeast-1
  instances=$(aws lightsail get-instances --region "$region" \
    --query 'instances[].name' --output text | tr '\t' '\n' || true)
  [[ -z "$instances" ]] && die "No Lightsail instances in $region."

  if [[ -n "${FLAGS[name]:-}" ]]; then
    target="${FLAGS[name]}"
  else
    target=$(echo "$instances" | gum choose --header "Select instance to destroy")
  fi
  yesno "Destroy Lightsail instance '$target'? This is irreversible." || exit 0
  gum spin --title "Destroying '$target'..." -- \
    aws lightsail delete-instance --instance-name "$target" --region "$region"

  # Release the static IP (#5)
  aws lightsail release-static-ip --static-ip-name "${target}-ip" --region "$region" 2>/dev/null || true

  gum style --foreground 2 --bold "Destroyed '$target'."
  gum style --foreground 3 "Remember to remove the WireGuard tunnel from your phone/device."
}

lightsail_status() {
  aws lightsail get-instances --query \
    'instances[].{Name:name,State:state.name,IP:publicIpAddress,Blueprint:blueprintId}' \
    --output table
}

# ── Provider: AWS EC2 ─────────────────────────────────────────────────────────

ec2_create() {
  local region itype key_name name ssh_user="ubuntu"
  pick region "region" gum choose --header "Region" us-east-1 us-west-2 eu-west-1 eu-north-1 ap-southeast-1
  pick itype  "type"   gum choose --header "Instance type" t3.small t3.medium t3.large

  # SSH key pair from provider API (optional)
  local available_keys key_name=""
  if [[ -n "${FLAGS[key]:-}" ]]; then
    key_name="${FLAGS[key]}"
  else
    available_keys=$(aws ec2 describe-key-pairs --region "$region" --query 'KeyPairs[].KeyName' --output text | tr '\t' '\n' || true)
    if [[ -n "$available_keys" ]]; then
      key_name=$(echo "$available_keys" "$(gum style --faint '(skip - use generated key only)')" | gum choose --header "SSH key (optional)")
      [[ "$key_name" == *"skip"* ]] && key_name=""
    fi
  fi

  pick name "name" gum input --header "Instance name" --value "dev-box"

  local key_display="${key_name:-generated (via cloud-init)}"
  gum style --bold "Summary"
  styled_box "Provider: AWS EC2" "Region:   $region" "Type:     $itype" "Key pair: $key_display" "Name:     $name"
  yesno "Create this instance?" || exit 0

  # Set up WireGuard keys and build modified cloud-init
  trap 'cleanup_wireguard' EXIT
  setup_wireguard
  local wg_cloud_init
  wg_cloud_init=$(build_cloud_init "$ssh_user" "ec2")

  # Look up latest Ubuntu 24.04 AMI
  local ami
  ami=$(gum spin --title "Looking up Ubuntu 24.04 AMI..." --show-output -- \
    aws ec2 describe-images --region "$region" \
      --owners 099720109477 \
      --filters "Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*" \
                "Name=state,Values=available" \
      --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
  [[ -z "$ami" || "$ami" == "None" ]] && die "Could not find Ubuntu 24.04 AMI in $region."

  # Create security group (WireGuard UDP only - no SSH from public internet)
  local sg_name="coder-deploy-${name}" sg_id vpc_id
  vpc_id=$(aws ec2 describe-vpcs --region "$region" \
    --filters "Name=isDefault,Values=true" \
    --query 'Vpcs[0].VpcId' --output text)

  sg_id=$(aws ec2 describe-security-groups --region "$region" \
    --filters "Name=group-name,Values=$sg_name" "Name=vpc-id,Values=$vpc_id" \
    --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || echo "None")

  if [[ "$sg_id" == "None" || -z "$sg_id" ]]; then
    sg_id=$(gum spin --title "Creating security group..." --show-output -- \
      aws ec2 create-security-group --region "$region" \
        --group-name "$sg_name" --description "WireGuard access for $name" \
        --vpc-id "$vpc_id" --query 'GroupId' --output text)
    aws ec2 authorize-security-group-ingress --region "$region" \
      --group-id "$sg_id" --protocol udp --port 51820 --cidr 0.0.0.0/0 >/dev/null
  fi

  # Launch instance
  local key_flag=()
  [[ -n "$key_name" ]] && key_flag=(--key-name "$key_name")

  local instance_id
  if ! instance_id=$(gum spin --title "Launching EC2 instance..." --show-output -- \
    aws ec2 run-instances --region "$region" \
      --image-id "$ami" --instance-type "$itype" \
      "${key_flag[@]}" --security-group-ids "$sg_id" \
      --user-data "file://${wg_cloud_init}" \
      --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$name}]" \
      --query 'Instances[0].InstanceId' --output text 2>&1); then
    die "Failed to launch EC2 instance: $instance_id"
  fi

  gum spin --title "Waiting for instance to be running..." -- \
    aws ec2 wait instance-running --region "$region" --instance-ids "$instance_id"

  # Allocate and associate an Elastic IP (#5)
  local alloc_id
  alloc_id=$(gum spin --title "Allocating Elastic IP..." --show-output -- \
    aws ec2 allocate-address --region "$region" --query 'AllocationId' --output text)
  gum spin --title "Associating Elastic IP..." -- \
    aws ec2 associate-address --region "$region" --instance-id "$instance_id" --allocation-id "$alloc_id"

  local ip
  ip=$(aws ec2 describe-addresses --region "$region" --allocation-ids "$alloc_id" \
       --query 'Addresses[0].PublicIp' --output text)

  SERVER_NAME="$name"
  show_qr "$ip" "$ssh_user"
}

ec2_destroy() {
  local region instances target iid
  pick region "region" gum choose --header "Region" us-east-1 us-west-2 eu-west-1 eu-north-1 ap-southeast-1
  instances=$(aws ec2 describe-instances --region "$region" \
    --filters "Name=instance-state-name,Values=running,stopped" \
    --query 'Reservations[].Instances[].[InstanceId,Tags[?Key==`Name`].Value|[0]]' \
    --output text | while read -r id nm; do echo "${id}  ${nm:-(unnamed)}"; done || true)
  [[ -z "$instances" ]] && die "No EC2 instances in $region."

  if [[ -n "${FLAGS[name]:-}" ]]; then
    iid=$(aws ec2 describe-instances --region "$region" \
      --filters "Name=instance-state-name,Values=running,stopped" \
                "Name=tag:Name,Values=$(printf '%s' "${FLAGS[name]}" | sed 's/[^a-zA-Z0-9_-]//g')" \
      --query 'Reservations[].Instances[0].InstanceId' --output text)
    [[ -z "$iid" || "$iid" == "None" ]] && die "No EC2 instance named '${FLAGS[name]}' in $region."
    target="$iid  ${FLAGS[name]}"
  else
    target=$(echo "$instances" | gum choose --header "Select instance to terminate")
    iid=$(echo "$target" | awk '{print $1}')
  fi
  yesno "Terminate EC2 instance $iid? This is irreversible." || exit 0

  # Release Elastic IP before terminating (#5)
  local eip_alloc
  eip_alloc=$(aws ec2 describe-addresses --region "$region" \
    --filters "Name=instance-id,Values=$iid" \
    --query 'Addresses[0].AllocationId' --output text 2>/dev/null || echo "None")
  if [[ "$eip_alloc" != "None" && -n "$eip_alloc" ]]; then
    aws ec2 release-address --region "$region" --allocation-id "$eip_alloc" 2>/dev/null || true
  fi

  gum spin --title "Terminating '$iid'..." -- \
    aws ec2 terminate-instances --region "$region" --instance-ids "$iid"

  # Try to clean up security group (#3)
  local sg_name="coder-deploy-$(echo "$target" | awk '{print $2}')"
  local sg_id
  sg_id=$(aws ec2 describe-security-groups --region "$region" \
    --filters "Name=group-name,Values=$sg_name" \
    --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || echo "None")
  if [[ "$sg_id" != "None" && -n "$sg_id" ]]; then
    sleep 5  # Wait for instance to fully release the SG
    aws ec2 delete-security-group --region "$region" --group-id "$sg_id" 2>/dev/null || true
  fi

  gum style --foreground 2 --bold "Terminated '$iid'."
  gum style --foreground 3 "Remember to remove the WireGuard tunnel from your phone/device."
}

ec2_status() {
  aws ec2 describe-instances \
    --filters "Name=instance-state-name,Values=running,stopped" \
    --query 'Reservations[].Instances[].{ID:InstanceId,Name:Tags[?Key==`Name`].Value|[0],State:State.Name,IP:PublicIpAddress,Type:InstanceType}' \
    --output table
}

# ── Provider: DigitalOcean ────────────────────────────────────────────────────

do_create() {
  local region size ssh_key name ssh_user="root"
  pick region "region" gum choose --header "Region" nyc1 lon1 fra1 sgp1
  pick size   "size"   gum choose --header "Size" s-1vcpu-2gb s-2vcpu-4gb s-4vcpu-8gb

  # SSH key from provider API (optional)
  local available_keys ssh_key=""
  if [[ -n "${FLAGS[key]:-}" ]]; then
    ssh_key="${FLAGS[key]}"
  else
    available_keys=$(doctl compute ssh-key list --format Name --no-header || true)
    if [[ -n "$available_keys" ]]; then
      ssh_key=$(echo "$available_keys" "$(gum style --faint '(skip - use generated key only)')" | gum choose --header "SSH key (optional)")
      [[ "$ssh_key" == *"skip"* ]] && ssh_key=""
    fi
  fi

  pick name "name" gum input --header "Droplet name" --value "dev-box"

  local key_display="${ssh_key:-generated (via cloud-init)}"
  gum style --bold "Summary"
  styled_box "Provider: DigitalOcean" "Region:   $region" "Size:     $size" "SSH key:  $key_display" "Name:     $name"
  gum style --foreground 3 "Warning: DigitalOcean creates droplets with no provider-level firewall."
  gum style --foreground 3 "All ports are open until cloud-init configures ufw (1-2 minutes)."
  gum style --foreground 3 "Consider adding a DO cloud firewall after creation for defense-in-depth."
  yesno "Create this droplet?" || exit 0

  # Set up WireGuard keys and build modified cloud-init
  trap 'cleanup_wireguard' EXIT
  setup_wireguard
  local wg_cloud_init
  wg_cloud_init=$(build_cloud_init "$ssh_user")

  local droplet_id
  local key_flag=()
  [[ -n "$ssh_key" ]] && key_flag=(--ssh-keys "$ssh_key")

  if ! droplet_id=$(gum spin --title "Creating droplet '$name'..." --show-output -- \
    doctl compute droplet create "$name" \
      --region "$region" --size "$size" --image ubuntu-24-04-x64 \
      "${key_flag[@]}" --user-data-file "$wg_cloud_init" \
      --wait --format ID --no-header 2>&1); then
    die "Failed to create droplet. Droplet '$name' may already exist or quota exceeded."
  fi

  # DigitalOcean droplet IPs are already static (don't change on reboot)
  local ip
  ip=$(doctl compute droplet get "$droplet_id" --format PublicIPv4 --no-header)

  SERVER_NAME="$name"
  show_qr "$ip" "$ssh_user"
}

do_destroy() {
  local instances target did
  instances=$(doctl compute droplet list --format "ID,Name,PublicIPv4,Status" --no-header || true)
  [[ -z "$instances" ]] && die "No droplets found."

  if [[ -n "${FLAGS[name]:-}" ]]; then
    target=$(echo "$instances" | awk -v name="${FLAGS[name]}" '$2 == name' | head -1)
    [[ -z "$target" ]] && die "No droplet named '${FLAGS[name]}' found."
  else
    target=$(echo "$instances" | gum choose --header "Select droplet to destroy")
  fi
  did=$(echo "$target" | awk '{print $1}')
  yesno "Destroy droplet $did? This is irreversible." || exit 0
  gum spin --title "Destroying droplet '$did'..." -- doctl compute droplet delete "$did" --force
  gum style --foreground 2 --bold "Destroyed droplet '$did'."
  gum style --foreground 3 "Remember to remove the WireGuard tunnel from your phone/device."
}

do_status() {
  doctl compute droplet list --format "ID,Name,PublicIPv4,Region,Size,Status"
}

# ── Provider: Hetzner ─────────────────────────────────────────────────────────

hetzner_create() {
  local location stype ssh_key name ssh_user="root"
  pick location "location" gum choose --header "Location" nbg1 fsn1 hel1 ash
  pick stype    "type"     gum choose --header "Server type" cx22 cx32 cx42

  # SSH key from provider API (optional)
  local available_keys ssh_key=""
  if [[ -n "${FLAGS[key]:-}" ]]; then
    ssh_key="${FLAGS[key]}"
  else
    available_keys=$(hcloud ssh-key list -o noheader -o columns=name || true)
    if [[ -n "$available_keys" ]]; then
      ssh_key=$(echo "$available_keys" "$(gum style --faint '(skip - use generated key only)')" | gum choose --header "SSH key (optional)")
      [[ "$ssh_key" == *"skip"* ]] && ssh_key=""
    fi
  fi

  pick name "name" gum input --header "Server name" --value "dev-box"

  local key_display="${ssh_key:-generated (via cloud-init)}"
  gum style --bold "Summary"
  styled_box "Provider: Hetzner" "Location: $location" "Type:     $stype" "SSH key:  $key_display" "Name:     $name"
  gum style --foreground 3 "Warning: Hetzner creates servers with no provider-level firewall."
  gum style --foreground 3 "All ports are open until cloud-init configures ufw (1-2 minutes)."
  gum style --foreground 3 "Consider adding a Hetzner firewall after creation for defense-in-depth."
  yesno "Create this server?" || exit 0

  # Set up WireGuard keys and build modified cloud-init
  trap 'cleanup_wireguard' EXIT
  setup_wireguard
  local wg_cloud_init
  wg_cloud_init=$(build_cloud_init "$ssh_user")

  local key_flag=()
  [[ -n "$ssh_key" ]] && key_flag=(--ssh-key "$ssh_key")

  local result
  if ! result=$(gum spin --title "Creating server '$name'..." --show-output -- \
    hcloud server create --name "$name" \
      --type "$stype" --image ubuntu-24.04 \
      --location "$location" "${key_flag[@]}" \
      --user-data-from-file "$wg_cloud_init" 2>&1); then
    die "Failed to create server. Server '$name' may already exist or quota exceeded."
  fi

  # Hetzner cloud server IPs are already static
  local ip
  ip=$(hcloud server ip "$name")

  SERVER_NAME="$name"
  show_qr "$ip" "$ssh_user"
}

hetzner_destroy() {
  local instances target
  instances=$(hcloud server list -o noheader -o columns=name 2>/dev/null || true)
  [[ -z "$instances" ]] && die "No Hetzner servers found."

  if [[ -n "${FLAGS[name]:-}" ]]; then
    target="${FLAGS[name]}"
  else
    target=$(echo "$instances" | gum choose --header "Select server to destroy")
  fi
  yesno "Destroy Hetzner server '$target'? This is irreversible." || exit 0
  gum spin --title "Destroying '$target'..." -- hcloud server delete "$target"
  gum style --foreground 2 --bold "Destroyed '$target'."
  gum style --foreground 3 "Remember to remove the WireGuard tunnel from your phone/device."
}

hetzner_status() {
  hcloud server list
}

# ── Provider: This machine (local) ───────────────────────────────────────────
#
# Runs setup directly on the current machine — no SSH, no cloud CLI.
# Supports macOS (Homebrew + LaunchAgents) and Linux (apt/dnf/pacman + systemd).
# Windows: prints WSL2 instructions and exits.

local_create() {
  local os arch
  os=$(uname -s)
  arch=$(uname -m)

  # Windows guard
  case "$os" in
    MINGW*|CYGWIN*|MSYS*)
      die "Windows is not supported directly.
Run this script inside WSL2 (Ubuntu) and try again:
  wsl --install
  wsl
  ./deploy.sh create local

Or use a VPS with: ./deploy.sh create any"
      ;;
  esac

  [[ "$os" != "Darwin" && "$os" != "Linux" ]] && die "Unsupported OS: $os"

  # macOS: require Homebrew
  if [[ "$os" == "Darwin" ]]; then
    if ! command -v brew &>/dev/null; then
      gum style --foreground 1 --bold "Homebrew is required but not installed."
      gum style "Install it from https://brew.sh then re-run this script."
      exit 1
    fi
    require_cli wg "WireGuard tools (brew install wireguard-tools)"
    require_cli qrencode qrencode
  else
    require_cli wg "WireGuard tools"
    require_cli qrencode qrencode
  fi

  local name
  pick name "name" gum input --header "Server name (display only)" --value "my-machine"

  gum style --bold "Summary"
  styled_box \
    "Provider: This machine ($os $arch)" \
    "Name:     $name"
  yesno "Set up this machine?" || exit 0

  trap 'cleanup_wireguard' EXIT
  setup_wireguard
  SERVER_NAME="$name"

  # Detect IPs and let user pick
  local server_ip
  server_ip=$(pick_server_ip)
  [[ -z "$server_ip" ]] && die "No IP address selected."

  # ── macOS setup ──────────────────────────────────────────────────────────
  if [[ "$os" == "Darwin" ]]; then
    local brew_prefix
    brew_prefix=$(get_brew_prefix)

    gum spin --title "Installing tmux via Homebrew..." -- \
      brew install tmux

    # continuum-relay binary
    local relay_bin="continuum-relay-darwin-arm64"
    [[ "$arch" == "x86_64" ]] && relay_bin="continuum-relay-darwin-amd64"
    # Download to a temp file and verify BEFORE touching the live binary, so a
    # corrupt/tampered download (or a checksum mismatch) can never overwrite the
    # running binary — a Restart=always service would otherwise respawn on it.
    local relay_tmp
    relay_tmp="$(mktemp)"
    gum spin --title "Downloading continuum-relay ($relay_bin)..." -- \
      curl -fsSL -o "$relay_tmp" \
        "https://github.com/kalleeh/continuum-relay/releases/latest/download/${relay_bin}"
    gum spin --title "Verifying checksum..." -- bash -c "
      expected=\$(curl -sfL 'https://github.com/kalleeh/continuum-relay/releases/latest/download/checksums.txt' | grep '${relay_bin}' | awk '{print \$1}')
      if [ -z \"\$expected\" ]; then
        echo 'ERROR: Could not fetch checksums.txt from release — aborting for safety' >&2
        rm -f '$relay_tmp' 2>/dev/null || true
        exit 1
      fi
      actual=\"\"
      if command -v sha256sum &>/dev/null; then
        actual=\$(sha256sum '$relay_tmp' 2>/dev/null | awk '{print \$1}')
      elif command -v shasum &>/dev/null; then
        actual=\$(shasum -a 256 '$relay_tmp' 2>/dev/null | awk '{print \$1}')
      fi
      if [ -z \"\$actual\" ]; then
        echo 'ERROR: Failed to compute checksum' >&2
        rm -f '$relay_tmp' 2>/dev/null || true
        exit 1
      fi
      if [ \"\$expected\" != \"\$actual\" ]; then
        echo 'ERROR: Checksum mismatch — binary may be corrupted or tampered' >&2
        rm -f '$relay_tmp' 2>/dev/null || true
        exit 1
      fi
    "
    install -m 0755 "$relay_tmp" /usr/local/bin/continuum-relay
    rm -f "$relay_tmp"

    # WireGuard config
    local wg_dir="${brew_prefix}/etc/wireguard"
    sudo mkdir -p "$wg_dir"

    local peers_block=""
    for i in $(seq 1 "$WG_PEER_COUNT"); do
      local idx=$((i - 1))
      local peer_ip="10.100.0.$((i + 1))"
      peers_block+="
[Peer]  # Device ${i}
PublicKey = ${WG_PHONE_PUBKEYS[$idx]}
AllowedIPs = ${peer_ip}/32
"
    done

    sudo tee "${wg_dir}/wg0.conf" > /dev/null << WGCONF
[Interface]
PrivateKey = ${WG_SERVER_PRIVKEY}
Address = 10.100.0.1/24
ListenPort = 51820
${peers_block}
WGCONF
    sudo chmod 600 "${wg_dir}/wg0.conf"

    if [[ -n "$MOBILE_APP" ]]; then
      sudo mkdir -p /etc/continuum /var/log/continuum
      printf 'CONTINUUM_TOKEN=%s\nCONTINUUM_RELAY_ADDR=10.100.0.1:7682\nCONTINUUM_RELAY_LOG=/var/log/continuum/relay.log\nOLLAMA_HOST=http://10.100.0.1:11434\n' \
        "$CONTINUUM_TOKEN" | sudo tee /etc/continuum/env > /dev/null
      [[ -n "$OLLAMA_API_KEY" ]] && echo "OLLAMA_API_KEY=$OLLAMA_API_KEY" | sudo tee -a /etc/continuum/env > /dev/null
      sudo chmod 600 /etc/continuum/env

      local agents_dir="$HOME/Library/LaunchAgents"
      mkdir -p "$agents_dir"

      # continuum-relay LaunchAgent
      cat > "${agents_dir}/com.continuum.relay.plist" << PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.continuum.relay</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/continuum-relay</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>CONTINUUM_TOKEN</key><string>${CONTINUUM_TOKEN}</string>
    <key>CONTINUUM_RELAY_ADDR</key><string>10.100.0.1:7682</string>
    <key>CONTINUUM_RELAY_LOG</key><string>/var/log/continuum/relay.log</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/var/log/continuum/relay.log</string>
  <key>StandardOutPath</key><string>/var/log/continuum/relay.log</string>
</dict></plist>
PLIST
      chmod 600 "${agents_dir}/com.continuum.relay.plist"

      launchctl load "${agents_dir}/com.continuum.relay.plist"
      gum style --foreground 2 "✓ Continuum services loaded as LaunchAgents"
    fi

  # ── Linux setup ──────────────────────────────────────────────────────────
  else
    # Detect package manager
    local pm=""
    if command -v apt-get &>/dev/null; then
      pm="apt"
    elif command -v dnf &>/dev/null; then
      pm="dnf"
    elif command -v pacman &>/dev/null; then
      pm="pacman"
    else
      die "No supported package manager found (apt/dnf/pacman). Install WireGuard and tmux manually then re-run."
    fi

    gum spin --title "Installing dependencies via $pm..." -- bash -c "
      case '$pm' in
        apt)    sudo apt-get update -qq && sudo apt-get install -y -qq ufw curl tmux ;;
        dnf)    sudo dnf install -y curl tmux ;;
        pacman) sudo pacman -Sy --noconfirm curl tmux ;;
      esac
    "

    # continuum-relay binary
    local relay_arch="amd64"
    [[ "$arch" == "aarch64" || "$arch" == "arm64" ]] && relay_arch="arm64"
    # Download to a temp file and verify BEFORE installing over the live binary,
    # so a corrupt/tampered download or checksum mismatch never reaches the path
    # a Restart=always service would respawn from.
    local relay_tmp
    relay_tmp="$(mktemp)"
    gum spin --title "Downloading continuum-relay..." -- \
      curl -fsSL -o "$relay_tmp" \
        "https://github.com/kalleeh/continuum-relay/releases/latest/download/continuum-relay-linux-${relay_arch}"
    gum spin --title "Verifying checksum..." -- bash -c "
      expected=\$(curl -sfL 'https://github.com/kalleeh/continuum-relay/releases/latest/download/checksums.txt' | grep 'continuum-relay-linux-${relay_arch}' | awk '{print \$1}')
      if [ -z \"\$expected\" ]; then
        echo 'ERROR: Could not fetch checksums.txt from release — aborting for safety' >&2
        rm -f '$relay_tmp' 2>/dev/null || true
        exit 1
      fi
      actual=\"\"
      if command -v sha256sum &>/dev/null; then
        actual=\$(sha256sum '$relay_tmp' 2>/dev/null | awk '{print \$1}')
      elif command -v shasum &>/dev/null; then
        actual=\$(shasum -a 256 '$relay_tmp' 2>/dev/null | awk '{print \$1}')
      fi
      if [ -z \"\$actual\" ]; then
        echo 'ERROR: Failed to compute checksum' >&2
        rm -f '$relay_tmp' 2>/dev/null || true
        exit 1
      fi
      if [ \"\$expected\" != \"\$actual\" ]; then
        echo 'ERROR: Checksum mismatch — binary may be corrupted or tampered' >&2
        rm -f '$relay_tmp' 2>/dev/null || true
        exit 1
      fi
    "
    sudo install -m 0755 "$relay_tmp" /usr/local/bin/continuum-relay
    rm -f "$relay_tmp"

    # WireGuard config
    sudo mkdir -p /etc/wireguard

    local peers_block=""
    for i in $(seq 1 "$WG_PEER_COUNT"); do
      local idx=$((i - 1))
      local peer_ip="10.100.0.$((i + 1))"
      peers_block+="
[Peer]  # Device ${i}
PublicKey = ${WG_PHONE_PUBKEYS[$idx]}
AllowedIPs = ${peer_ip}/32
"
    done

    sudo tee /etc/wireguard/wg0.conf > /dev/null << WGCONF
[Interface]
PrivateKey = ${WG_SERVER_PRIVKEY}
Address = 10.100.0.1/24
ListenPort = 51820
${peers_block}
WGCONF
    sudo chmod 600 /etc/wireguard/wg0.conf
    # Owned by the relay's runtime user so it can read at startup and
    # append/remove peers via `continuum-relay peers add|remove`.
    sudo chown "${USER}:${USER}" /etc/wireguard/wg0.conf

    # Firewall (apt/ufw only)
    if [[ "$pm" == "apt" ]] && command -v ufw &>/dev/null; then
      sudo ufw default deny incoming
      sudo ufw allow 51820/udp
      sudo ufw allow 22/tcp
      sudo ufw --force enable
    fi

    if [[ -n "$MOBILE_APP" ]]; then
      sudo mkdir -p /etc/continuum /var/log/continuum
      printf 'CONTINUUM_TOKEN=%s\nCONTINUUM_RELAY_ADDR=10.100.0.1:7682\nCONTINUUM_RELAY_LOG=/var/log/continuum/relay.log\nOLLAMA_HOST=http://10.100.0.1:11434\n' \
        "$CONTINUUM_TOKEN" | sudo tee /etc/continuum/env > /dev/null
      [[ -n "$OLLAMA_API_KEY" ]] && echo "OLLAMA_API_KEY=$OLLAMA_API_KEY" | sudo tee -a /etc/continuum/env > /dev/null
      sudo chmod 600 /etc/continuum/env

      # Install Ollama and bind to WireGuard interface
      curl -fsSL https://ollama.com/install.sh | sh
      sudo mkdir -p /etc/systemd/system/ollama.service.d
      printf '[Service]\nEnvironment=OLLAMA_HOST=10.100.0.1:11434\n' | sudo tee /etc/systemd/system/ollama.service.d/override.conf > /dev/null
      sudo systemctl daemon-reload && sudo systemctl restart ollama

      sudo tee /etc/systemd/system/continuum-relay.service > /dev/null << SVC
[Unit]
Description=Continuum Relay
After=network.target
[Service]
Type=simple
User=${USER}
EnvironmentFile=/etc/continuum/env
ExecStart=/usr/local/bin/continuum-relay
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
# /etc/wireguard must be writable: the relay updates wg0.conf when peers
# are added or removed via \`continuum-relay peers add|remove\`.
ReadWritePaths=/var/log/continuum /etc/wireguard
ReadOnlyPaths=/etc/continuum
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
StandardOutput=append:/var/log/continuum/relay.log
StandardError=append:/var/log/continuum/relay.log
[Install]
WantedBy=multi-user.target
SVC

      # copytruncate: the relay logs via systemd StandardOutput=append:, which
      # holds the file open; a HUP won't make systemd reopen it, so a rename-based
      # rotation would leave the relay writing to a deleted inode. Truncating in
      # place keeps the existing fd valid.
      sudo tee /etc/logrotate.d/continuum > /dev/null << 'LOGROTATE'
/var/log/continuum/*.log {
    daily
    rotate 7
    compress
    missingok
    notifempty
    copytruncate
}
LOGROTATE

      sudo systemctl daemon-reload
      sudo systemctl enable --now continuum-relay
      # PTY sessions spawn into the user manager's cgroup (systemd-run --user
      # --scope) to survive relay restarts; that needs linger so the manager
      # runs without an active login.
      sudo loginctl enable-linger "${USER}" 2>/dev/null || true

      if command -v ufw &>/dev/null; then
        sudo ufw allow from 10.100.0.0/24 to any port 7682 proto tcp comment "Continuum relay"
        sudo ufw allow from 10.100.0.0/24 to any port 11434 proto tcp comment "Ollama"
      fi
    fi
  fi

  show_qr "$server_ip" "$USER"

  echo ""
  gum style --foreground 3 "WireGuard is managed by continuum-relay, which starts automatically on boot."
  if [[ "$os" == "Darwin" ]]; then
    gum style --foreground 3 "  launchctl start com.continuum.relay  # to restart manually"
  else
    gum style --foreground 3 "  sudo systemctl restart continuum-relay  # to restart manually"
  fi
}

local_destroy() {
  local os
  os=$(uname -s)

  yesno "Remove WireGuard and Continuum services from this machine?" || exit 0

  if [[ "$os" == "Darwin" ]]; then
    local agents_dir="$HOME/Library/LaunchAgents"
    launchctl unload "${agents_dir}/com.continuum.ttyd.plist"  2>/dev/null || true
    launchctl unload "${agents_dir}/com.continuum.relay.plist" 2>/dev/null || true
    rm -f "${agents_dir}/com.continuum.ttyd.plist" "${agents_dir}/com.continuum.relay.plist"
    sudo wg-quick down "$(get_brew_prefix)/etc/wireguard/wg0.conf" 2>/dev/null || true
    sudo rm -f "$(get_brew_prefix)/etc/wireguard/wg0.conf"
    sudo rm -rf /etc/continuum
    sudo rm -f /usr/local/bin/continuum-relay
  else
    sudo systemctl stop ttyd continuum-relay wg-quick@wg0 2>/dev/null || true
    sudo systemctl disable ttyd continuum-relay wg-quick@wg0 2>/dev/null || true
    sudo rm -f /etc/systemd/system/ttyd.service /etc/systemd/system/continuum-relay.service
    sudo rm -f /etc/wireguard/wg0.conf
    sudo rm -f /usr/local/bin/ttyd /usr/local/bin/continuum-relay
    sudo rm -rf /etc/continuum
    sudo systemctl daemon-reload
  fi

  gum style --foreground 2 --bold "Continuum services removed."
  gum style --foreground 3 "Remember to remove the WireGuard tunnel from your phone/device."
}

local_status() {
  local os
  os=$(uname -s)

  echo "=== WireGuard ==="
  sudo wg show 2>/dev/null || echo "not running"
  echo ""
  echo "=== Continuum services ==="
  if [[ "$os" == "Darwin" ]]; then
    launchctl list 2>/dev/null | grep continuum || echo "no services loaded"
  else
    for svc in ttyd continuum-relay; do
      printf "  %-20s %s\n" "$svc" "$(systemctl is-active $svc 2>/dev/null || echo 'not installed')"
    done
  fi
}

# ── Provider: Any existing server (SSH) ──────────────────────────────────────
#
# Works with any Ubuntu/Debian Linux server you can SSH into:
# local VM, VPS, home server, bare metal, cloud instance without a CLI tool.
# Generates WireGuard keys locally, builds a self-contained setup script,
# then runs it on the remote via SSH.

generic_create() {
  local ssh_target ssh_user server_ip name

  pick ssh_target "host" gum input \
    --header "SSH connection" \
    --placeholder "ubuntu@1.2.3.4  or  root@myserver.com"

  if [[ "$ssh_target" == *@* ]]; then
    ssh_user="${ssh_target%%@*}"
    server_ip="${ssh_target##*@}"
  else
    ssh_user="root"
    server_ip="$ssh_target"
  fi

  local -a ssh_opts=("-o" "StrictHostKeyChecking=accept-new" "-o" "ConnectTimeout=15")
  [[ -n "${FLAGS[ssh-key]:-}" ]] && ssh_opts+=("-i" "${FLAGS[ssh-key]}")

  pick name "name" gum input --header "Server name (display only)" --value "my-server"

  gum style --bold "Summary"
  styled_box \
    "Provider: Any server (SSH)" \
    "SSH:      $ssh_target" \
    "Name:     $name"
  yesno "Set up this server?" || exit 0

  trap 'cleanup_wireguard' EXIT
  setup_wireguard
  SERVER_NAME="$name"

  # Build peers block for WireGuard config
  local peers_block=""
  for i in $(seq 1 "$WG_PEER_COUNT"); do
    local idx=$((i - 1))
    local peer_ip="10.100.0.$((i + 1))"
    peers_block+="
[Peer]  # Device ${i}
PublicKey = ${WG_PHONE_PUBKEYS[$idx]}
AllowedIPs = ${peer_ip}/32
"
  done

  # Write WireGuard config template locally (variables substituted here)
  cat > "$WG_TMPDIR/wg0.conf" << WGCONF
[Interface]
PrivateKey = ${WG_SERVER_PRIVKEY}
Address = 10.100.0.1/24
ListenPort = 51820
${peers_block}
WGCONF

  # Encode as base64 for safe inline embedding in the remote script
  local wg_b64
  wg_b64=$(base64 < "$WG_TMPDIR/wg0.conf" | tr -d '\n')

  # Build the remote setup script — header (no local expansion)
  cat > "$WG_TMPDIR/setup.sh" << 'SCRIPT_HEADER'
#!/usr/bin/env bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

echo "==> Installing dependencies..."
apt-get update -qq
apt-get install -y -qq ufw curl tmux
SCRIPT_HEADER

  # WireGuard config + firewall (inject base64 blob, local vars expanded here)
  cat >> "$WG_TMPDIR/setup.sh" << SCRIPT_WG

echo "==> Configuring WireGuard..."
mkdir -p /etc/wireguard
echo "${wg_b64}" | base64 -d > /etc/wireguard/wg0.conf
chmod 600 /etc/wireguard/wg0.conf

echo "==> Configuring firewall..."
ufw default deny incoming
ufw allow 51820/udp
ufw allow 22/tcp
ufw --force enable
SCRIPT_WG

  # Mobile-app services (continuum-relay)
  if [[ -n "$MOBILE_APP" ]]; then
    # Write relay service file template locally so we can substitute ssh_user
    cat > "$WG_TMPDIR/relay.service" << RELAY_SVC
[Unit]
Description=Continuum Relay
After=network.target

[Service]
Type=simple
User=${ssh_user}
EnvironmentFile=/etc/continuum/env
ExecStart=/usr/local/bin/continuum-relay
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
# /etc/wireguard must be writable: the relay updates wg0.conf when peers are
# added/removed via \`continuum-relay peers add|remove\`.
ReadWritePaths=/var/log/continuum /etc/wireguard
ReadOnlyPaths=/etc/continuum
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
StandardOutput=append:/var/log/continuum/relay.log
StandardError=append:/var/log/continuum/relay.log

[Install]
WantedBy=multi-user.target
RELAY_SVC

    local relay_b64
    relay_b64=$(base64 < "$WG_TMPDIR/relay.service" | tr -d '\n')

    cat >> "$WG_TMPDIR/setup.sh" << SCRIPT_MOBILE

echo "==> Installing Continuum services..."
mkdir -p /etc/continuum /var/log/continuum

# Pre-seed /etc/continuum/env so install.sh detects an existing install
# and runs only its binary download + checksum verification path.
printf 'CONTINUUM_TOKEN=${CONTINUUM_TOKEN}\nCONTINUUM_RELAY_ADDR=10.100.0.1:7682\nCONTINUUM_RELAY_LOG=/var/log/continuum/relay.log\nOLLAMA_HOST=http://10.100.0.1:11434\n' \
  > /etc/continuum/env
[[ -n "${OLLAMA_API_KEY:-}" ]] && echo "OLLAMA_API_KEY=${OLLAMA_API_KEY}" >> /etc/continuum/env
chmod 600 /etc/continuum/env
chown ${ssh_user}:${ssh_user} /etc/continuum/env 2>/dev/null || true

# Pre-seed systemd unit so install.sh can restart it after install.
echo "${relay_b64}" | base64 -d > /etc/systemd/system/continuum-relay.service
systemctl daemon-reload
systemctl enable continuum-relay

# Logrotate
cat > /etc/logrotate.d/continuum << 'LR'
/var/log/continuum/*.log {
    daily
    rotate 7
    compress
    missingok
    notifempty
    # copytruncate: systemd holds the append: log fd open and won't reopen on
    # HUP, so truncate in place rather than rename (which orphans the relay's fd).
    copytruncate
}
LR

echo "  Downloading and installing continuum-relay (via install.sh)..."
curl -fsSL https://raw.githubusercontent.com/kalleeh/continuum-relay/main/install.sh | sh

ufw allow from 10.100.0.0/24 to any port 7682 proto tcp comment "Continuum relay (WireGuard only)"
SCRIPT_MOBILE
  fi

  cat >> "$WG_TMPDIR/setup.sh" << 'SCRIPT_FOOTER'

echo ""
echo "✓ Setup complete! WireGuard is running on this server."
SCRIPT_FOOTER

  if ! gum spin --title "Running setup on $server_ip (may take 1-2 minutes)..." -- \
    ssh "${ssh_opts[@]}" "$ssh_target" sudo bash -s < "$WG_TMPDIR/setup.sh"; then
    die "Remote setup failed on $server_ip — check server logs for details"
  fi

  show_qr "$server_ip" "$ssh_user"
}

generic_destroy() {
  local ssh_target
  pick ssh_target "host" gum input \
    --header "SSH connection to clean up" \
    --placeholder "ubuntu@1.2.3.4"

  local -a ssh_opts=("-o" "StrictHostKeyChecking=accept-new")
  [[ -n "${FLAGS[ssh-key]:-}" ]] && ssh_opts+=("-i" "${FLAGS[ssh-key]}")

  yesno "Remove WireGuard and Continuum services from $ssh_target?" || exit 0

  ssh "${ssh_opts[@]}" "$ssh_target" sudo bash << 'CLEANUP'
systemctl stop wg-quick@wg0 ttyd continuum-relay 2>/dev/null || true
systemctl disable wg-quick@wg0 ttyd continuum-relay 2>/dev/null || true
rm -f /etc/wireguard/wg0.conf
rm -f /etc/systemd/system/ttyd.service /etc/systemd/system/continuum-relay.service
rm -f /usr/local/bin/ttyd /usr/local/bin/continuum-relay
rm -rf /etc/continuum
systemctl daemon-reload
echo "Done."
CLEANUP

  gum style --foreground 2 --bold "Cleaned up $ssh_target."
  gum style --foreground 3 "Remember to remove the WireGuard tunnel from your phone/device."
}

generic_status() {
  local ssh_target
  pick ssh_target "host" gum input \
    --header "SSH connection" \
    --placeholder "ubuntu@1.2.3.4"

  local -a ssh_opts=("-o" "StrictHostKeyChecking=accept-new")
  [[ -n "${FLAGS[ssh-key]:-}" ]] && ssh_opts+=("-i" "${FLAGS[ssh-key]}")

  ssh "${ssh_opts[@]}" "$ssh_target" bash << 'STATUS'
echo "=== WireGuard ==="
systemctl is-active wg-quick@wg0 2>/dev/null && wg show 2>/dev/null || echo "not running"
echo ""
echo "=== Continuum services ==="
for svc in ttyd continuum-relay; do
  printf "  %-20s %s\n" "$svc" "$(systemctl is-active $svc 2>/dev/null || echo 'not installed')"
done
STATUS
}

# ── Dispatch ──────────────────────────────────────────────────────────────────

choose_provider() {
  # Provider can be specified as 2nd positional arg (shorthand accepted)
  local arg=""
  [[ ${#POSITIONAL_ARGS[@]} -gt 1 ]] && arg="${POSITIONAL_ARGS[1]}"
  case "$arg" in
    local|this)              echo "This machine (local)"; return ;;
    any|ssh|generic)         echo "Any server (SSH)"; return ;;
    lightsail)               echo "AWS Lightsail"; return ;;
    ec2)                     echo "AWS EC2"; return ;;
    do|digitalocean)         echo "DigitalOcean"; return ;;
    hetzner)                 echo "Hetzner"; return ;;
    lima)                    echo "Lima (local VM)"; return ;;
  esac
  gum choose --header "Select provider" \
    "This machine (local)" "Any server (SSH)" "AWS Lightsail" "AWS EC2" "DigitalOcean" "Hetzner" "Lima (local VM)"
}

cmd_create() {
  local provider
  provider=$(choose_provider)

  # WireGuard tools required for all providers except Lima
  if [[ "$provider" != "Lima (local)" ]]; then
    require_cli wg "WireGuard tools"
    require_cli qrencode qrencode
  fi

  case "$provider" in
    "This machine (local)") local_create ;;
    "Any server (SSH)")     generic_create ;;
    "Lima (local VM)")      require_cli limactl Lima;        lima_create ;;
    "AWS Lightsail")        require_cli aws "AWS CLI";       lightsail_create ;;
    "AWS EC2")              require_cli aws "AWS CLI";       ec2_create ;;
    "DigitalOcean")         require_cli doctl DigitalOcean;  do_create ;;
    "Hetzner")              require_cli hcloud Hetzner;      hetzner_create ;;
  esac
}

cmd_destroy() {
  local provider
  provider=$(choose_provider)

  case "$provider" in
    "This machine (local)") local_destroy ;;
    "Any server (SSH)")     generic_destroy ;;
    "Lima (local VM)")      require_cli limactl Lima;        lima_destroy ;;
    "AWS Lightsail")        require_cli aws "AWS CLI";       lightsail_destroy ;;
    "AWS EC2")              require_cli aws "AWS CLI";       ec2_destroy ;;
    "DigitalOcean")         require_cli doctl DigitalOcean;  do_destroy ;;
    "Hetzner")              require_cli hcloud Hetzner;      hetzner_destroy ;;
  esac
}

cmd_status() {
  local provider
  provider=$(choose_provider)

  case "$provider" in
    "This machine (local)") local_status ;;
    "Any server (SSH)")     generic_status ;;
    "Lima (local VM)")      require_cli limactl Lima;        lima_status ;;
    "AWS Lightsail")        require_cli aws "AWS CLI";       lightsail_status ;;
    "AWS EC2")              require_cli aws "AWS CLI";       ec2_status ;;
    "DigitalOcean")         require_cli doctl DigitalOcean;  do_status ;;
    "Hetzner")              require_cli hcloud Hetzner;      hetzner_status ;;
  esac
}

cmd_help() {
  echo "Usage: $0 {create|destroy|status} [provider] [flags]"
  echo ""
  echo "Providers:"
  echo "  local        This machine — macOS or Linux laptop/desktop (no SSH or cloud CLI needed)"
  echo "  any          Any Ubuntu/Debian server you can SSH into (VPS, home lab, cloud VM)"
  echo "  lightsail    AWS Lightsail  (requires: aws CLI)"
  echo "  ec2          AWS EC2        (requires: aws CLI)"
  echo "  do           DigitalOcean   (requires: doctl)"
  echo "  hetzner      Hetzner Cloud  (requires: hcloud)"
  echo "  lima         Local VM on macOS via Lima (requires: limactl)"
  echo ""
  echo "Flags (all providers):"
  echo "  --ssh-key PATH        SSH private key path"
  echo "  --name NAME           Server display name"
  echo "  --peers N             WireGuard device count (default: 1)"
  echo "  --bedrock-region R    Enable Bedrock with this region"
  echo "  --bedrock-key KEY     Bedrock API key (optional; uses IAM if omitted)"
  echo "  --yes, -y, --force    Skip confirmation prompts"
  echo "  --yes                Skip all confirmation prompts"
  echo ""
  echo "Flags (cloud providers):"
  echo "  --region REGION       Cloud region (e.g. eu-north-1)"
  echo "  --bundle BUNDLE       Lightsail bundle (e.g. small_3_0)"
  echo "  --type TYPE           EC2/Hetzner instance type (e.g. t3.small)"
  echo "  --size SIZE           DigitalOcean droplet size (e.g. s-1vcpu-2gb)"
  echo "  --location LOC        Hetzner location (e.g. nbg1)"
  echo "  --key KEYPAIR         Provider SSH key pair name"
  echo "  --cpus N              Lima CPUs"
  echo "  --memory MEM          Lima memory (e.g. 4GiB)"
  echo ""
  echo "Examples:"
  echo "  # Run on this machine (macOS or Linux):"
  echo "  $0 create local"
  echo ""
  echo "  # Any server you already have SSH access to:"
  echo "  $0 create any --ssh-key ~/.ssh/id_ed25519"
  echo ""
  echo "  # Spin up a new cloud VM:"
  echo "  $0 create lightsail --region eu-north-1 --bundle small_3_0 --yes"
  echo "  $0 create ec2       --region eu-north-1 --type t3.small    --yes"
  echo "  $0 create do        --region fra1        --size s-1vcpu-2gb --yes"
  echo "  $0 create hetzner   --location nbg1      --type cx22        --yes"
}

# ── Main ──────────────────────────────────────────────────────────────────────

require_gum
parse_args "$@"
load_env

ACTION="${POSITIONAL_ARGS[0]:-create}"
case "$ACTION" in
  create)       cmd_create ;;
  destroy)      cmd_destroy ;;
  status)       cmd_status ;;
  help|--help)  cmd_help ;;
  *)
    gum style --bold "Usage: $0 {create|destroy|status} [provider] [flags]"
    gum style "Run '$0 help' for full usage."
    exit 1
    ;;
esac
