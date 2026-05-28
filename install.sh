#!/usr/bin/env bash
#
# Lantern installer — pure-Go Filecoin light node.
#
#   curl -fsSL https://get.golantern.io | bash
#
# Or, from a local checkout:
#
#   bash install.sh
#
# Environment variables:
#   LANTERN_VERSION   Tag to install (default: latest)
#   LANTERN_HOME      Data + binary directory (default: ~/.lantern)
#   LANTERN_PREFIX    Where to symlink the binary (default: /usr/local/bin)
#   LANTERN_YES=1     Non-interactive; assume defaults (background service)
#   LANTERN_NO_SERVICE=1   Skip the OS service installation step
#
# Exit codes: 0 success, anything else fatal.

set -euo pipefail

# ---------- UI helpers ----------

CLR_RESET='\033[0m'; CLR_BOLD='\033[1m'
CLR_RED='\033[0;31m'; CLR_GREEN='\033[0;32m'; CLR_YELLOW='\033[0;33m'
CLR_BLUE='\033[0;34m'; CLR_CYAN='\033[0;36m'; CLR_DIM='\033[2m'

if [[ "${TERM:-}" == "dumb" || ! -t 1 ]]; then
  CLR_RESET=''; CLR_BOLD=''; CLR_RED=''; CLR_GREEN=''; CLR_YELLOW=''
  CLR_BLUE=''; CLR_CYAN=''; CLR_DIM=''
fi

banner() {
  cat <<EOF

  ${CLR_CYAN}${CLR_BOLD}🪔  Lantern${CLR_RESET}
  ${CLR_DIM}Pure-Go Filecoin light node${CLR_RESET}
  ${CLR_DIM}no CGo, no 76 GB snapshot, no third-party trust${CLR_RESET}

EOF
}
step()  { printf "\n${CLR_BOLD}▸ %s${CLR_RESET}\n" "$*"; }
ok()    { printf "    ${CLR_GREEN}✓${CLR_RESET} %s\n" "$*"; }
warn()  { printf "    ${CLR_YELLOW}⚠${CLR_RESET}  %s\n" "$*"; }
fail()  { printf "    ${CLR_RED}✗${CLR_RESET} %s\n" "$*" >&2; exit 1; }
info()  { printf "    ${CLR_DIM}·${CLR_RESET} %s\n" "$*"; }
ask()   { # ask "question" default_yes_or_no
  local q="$1" def="${2:-y}" reply
  if [[ "${LANTERN_YES:-}" == "1" ]]; then echo "$def"; return; fi
  if [[ "$def" == "y" ]]; then printf "    ? %s [Y/n] " "$q"; else printf "    ? %s [y/N] " "$q"; fi
  read -r reply || reply="$def"
  reply="${reply:-$def}"
  echo "$reply"
}

# ---------- Preflight ----------

preflight() {
  step "Preflight"
  case "$(uname -s)" in
    Darwin) OS=darwin ;;
    Linux)  OS=linux ;;
    *)      fail "Unsupported OS: $(uname -s). Lantern supports macOS and Linux." ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) ARCH=arm64 ;;
    x86_64|amd64)  ARCH=amd64 ;;
    *)             fail "Unsupported arch: $(uname -m). Lantern supports arm64 and amd64." ;;
  esac
  ok "Detected ${OS}/${ARCH}"

  for cmd in curl tar shasum sha256sum mktemp; do
    if command -v "$cmd" >/dev/null 2>&1; then
      :
    elif [[ "$cmd" == "shasum" || "$cmd" == "sha256sum" ]]; then
      # we need at least one of them
      continue
    else
      fail "Missing required command: $cmd"
    fi
  done
  if ! command -v shasum >/dev/null 2>&1 && ! command -v sha256sum >/dev/null 2>&1; then
    fail "Need either shasum or sha256sum for SHA-256 verification"
  fi
  ok "Tools: curl, tar, shasum/sha256sum available"

  LANTERN_HOME="${LANTERN_HOME:-$HOME/.lantern}"
  LANTERN_PREFIX="${LANTERN_PREFIX:-/usr/local/bin}"
  mkdir -p "$LANTERN_HOME"
  ok "Data directory: $LANTERN_HOME"
}

# ---------- Download ----------

sha256_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

