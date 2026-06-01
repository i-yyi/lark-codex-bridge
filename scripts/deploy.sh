#!/usr/bin/env bash
set -euo pipefail

APP_NAME="lark-bridge"
GO_VERSION="${GO_VERSION:-1.22.12}"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
STATE_HOME="${XDG_STATE_HOME:-$HOME/.local/state}"
CONFIG_DIR="$CONFIG_HOME/$APP_NAME"
STATE_DIR="$STATE_HOME/$APP_NAME"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
NPM_PREFIX="${NPM_CONFIG_PREFIX:-$HOME/.local/npm_packages}"
LOCAL_GO_DIR="${LOCAL_GO_DIR:-$HOME/.local/go}"
CONFIG_FILE="${CONFIG_FILE:-$CONFIG_DIR/config.json}"
ENV_FILE="$CONFIG_DIR/env"
SERVICE_DIR="$CONFIG_HOME/systemd/user"
SERVICE_FILE="$SERVICE_DIR/$APP_NAME.service"
HEALTH_SERVICE_FILE="$SERVICE_DIR/$APP_NAME-health.service"
HEALTH_TIMER_FILE="$SERVICE_DIR/$APP_NAME-health.timer"
CHECK_ONLY=false
USE_LOCAL_CONFIG=false
PROBE_LARK=true

usage() {
  cat <<EOF
Usage: ./scripts/deploy.sh [--check] [--use-local-config] [--no-probe]

  --check             Run preflight checks only. Do not install, login, write config, or write services.
  --use-local-config  If target config is missing, copy ./config.json from this repo.
  --no-probe          Skip active Feishu permission probe during deploy.
EOF
}

parse_args() {
  for arg in "$@"; do
    case "$arg" in
      --check)
        CHECK_ONLY=true
        ;;
      --use-local-config)
        USE_LOCAL_CONFIG=true
        ;;
      --no-probe)
        PROBE_LARK=false
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      *)
        die "unknown argument: $arg"
        ;;
    esac
  done
}

log() {
  printf '[%s] %s\n' "$APP_NAME" "$*"
}

warn() {
  printf '[%s] WARN: %s\n' "$APP_NAME" "$*" >&2
}

die() {
  printf '[%s] ERROR: %s\n' "$APP_NAME" "$*" >&2
  exit 1
}

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

prepend_path() {
  case ":$PATH:" in
    *":$1:"*) ;;
    *) export PATH="$1:$PATH" ;;
  esac
}

bootstrap_path() {
  prepend_path "$INSTALL_DIR"
  prepend_path "$NPM_PREFIX/bin"
  prepend_path "$LOCAL_GO_DIR/bin"
  prepend_path "/usr/local/go/bin"
}

ensure_dirs() {
  mkdir -p "$CONFIG_DIR" "$STATE_DIR" "$INSTALL_DIR"
}

ensure_npm() {
  if has_cmd npm && has_cmd npx; then
    log "npm found: $(npm --version)"
    return
  fi

  if has_cmd apt-get && has_cmd sudo; then
    log "npm not found, installing nodejs/npm with apt-get"
    sudo apt-get update
    sudo apt-get install -y nodejs npm
  elif has_cmd dnf && has_cmd sudo; then
    log "npm not found, installing nodejs/npm with dnf"
    sudo dnf install -y nodejs npm
  elif has_cmd yum && has_cmd sudo; then
    log "npm not found, installing nodejs/npm with yum"
    sudo yum install -y nodejs npm
  elif has_cmd brew; then
    log "npm not found, installing node with brew"
    brew install node
  else
    die "npm/npx not found. Install Node.js/npm first, then rerun this script."
  fi

  has_cmd npm && has_cmd npx || die "npm/npx installation did not expose commands in PATH"
}

ensure_lark_cli() {
  if has_cmd lark-cli; then
    log "lark-cli found: $(lark-cli --version)"
    return
  fi

  log "lark-cli not found, installing @larksuite/cli into $NPM_PREFIX"
  mkdir -p "$NPM_PREFIX"
  npm config set prefix "$NPM_PREFIX" --location=user
  npm install -g @larksuite/cli
  prepend_path "$NPM_PREFIX/bin"
  has_cmd lark-cli || die "lark-cli installation completed but command is still not in PATH"
}

ensure_lark_skills() {
  if [[ -f "$HOME/.agents/skills/lark-shared/SKILL.md" && -f "$HOME/.agents/skills/lark-im/SKILL.md" ]]; then
    log "lark skills found"
    return
  fi

  log "lark skills not found, installing larksuite/cli skills"
  if ! npx --yes skills add larksuite/cli -g -y; then
    warn "failed to install lark skills; runtime daemon can still run, but agent-side help will be weaker"
  fi
}

