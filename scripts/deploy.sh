#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="lark-bridge"
GO_VERSION="${GO_VERSION:-1.22.12}"
MIN_NODE_MAJOR="${MIN_NODE_MAJOR:-18}"
NODE_VERSION="${NODE_VERSION:-lts/*}"
NVM_VERSION="${NVM_VERSION:-v0.40.3}"
HEARTBEAT_SECONDS="${HEARTBEAT_SECONDS:-15}"
DOWNLOAD_CONNECT_TIMEOUT="${DOWNLOAD_CONNECT_TIMEOUT:-15}"
DOWNLOAD_MAX_TIME="${DOWNLOAD_MAX_TIME:-300}"
LARK_SKILLS_PACKAGE="${LARK_SKILLS_PACKAGE:-larksuite/cli}"
LARK_SKILLS_AGENT="${LARK_SKILLS_AGENT:-codex}"
LARK_SKILLS_CHECK="${LARK_SKILLS_CHECK:-lark-shared}"
CODEX_LOGIN_MODE="${CODEX_LOGIN_MODE:-device}"
LARK_CONFIG_INIT_TIMEOUT="${LARK_CONFIG_INIT_TIMEOUT:-600}"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_HOME="${XDG_CONFIG_HOME:-$HOME/.config}"
STATE_HOME="${XDG_STATE_HOME:-$HOME/.local/state}"
CONFIG_DIR="$CONFIG_HOME/$APP_NAME"
STATE_DIR="$STATE_HOME/$APP_NAME"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
NPM_PREFIX="${LARK_BRIDGE_NPM_PREFIX:-$HOME/.local/npm_packages}"
LOCAL_GO_DIR="${LOCAL_GO_DIR:-$HOME/.local/go}"
CONFIG_FILE="${CONFIG_FILE:-$CONFIG_DIR/config.json}"
ENV_FILE="$CONFIG_DIR/env"
SHELL_ENV_FILE="$CONFIG_DIR/shell.env"
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

on_error() {
  status="$1"
  line="$2"
  command="$3"
  printf '[%s] ERROR: command failed at line %s with exit %s: %s\n' "$APP_NAME" "$line" "$status" "$command" >&2
}

trap 'on_error "$?" "$LINENO" "$BASH_COMMAND"' ERR

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

now_seconds() {
  date +%s
}

run_with_heartbeat() {
  label="$1"
  shift
  start="$(now_seconds)"
  log "$label"
  "$@" &
  pid="$!"
  while kill -0 "$pid" >/dev/null 2>&1; do
    sleep "$HEARTBEAT_SECONDS"
    if kill -0 "$pid" >/dev/null 2>&1; then
      elapsed="$(( $(now_seconds) - start ))"
      log "$label still running (${elapsed}s)"
    fi
  done
  set +e
  wait "$pid"
  status="$?"
  set -e
  elapsed="$(( $(now_seconds) - start ))"
  if [[ "$status" -ne 0 ]]; then
    die "$label failed after ${elapsed}s"
  fi
  log "$label done (${elapsed}s)"
}

prepend_path() {
  case ":$PATH:" in
    *":$1:"*) ;;
    *) export PATH="$1:$PATH" ;;
  esac
}

bootstrap_path() {
  log "bootstrapping PATH"
  prepend_path "$INSTALL_DIR"
  prepend_path "$NPM_PREFIX/bin"
  prepend_path "$LOCAL_GO_DIR/bin"
  prepend_path "/usr/local/go/bin"
}