download_binary() {
  step "Download Lantern binary"
  LANTERN_VERSION="${LANTERN_VERSION:-latest}"
  bin_name="lantern-${OS}-${ARCH}"
  target="${LANTERN_HOME}/lantern"

  # Idempotence: if a binary already exists and matches the latest sha,
  # skip download.
  if [[ -x "$target" && "${LANTERN_REINSTALL:-0}" != "1" ]]; then
    if existing_sha=$(sha256_of "$target" 2>/dev/null); then
      info "Existing binary at $target (sha256 $(echo "$existing_sha" | cut -c1-12)...)"
      info "Skipping download (LANTERN_REINSTALL=1 to force)"
      ok "Using existing binary"
      return
    fi
  fi

  # Mirror chain. The Lantern git repo is currently private, so the
  # GitHub release endpoint returns 404 for anonymous downloads. The
  # primary mirror is dl-lantern.reiers.io, served from the same
  # Hetzner host that runs the gateway. GitHub releases are listed as
  # a fallback so once the repo flips public the chain Just Works
  # without an installer update.
  if [[ "$LANTERN_VERSION" == "latest" ]]; then
    declare -a urls=(
      "https://dl-lantern.reiers.io/latest/${bin_name}"
      "https://github.com/Reiers/lantern/releases/latest/download/${bin_name}"
    )
  else
    declare -a urls=(
      "https://dl-lantern.reiers.io/${LANTERN_VERSION}/${bin_name}"
      "https://github.com/Reiers/lantern/releases/download/${LANTERN_VERSION}/${bin_name}"
    )
  fi

  tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' EXIT

  bin_url=""
  for candidate in "${urls[@]}"; do
    info "Trying ${candidate}"
    http_code=$(curl -fsSL -o "$tmp_dir/$bin_name" -w "%{http_code}" "$candidate" 2>/dev/null || echo "000")
    if [[ "$http_code" == "200" ]]; then
      bin_url="$candidate"
      break
    fi
    info "  not available (HTTP $http_code)"
  done

  if [[ -z "$bin_url" ]]; then
    warn "No release binary available from any mirror"
    info "Falling back to local source build (requires Go 1.25+)"
    build_from_source "$target"
    return
  fi

  sha_url="${bin_url}.sha256"
  info "Fetching SHA-256 manifest from ${sha_url}"
  if curl -fsSL -o "$tmp_dir/$bin_name.sha256" "$sha_url"; then
    expected=$(cut -d' ' -f1 < "$tmp_dir/$bin_name.sha256")
    actual=$(sha256_of "$tmp_dir/$bin_name")
    if [[ "$expected" != "$actual" ]]; then
      fail "SHA-256 mismatch! expected=$expected actual=$actual"
    fi
    ok "SHA-256 verified ($expected)"
  else
    warn "SHA-256 file not available; skipping integrity check"
  fi

  chmod +x "$tmp_dir/$bin_name"
  mv "$tmp_dir/$bin_name" "$target"
  ok "Installed binary to $target"

  install_symlink "$target"
}

build_from_source() {
  local target="$1"
  if ! command -v go >/dev/null 2>&1; then
    fail "go is not installed and no release binary available. Install Go 1.25+ from https://go.dev/dl"
  fi
  # If install.sh is being run from inside a clone, build from here.
  if [[ -f "go.mod" && -d "cmd/lantern" ]]; then
    info "Building from $(pwd)"
    CGO_ENABLED=0 go build -o "$target" ./cmd/lantern
    chmod +x "$target"
    ok "Built and installed to $target"
    install_symlink "$target"
    return
  fi
  fail "No release artifact and no source tree found to build from"
}

install_symlink() {
  local target="$1"
  local link="${LANTERN_PREFIX}/lantern"
  if [[ -L "$link" || -e "$link" ]]; then
    local existing
    existing=$(readlink "$link" 2>/dev/null || echo "$link")
    if [[ "$existing" == "$target" ]]; then
      ok "Symlink already in place: $link → $target"
      return
    fi
    info "Replacing existing $link (was: $existing)"
  fi
  if [[ -w "$(dirname "$link")" ]]; then
    ln -sf "$target" "$link"
    ok "Symlink: $link → $target"
  else
    info "Need sudo to write $link"
    if sudo ln -sf "$target" "$link"; then
      ok "Symlink: $link → $target"
    else
      warn "Could not create $link. Add $LANTERN_HOME to PATH or run with sudo."
    fi
  fi
}

# ---------- Trust bootstrap ----------