ensure_codex() {
  if has_cmd codex; then
    log "codex found: $(codex --version 2>/dev/null | tail -n 1)"
  else
    log "codex not found, installing @openai/codex into $NPM_PREFIX"
    mkdir -p "$NPM_PREFIX"
    npm config set prefix "$NPM_PREFIX" --location=user
    npm install -g @openai/codex
    prepend_path "$NPM_PREFIX/bin"
    has_cmd codex || die "codex installation completed but command is still not in PATH"
  fi

  ensure_codex_auth
}

ensure_codex_auth() {
  codex_home="${CODEX_HOME:-$HOME/.codex}"
  if [[ -n "${OPENAI_API_KEY:-}" || -f "$codex_home/auth.json" ]]; then
    log "codex auth/config looks present"
    return
  fi

  log "codex auth is missing; starting codex login"
  codex login
  if [[ -z "${OPENAI_API_KEY:-}" && ! -f "$codex_home/auth.json" ]]; then
    die "codex login finished but auth file was not found at $codex_home/auth.json"
  fi
}

ensure_go() {
  if has_cmd go; then
    log "go found: $(go version)"
    GO_BIN="$(command -v go)"
    export GO_BIN
    return
  fi
  if [[ -x "$LOCAL_GO_DIR/bin/go" ]]; then
    prepend_path "$LOCAL_GO_DIR/bin"
    log "go found: $("$LOCAL_GO_DIR/bin/go" version)"
    GO_BIN="$LOCAL_GO_DIR/bin/go"
    export GO_BIN
    return
  fi

  log "go not found, installing Go $GO_VERSION into $LOCAL_GO_DIR"
  has_cmd tar || die "tar not found; cannot install Go"

  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$arch" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *) die "unsupported CPU architecture for automatic Go install: $arch" ;;
  esac

  archive="go${GO_VERSION}.${os}-${arch}.tar.gz"
  url="https://go.dev/dl/${archive}"
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT

  if has_cmd curl; then
    curl -fsSL "$url" -o "$tmp_dir/go.tar.gz"
  elif has_cmd wget; then
    wget -q "$url" -O "$tmp_dir/go.tar.gz"
  else
    die "curl/wget not found; cannot download Go"
  fi

  mkdir -p "$(dirname "$LOCAL_GO_DIR")"
  rm -rf "$LOCAL_GO_DIR"
  tar -C "$(dirname "$LOCAL_GO_DIR")" -xzf "$tmp_dir/go.tar.gz"
  if [[ "$(dirname "$LOCAL_GO_DIR")/go" != "$LOCAL_GO_DIR" ]]; then
    mv "$(dirname "$LOCAL_GO_DIR")/go" "$LOCAL_GO_DIR"
  fi
  prepend_path "$LOCAL_GO_DIR/bin"
  GO_BIN="$LOCAL_GO_DIR/bin/go"
  export GO_BIN
  "$GO_BIN" version
}

ensure_lark_cli_config() {
  if lark-cli doctor --offline >/dev/null 2>&1; then
    log "lark-cli local config looks present"
    return
  fi

  log "lark-cli config is missing; starting app setup flow"
  log "Follow the browser/QR flow, approve requested app setup, then return to this terminal."
  lark-cli config init --new
}

read_secret() {
  prompt="$1"
  value=""
  while [[ -z "$value" ]]; do
    read -r -s -p "$prompt" value
    printf '\n'
  done
  printf '%s' "$value"
}

read_required() {
  prompt="$1"
  value=""
  while [[ -z "$value" ]]; do
    read -r -p "$prompt" value
    value="$(printf '%s' "$value" | xargs)"
  done
  printf '%s' "$value"
}

detect_owner_open_id() {
  if [[ -n "${OWNER_OPEN_ID:-}" ]]; then
    printf '%s' "$OWNER_OPEN_ID"
    return
  fi

  raw="$(lark-cli contact +get-user --as user --format json 2>/dev/null || true)"
  if [[ -z "$raw" ]]; then
    return 1
  fi

  open_id="$(node -e '
const fs = require("fs");
let data;
try {
  data = JSON.parse(fs.readFileSync(0, "utf8"));
} catch {
  process.exit(1);
}
function findOpenId(value) {
  if (!value || typeof value !== "object") return "";
  if (typeof value.open_id === "string" && value.open_id.trim()) return value.open_id.trim();
  if (Array.isArray(value)) {
    for (const item of value) {
      const found = findOpenId(item);
      if (found) return found;
    }
    return "";
  }
  for (const item of Object.values(value)) {
    const found = findOpenId(item);
    if (found) return found;
  }
  return "";
}
const openId = findOpenId(data);
if (!openId) process.exit(1);
console.log(openId);
' <<<"$raw" 2>/dev/null || true)"

  if [[ -z "$open_id" ]]; then
    return 1
  fi
  printf '%s' "$open_id"
}