shell_quote() {
  printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

trim_space() {
  value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

check_npm_nvm_compatibility() {
  if [[ -n "${NPM_CONFIG_PREFIX:-}" || -n "${npm_config_prefix:-}" ]]; then
    die "NPM_CONFIG_PREFIX is set, which is incompatible with nvm. Run manually: unset NPM_CONFIG_PREFIX npm_config_prefix"
  fi

  npmrc="$HOME/.npmrc"
  [[ -f "$npmrc" ]] || return 0

  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^[[:space:]]*prefix[[:space:]]*= ]]; then
      value="$(trim_space "${line#*=}")"
      die "npm prefix in $npmrc is incompatible with nvm: $value. Remove it manually, for example: npm config delete prefix --location=user"
    fi
    if [[ "$line" =~ ^[[:space:]]*globalconfig[[:space:]]*= ]]; then
      value="$(trim_space "${line#*=}")"
      die "npm globalconfig in $npmrc is incompatible with nvm: $value. Remove it manually, for example: npm config delete globalconfig --location=user"
    fi
  done < "$npmrc"
}

load_nvm() {
  if ! has_cmd nvm; then
    nvm_dir="${NVM_DIR:-$HOME/.nvm}"
    if [[ -s "$nvm_dir/nvm.sh" ]]; then
      check_npm_nvm_compatibility
      # shellcheck disable=SC1090
      . "$nvm_dir/nvm.sh"
    fi
  fi

  if has_cmd nvm; then
    check_npm_nvm_compatibility
    nvm use --silent default >/dev/null 2>&1 || nvm use --silent node >/dev/null 2>&1 || true
  fi
}

node_major_version() {
  node --version | sed -E 's/^v([0-9]+).*/\1/'
}

ensure_dirs() {
  log "ensuring install directories"
  mkdir -p "$CONFIG_DIR" "$STATE_DIR" "$INSTALL_DIR"
}

install_nvm() {
  nvm_dir="${NVM_DIR:-$HOME/.nvm}"
  if [[ -s "$nvm_dir/nvm.sh" ]]; then
    return
  fi

  check_npm_nvm_compatibility

  nvm_url="https://raw.githubusercontent.com/nvm-sh/nvm/$NVM_VERSION/install.sh"
  log "nvm not found, installing nvm $NVM_VERSION into $nvm_dir"
  mkdir -p "$nvm_dir"
  if has_cmd curl; then
    curl -fL --connect-timeout "$DOWNLOAD_CONNECT_TIMEOUT" --max-time "$DOWNLOAD_MAX_TIME" "$nvm_url" | PROFILE=/dev/null NVM_DIR="$nvm_dir" bash
  elif has_cmd wget; then
    wget --timeout="$DOWNLOAD_CONNECT_TIMEOUT" --tries=3 -O- "$nvm_url" | PROFILE=/dev/null NVM_DIR="$nvm_dir" bash
  else
    die "curl or wget is required to install nvm"
  fi

  [[ -s "$nvm_dir/nvm.sh" ]] || die "nvm installation did not create $nvm_dir/nvm.sh"
}

validate_npm() {
  missing_node_msg="$1"
  version_msg="$2"

  has_cmd node || die "$missing_node_msg"
  has_cmd npm || die "npm not found. Install Node.js/npm first, then rerun."
  has_cmd npx || die "npx not found. Install Node.js/npm first, then rerun."

  node_major="$(node_major_version)"
  log "node found: $(node --version) ($(command -v node))"
  log "npm found: $(npm --version) ($(command -v npm))"
  log "npx found: $(npx --version) ($(command -v npx))"
  if [[ ! "$node_major" =~ ^[0-9]+$ || "$node_major" -lt "$MIN_NODE_MAJOR" ]]; then
    die "$version_msg; current node is $(node --version) at $(command -v node)."
  fi
}

ensure_node_runtime() {
  log "checking Node.js runtime"
  load_nvm

  if has_cmd node && has_cmd npm && has_cmd npx; then
    node_major="$(node_major_version)"
    if [[ "$node_major" =~ ^[0-9]+$ && "$node_major" -ge "$MIN_NODE_MAJOR" ]]; then
      return
    fi
  fi

  install_nvm
  load_nvm
  has_cmd nvm || die "nvm was installed but is not available in the current shell"

  run_with_heartbeat "installing Node.js $NODE_VERSION with nvm" nvm install "$NODE_VERSION"
  installed_node="$(nvm version "$NODE_VERSION")"
  if [[ "$installed_node" == "N/A" ]]; then
    installed_node="$NODE_VERSION"
  fi
  nvm alias default "$installed_node" >/dev/null
  nvm use --silent "$installed_node"
}

check_npm() {
  log "checking npm runtime"
  load_nvm
  validate_npm \
    "node not found; normal deploy can install Node.js with nvm" \
    "Node.js >= $MIN_NODE_MAJOR is required. Normal deploy can install a supported version with nvm"
}

ensure_npm() {
  ensure_node_runtime
  validate_npm \
    "node not found after attempted nvm install" \
    "Node.js >= $MIN_NODE_MAJOR is required"
}

write_shell_env_file() {
  node_bin_dir="$(dirname "$(command -v node)")"
  nvm_dir="${NVM_DIR:-$HOME/.nvm}"
  log "writing shell env file: $SHELL_ENV_FILE"
  {
    printf '# Generated by lark-bridge deploy. Safe to source from interactive shells.\n'
    printf 'export NVM_DIR=%s\n' "$(shell_quote "$nvm_dir")"
    printf '[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"\n'
    printf '_lark_bridge_prepend_path() { case ":$PATH:" in *":$1:"*) ;; *) PATH="$1:$PATH" ;; esac; }\n'
    printf '_lark_bridge_prepend_path %s\n' "$(shell_quote "$INSTALL_DIR")"
    printf '_lark_bridge_prepend_path %s\n' "$(shell_quote "$NPM_PREFIX/bin")"
    printf '_lark_bridge_prepend_path %s\n' "$(shell_quote "$LOCAL_GO_DIR/bin")"
    printf '_lark_bridge_prepend_path %s\n' "$(shell_quote "$node_bin_dir")"
    printf 'export PATH\n'
    printf 'unset -f _lark_bridge_prepend_path 2>/dev/null || true\n'
  } > "$SHELL_ENV_FILE"
  chmod 600 "$SHELL_ENV_FILE"
}

source_line_for_profile() {
  printf '[ -f %s ] && . %s' "$(shell_quote "$SHELL_ENV_FILE")" "$(shell_quote "$SHELL_ENV_FILE")"
}

ensure_profile_sources_shell_env() {
  source_line="$(source_line_for_profile)"
  profiles=("$HOME/.profile")
  case "${SHELL:-}" in
    */zsh) profiles+=("$HOME/.zshrc") ;;
    */bash) profiles+=("$HOME/.bashrc") ;;
  esac
  [[ -f "$HOME/.zshrc" ]] && profiles+=("$HOME/.zshrc")
  [[ -f "$HOME/.bashrc" ]] && profiles+=("$HOME/.bashrc")

  seen_profiles=""
  for profile in "${profiles[@]}"; do
    case ":$seen_profiles:" in
      *":$profile:"*) continue ;;
    esac
    seen_profiles="$seen_profiles:$profile"
    mkdir -p "$(dirname "$profile")"
    touch "$profile"
    if grep -F "$SHELL_ENV_FILE" "$profile" >/dev/null 2>&1; then
      continue
    fi
    {
      printf '\n# lark-bridge environment\n'
      printf '%s\n' "$source_line"
    } >> "$profile"
    log "added lark-bridge env source to $profile"
  done
}