trust_bootstrap() {
  step "Trust bootstrap — multi-source quorum"
  info "This runs once. Asking 5+ independent sources for the current"
  info "F3-finalized chain head; refuses to continue if they disagree."

  local q="${LANTERN_BOOTSTRAP_QUORUM:-5}"
  local t="${LANTERN_BOOTSTRAP_TIMEOUT:-90s}"
  local extra_peers=""
  if [[ -n "${LANTERN_PEERS:-}" ]]; then
    # comma-separated list → repeated --peer flags
    IFS=',' read -r -a _peers <<< "$LANTERN_PEERS"
    for p in "${_peers[@]}"; do extra_peers+=" --peer $p"; done
  fi

  # Detect whether an anchor already exists; offer to skip if so.
  if [[ -s "${LANTERN_HOME}/bootstrap-anchor.json" && "${LANTERN_REANCHOR:-0}" != "1" ]]; then
    info "Existing bootstrap anchor found:"
    sed 's/^/      /' "${LANTERN_HOME}/bootstrap-anchor.json"
    info "Re-run with LANTERN_REANCHOR=1 to refresh."
    ok "Skipping quorum probe (anchor exists)"
    return
  fi

  # Run `lantern init` in --no-wallet mode; we handle wallet creation
  # ourselves below so the installer can ask the user about it.
  local bin="${LANTERN_HOME}/lantern"
  if ! LANTERN_HOME="$LANTERN_HOME" \
       "$bin" init \
         --bootstrap-quorum="$q" \
         --bootstrap-timeout="$t" \
         --no-wallet $extra_peers; then
    fail "Quorum bootstrap failed. Try 'lantern doctor' for per-source diagnostics."
  fi
}

# ---------- Wallet ----------

wallet_setup() {
  step "Wallet"
  local bin="${LANTERN_HOME}/lantern"
  if "$bin" wallet list 2>/dev/null | grep -q '^\*'; then
    ok "Wallet already configured"
    return
  fi
  ans=$(ask "Create a fresh BLS wallet now?" y)
  if [[ "$ans" =~ ^[Yy]$ ]]; then
    "$bin" wallet new --type=bls
    ok "Wallet created"
  else
    info "Skipping wallet creation. Use 'lantern wallet new --type=bls' later."
  fi
}

# ---------- Service ----------

service_setup() {
  if [[ "${LANTERN_NO_SERVICE:-}" == "1" ]]; then
    info "LANTERN_NO_SERVICE=1 set; skipping OS service installation"
    return
  fi
  step "Daemon lifecycle"
  local default_choice
  if [[ "${LANTERN_YES:-}" == "1" ]]; then
    default_choice=background
  else
    default_choice=foreground
  fi

  printf "    How should Lantern run?\n"
  printf "      ${CLR_BOLD}b${CLR_RESET}) Background service (launchd / systemd user)\n"
  printf "      ${CLR_BOLD}f${CLR_RESET}) Foreground only (start manually with 'lantern daemon')\n"
  printf "      ${CLR_BOLD}s${CLR_RESET}) Skip — I'll decide later\n"
  if [[ "${LANTERN_YES:-}" == "1" ]]; then
    choice="b"
  else
    printf "    Choice [default: ${default_choice:0:1}]: "
    read -r choice || choice=""
    choice="${choice:-${default_choice:0:1}}"
  fi

  case "$choice" in
    b|B|background)
      "${LANTERN_HOME}/lantern" service install
      ok "Service installed and started"
      ;;
    f|F|foreground)
      ok "Foreground only. Start with: lantern daemon"
      ;;
    s|S|skip|*)
      info "Skipped service setup. Run 'lantern service install' later."
      ;;
  esac
}

# ---------- Closing summary ----------

closing() {
  step "Done"
  local bin="${LANTERN_HOME}/lantern"
  local token=""
  if [[ -s "${LANTERN_HOME}/token" ]]; then
    token=$(cat "${LANTERN_HOME}/token")
  fi

  cat <<EOF

  ${CLR_GREEN}✓ Lantern is installed.${CLR_RESET}

  Status:        ${CLR_BOLD}lantern info${CLR_RESET}
  Chain head:    ${CLR_BOLD}lantern chain head${CLR_RESET}
  Service:       ${CLR_BOLD}lantern service status${CLR_RESET}
  Refresh trust: ${CLR_BOLD}lantern repair${CLR_RESET}

EOF
  if [[ -n "$token" ]]; then
    printf "  ${CLR_DIM}Connect Curio / Boost:${CLR_RESET}\n"
    printf "    export FULLNODE_API_INFO='%s:/ip4/127.0.0.1/tcp/1234/http'\n\n" "$token"
  fi

  # Closing quote (filbucket-style rotating).
  local quotes=(
    '"The lighter the node, the brighter the chain."'
    '"Trust the math, not the gateway."'
    '"Five sources agree; the chain has spoken."'
    '"No CGo. No snapshot. No third-party trust."'
  )
  local idx=$((RANDOM % ${#quotes[@]}))
  printf "  ${CLR_DIM}%s${CLR_RESET}\n\n" "${quotes[$idx]}"
  printf "  ${CLR_DIM}Lantern home: %s${CLR_RESET}\n" "$LANTERN_HOME"
  printf "  ${CLR_DIM}Logs:         tail -f %s/lantern.log${CLR_RESET}\n\n" "$LANTERN_HOME"
}

# ---------- main ----------

main() {
  banner
  preflight
  download_binary
  trust_bootstrap
  wallet_setup
  service_setup
  closing
}

main "$@"