write_config_if_missing() {
  if [[ -f "$CONFIG_FILE" ]]; then
    log "config exists: $CONFIG_FILE"
    return
  fi

  if [[ "$USE_LOCAL_CONFIG" == true && -f "$ROOT_DIR/config.json" ]]; then
    log "copying existing repo config to $CONFIG_FILE"
    install -m 600 "$ROOT_DIR/config.json" "$CONFIG_FILE"
    return
  fi

  log "creating $CONFIG_FILE"
  owner_open_id="$(detect_owner_open_id)" || die "failed to get owner_open_id from lark-cli. Run lark-cli auth login for the user identity, or set OWNER_OPEN_ID and rerun."
  log "owner_open_id detected from lark-cli"
  lark_app_id="${LARK_APP_ID:-$(read_required 'lark_app_id: ')}"
  lark_app_secret="${LARK_APP_SECRET:-$(read_secret 'lark_app_secret: ')}"
  default_work_dir="${DEFAULT_WORK_DIR:-$HOME}"

  umask 077
  node - "$CONFIG_FILE" "$owner_open_id" "$lark_app_id" "$lark_app_secret" "$default_work_dir" "$STATE_DIR/state.json" <<'NODE'
const fs = require('fs');
const [path, ownerOpenId, appId, appSecret, defaultWorkDir, statePath] = process.argv.slice(2);
const config = {
  owner_open_id: ownerOpenId,
  default_work_dir: defaultWorkDir,
  state_path: statePath,
  lark_app_id: appId,
  lark_app_secret: appSecret,
  codex_cli: 'codex',
  work_dirs: {},
  default_task_model: 'gpt-5.5',
  default_task_reasoning_effort: 'xhigh',
  default_task_service_tier: 'fast',
  default_chat_model: 'gpt-5.5',
  default_chat_reasoning_effort: 'medium',
  default_chat_service_tier: 'fast',
  log_level: 'info'
};
fs.writeFileSync(path, JSON.stringify(config, null, 2) + '\n', { mode: 0o600 });
NODE
}

build_binary() {
  go_bin="${GO_BIN:-$(command -v go)}"
  log "building $APP_NAME"
  (cd "$ROOT_DIR" && "$go_bin" build -o "$INSTALL_DIR/$APP_NAME" ./cmd/lark-bridge)
}

check_bridge_config() {
  log "checking lark-bridge config"
  (cd "$CONFIG_DIR" && "$INSTALL_DIR/$APP_NAME" --config "$CONFIG_FILE" --check-config)
}

probe_lark_permissions() {
  if [[ "$PROBE_LARK" != true ]]; then
    log "skipping Feishu permission probe"
    return
  fi
  log "probing Feishu bot permissions"
  (cd "$CONFIG_DIR" && "$INSTALL_DIR/$APP_NAME" --config "$CONFIG_FILE" --probe-lark)
}