ensure_shell_environment() {
  write_shell_env_file
  ensure_profile_sources_shell_env
}

npm_install_global() {
  package="$1"
  command_name="$2"
  mkdir -p "$NPM_PREFIX"
  prepend_path "$NPM_PREFIX/bin"
  run_with_heartbeat "installing $package into $NPM_PREFIX" npm install -g --prefix "$NPM_PREFIX" "$package" --no-audit --no-fund --progress=false
  prepend_path "$NPM_PREFIX/bin"
  has_cmd "$command_name" || die "$command_name installation completed but command is still not in PATH"
}

ensure_lark_cli() {
  if has_cmd lark-cli; then
    log "lark-cli found: $(lark-cli --version)"
    return
  fi

  log "lark-cli not found, installing @larksuite/cli into $NPM_PREFIX"
  npm_install_global "@larksuite/cli" "lark-cli"
}

skill_installed() {
  skill_name="$1"
  codex_home="${CODEX_HOME:-$HOME/.codex}"
  for skills_dir in "$HOME/.agents/skills" "$codex_home/skills"; do
    if [[ -f "$skills_dir/$skill_name/SKILL.md" ]]; then
      return 0
    fi
  done
  return 1
}

ensure_lark_skills() {
  if skill_installed "$LARK_SKILLS_CHECK"; then
    log "lark skills found: $LARK_SKILLS_CHECK"
    return
  fi

  run_with_heartbeat "installing lark skills package $LARK_SKILLS_PACKAGE for agent $LARK_SKILLS_AGENT" \
    npx --yes skills add "$LARK_SKILLS_PACKAGE" -g -y -a "$LARK_SKILLS_AGENT"
  skill_installed "$LARK_SKILLS_CHECK" || die "lark skills installation completed but $LARK_SKILLS_CHECK was not found"
}

ensure_codex() {
  if has_cmd codex; then
    log "codex found: $(codex --version 2>/dev/null | tail -n 1)"
  else
    log "codex not found, installing @openai/codex into $NPM_PREFIX"
    npm_install_global "@openai/codex" "codex"
  fi

  ensure_codex_auth
}