write_env_file() {
  log "writing env file: $ENV_FILE"
  {
    printf 'PATH=%s:%s/bin:%s/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin\n' "$INSTALL_DIR" "$NPM_PREFIX" "$LOCAL_GO_DIR"
    for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy CODEX_HOME; do
      value="${!name:-}"
      if [[ -n "$value" ]]; then
        printf '%s=%s\n' "$name" "$value"
      fi
    done
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
}

write_systemd_service() {
  has_cmd systemctl || die "systemctl not found; this deploy script currently targets Linux user systemd"
  mkdir -p "$SERVICE_DIR"
  log "writing systemd user service: $SERVICE_FILE"
  cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Lark Bridge daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$CONFIG_DIR
EnvironmentFile=-$ENV_FILE
ExecStart=$INSTALL_DIR/$APP_NAME --config $CONFIG_FILE
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
EOF
}

write_systemd_health_timer() {
  has_cmd systemctl || die "systemctl not found; this deploy script currently targets Linux user systemd"
  mkdir -p "$SERVICE_DIR"
  log "writing health service: $HEALTH_SERVICE_FILE"
  cat > "$HEALTH_SERVICE_FILE" <<EOF
[Unit]
Description=Lark Bridge daily health check
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
WorkingDirectory=$CONFIG_DIR
EnvironmentFile=-$ENV_FILE
ExecStart=$INSTALL_DIR/$APP_NAME --config $CONFIG_FILE --health-once
EOF

  log "writing health timer: $HEALTH_TIMER_FILE"
  cat > "$HEALTH_TIMER_FILE" <<EOF
[Unit]
Description=Run Lark Bridge health check every day at 10:00

[Timer]
OnCalendar=*-*-* 10:00:00
Persistent=true

[Install]
WantedBy=timers.target
EOF
}

enable_service() {
  log "enabling and starting systemd user service"
  systemctl --user daemon-reload
  systemctl --user enable --now "$APP_NAME.service"
  systemctl --user enable --now "$APP_NAME-health.timer"
  systemctl --user --no-pager --full status "$APP_NAME.service" || true
}

print_permission_checklist() {
  cat <<'EOF'

Required Feishu app setup checklist:
  - Bot is enabled and can send messages to the owner.
  - Event subscription includes im.message.receive_v1.
  - Card callback event card.action.trigger is enabled.
  - IM message send/reply/update and reaction scopes are approved.

Useful checks:
  lark-cli doctor
  lark-cli auth scopes --format pretty
  journalctl --user -u lark-bridge -f
  systemctl --user list-timers lark-bridge-health.timer

If Feishu returns permission errors, open the console_url from the error,
approve the missing scope/event, publish the app version if required, then:
  systemctl --user restart lark-bridge
EOF
}

check_cmd() {
  if has_cmd "$1"; then
    log "$1 found: $(command -v "$1")"
    return
  fi
  die "$1 not found"
}

check_go() {
  if has_cmd go; then
    log "go found: $(go version)"
    return
  fi
  if [[ -x "$LOCAL_GO_DIR/bin/go" ]]; then
    log "go found: $("$LOCAL_GO_DIR/bin/go" version)"
    return
  fi
  die "go not found"
}

check_codex_auth() {
  codex_home="${CODEX_HOME:-$HOME/.codex}"
  if [[ -n "${OPENAI_API_KEY:-}" || -f "$codex_home/auth.json" ]]; then
    log "codex auth/config found"
    return
  fi
  die "codex auth missing; run codex login or set OPENAI_API_KEY"
}

check_lark_skills() {
  if [[ -f "$HOME/.agents/skills/lark-shared/SKILL.md" && -f "$HOME/.agents/skills/lark-im/SKILL.md" ]]; then
    log "lark skills found"
    return
  fi
  die "lark skills missing; normal deploy can install them with npx skills add larksuite/cli -g -y"
}

check_lark_config() {
  if lark-cli doctor --offline >/dev/null 2>&1; then
    log "lark-cli local config found"
    return
  fi
  die "lark-cli config missing; normal deploy can start lark-cli config init --new"
}

check_systemd_user() {
  check_cmd systemctl
  if systemctl --user show-environment >/dev/null 2>&1; then
    log "systemd user manager is reachable"
    return
  fi
  die "systemd user manager is not reachable"
}

check_existing_config() {
  if [[ ! -f "$CONFIG_FILE" ]]; then
    die "config missing: $CONFIG_FILE"
  fi
  check_go
  go_bin="$(command -v go || true)"
  if [[ -z "$go_bin" && -x "$LOCAL_GO_DIR/bin/go" ]]; then
    go_bin="$LOCAL_GO_DIR/bin/go"
  fi
  (cd "$ROOT_DIR" && "$go_bin" run ./cmd/lark-bridge --config "$CONFIG_FILE" --check-config)
}

preflight_check() {
  bootstrap_path
  log "running preflight checks only"
  check_cmd npm
  check_cmd npx
  check_cmd lark-cli
  check_lark_skills
  check_cmd codex
  check_codex_auth
  check_go
  check_lark_config
  check_systemd_user
  check_existing_config
  log "preflight ok"
}

main() {
  parse_args "$@"
  if [[ "$CHECK_ONLY" == true ]]; then
    preflight_check
    return
  fi

  bootstrap_path
  ensure_dirs
  ensure_npm
  ensure_lark_cli
  ensure_lark_skills
  ensure_codex
  ensure_go
  ensure_lark_cli_config
  write_config_if_missing
  build_binary
  check_bridge_config
  probe_lark_permissions
  write_env_file
  write_systemd_service
  write_systemd_health_timer
  enable_service
  print_permission_checklist
}

main "$@"