ensure_codex_auth() {
  codex_home="${CODEX_HOME:-$HOME/.codex}"
  if [[ -n "${OPENAI_API_KEY:-}" || -f "$codex_home/auth.json" ]]; then
    log "codex auth/config looks present"
    return
  fi

  login_mode="$CODEX_LOGIN_MODE"
  if [[ -t 0 ]]; then
    printf '[%s] codex auth is missing. Choose login mode:\n' "$APP_NAME"
    printf '  1) device auth (recommended for remote/headless servers)\n'
    printf '  2) browser auth (local desktop/browser available)\n'
    printf '  3) skip for now\n'
    read -r -p "Select [1]: " login_choice
    case "${login_choice:-1}" in
      1 | device) login_mode="device" ;;
      2 | browser) login_mode="browser" ;;
      3 | skip) login_mode="skip" ;;
      *) die "unknown Codex login choice: $login_choice" ;;
    esac
  fi

  case "$login_mode" in
    skip)
      warn "codex auth is missing; skipping login"
      return
      ;;
    browser)
      log "codex auth is missing; starting browser login"
      codex login
      ;;
    device)
      log "codex auth is missing; starting device login"
      codex login --device-auth
      ;;
    *)
      die "unknown Codex login mode: $login_mode; use device, browser, or skip"
      ;;
  esac

  if [[ -z "${OPENAI_API_KEY:-}" && ! -f "$codex_home/auth.json" ]]; then
    die "codex login finished but auth file was not found at $codex_home/auth.json. You can choose skip and login manually later."
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
  set +e
  if has_cmd timeout; then
    timeout --foreground "${LARK_CONFIG_INIT_TIMEOUT}s" lark-cli config init --new
  else
    lark-cli config init --new
  fi
  status="$?"
  set -e

  if lark-cli doctor --offline >/dev/null 2>&1; then
    log "lark-cli local config looks present after setup"
    return
  fi

  if [[ "$status" -eq 124 ]]; then
    die "lark-cli config init timed out after ${LARK_CONFIG_INIT_TIMEOUT}s and local config is still missing. If you already approved it, run lark-cli doctor --offline, then rerun this deploy script."
  fi
  if [[ "$status" -ne 0 ]]; then
    die "lark-cli config init failed with exit $status and local config is still missing"
  fi

  die "lark-cli config init finished but local config is still missing"
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

  raw="$(lark-cli auth status 2>/dev/null || true)"
  open_id="$(extract_open_id "$raw" || true)"
  if [[ -n "$open_id" ]]; then
    printf '%s' "$open_id"
    return
  fi

  raw="$(lark-cli contact +get-user --as user --format json 2>/dev/null || true)"
  open_id="$(extract_open_id "$raw" || true)"
  if [[ -n "$open_id" ]]; then
    printf '%s' "$open_id"
    return
  fi

  return 1
}

extract_open_id() {
  raw="$1"
  [[ -n "$raw" ]] || return 1
  node -e '
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
  if (typeof value.openId === "string" && value.openId.trim()) return value.openId.trim();
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
' <<<"$raw" 2>/dev/null
}

resolve_owner_open_id() {
  owner_open_id="$(detect_owner_open_id || true)"
  if [[ -n "$owner_open_id" ]]; then
    log "owner_open_id detected from lark-cli" >&2
    printf '%s' "$owner_open_id"
    return
  fi

  if [[ -t 0 ]]; then
    warn "failed to detect owner_open_id from lark-cli"
    read_required "owner_open_id: "
    return
  fi

  die "failed to get owner_open_id from lark-cli. Run lark-cli auth login for the user identity, or set OWNER_OPEN_ID and rerun."
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
  owner_open_id="$(resolve_owner_open_id)"
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
  node_bin_dir="$(dirname "$(command -v node)")"
  {
    printf 'PATH=%s:%s/bin:%s/bin:%s:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin\n' "$INSTALL_DIR" "$NPM_PREFIX" "$LOCAL_GO_DIR" "$node_bin_dir"
    for name in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy CODEX_HOME; do
      value="${!name:-}"
      if [[ -n "$value" ]]; then
        printf '%s=%s\n' "$name" "$value"
      fi
    done
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
}

ensure_systemd_user() {
  has_cmd systemctl || die "systemctl not found; this deploy script currently targets Linux user systemd"
  if ! systemctl --user show-environment >/dev/null 2>&1; then
    die "systemd user manager is not reachable"
  fi

  if has_cmd loginctl; then
    system_user="${USER:-$(id -un)}"
    linger="$(loginctl show-user "$system_user" -p Linger --value 2>/dev/null || true)"
    if [[ "$linger" != "yes" ]]; then
      die "systemd linger is disabled for $system_user. Run: loginctl enable-linger $system_user"
    fi
    log "systemd linger enabled for $system_user"
  else
    warn "loginctl not found; cannot verify systemd linger"
  fi
}

write_systemd_service() {
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
  if skill_installed "$LARK_SKILLS_CHECK"; then
    log "lark skills found: $LARK_SKILLS_CHECK"
    return
  fi
  die "lark skills missing; normal deploy can install them with: npx --yes skills add $LARK_SKILLS_PACKAGE -g -y -a $LARK_SKILLS_AGENT"
}

check_lark_config() {
  if lark-cli doctor --offline >/dev/null 2>&1; then
    log "lark-cli local config found"
    return
  fi
  die "lark-cli config missing; normal deploy can start lark-cli config init --new"
}

check_systemd_user() {
  ensure_systemd_user
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
  check_npm
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
  ensure_shell_environment
  ensure_lark_cli
  ensure_lark_skills
  ensure_codex
  ensure_go
  ensure_lark_cli_config
  write_config_if_missing
  build_binary
  check_bridge_config
  probe_lark_permissions
  ensure_systemd_user
  write_env_file
  write_systemd_service
  write_systemd_health_timer
  enable_service
  print_permission_checklist
}

main "$@"
