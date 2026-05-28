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
#
# Use real ESC bytes via $'\e[..]' so colors render in BOTH printf and
# cat<<EOF heredocs. The previous '\033[..]' strings only rendered when
# fed through printf, which is why the banner+closing summary in earlier
# releases looked like raw escape codes.

CLR_RESET=$'\e[0m'; CLR_BOLD=$'\e[1m'; CLR_DIM=$'\e[2m'
CLR_RED=$'\e[0;31m'; CLR_GREEN=$'\e[0;32m'; CLR_YELLOW=$'\e[0;33m'
CLR_BLUE=$'\e[0;34m'; CLR_CYAN=$'\e[0;36m'; CLR_MAGENTA=$'\e[0;35m'

# Disable colors if stdout isn't a TTY or NO_COLOR is set. When run via
# `curl ... | bash`, stdout IS still a TTY (the user's terminal), so colors
# are on by default.
if [[ "${TERM:-}" == "dumb" || ! -t 1 || -n "${NO_COLOR:-}" ]]; then
  CLR_RESET=''; CLR_BOLD=''; CLR_DIM=''
  CLR_RED=''; CLR_GREEN=''; CLR_YELLOW=''
  CLR_BLUE=''; CLR_CYAN=''; CLR_MAGENTA=''
fi

banner() {
  printf '\n'
  printf '  %s%s🪔  Lantern%s\n' "$CLR_CYAN" "$CLR_BOLD" "$CLR_RESET"
  printf '  %sPure-Go Filecoin light node%s\n' "$CLR_DIM" "$CLR_RESET"
  printf '  %sno CGo, no 76 GB snapshot, no third-party trust%s\n' "$CLR_DIM" "$CLR_RESET"
  printf '\n'
}
step()  { printf '\n%s▸ %s%s\n' "$CLR_BOLD" "$*" "$CLR_RESET"; }
ok()    { printf '    %s✓%s %s\n' "$CLR_GREEN" "$CLR_RESET" "$*"; }
warn()  { printf '    %s⚠%s  %s\n' "$CLR_YELLOW" "$CLR_RESET" "$*"; }
fail()  { printf '    %s✗%s %s\n' "$CLR_RED" "$CLR_RESET" "$*" >&2; exit 1; }
info()  { printf '    %s·%s %s\n' "$CLR_DIM" "$CLR_RESET" "$*"; }
ask()   { # ask "question" default_yes_or_no
  local q="$1" def="${2:-y}" reply
  if [[ "${LANTERN_YES:-}" == "1" ]]; then echo "$def"; return; fi
  if [[ "$def" == "y" ]]; then printf '    ? %s [Y/n] ' "$q"; else printf '    ? %s [y/N] ' "$q"; fi
  # Read from /dev/tty so prompts work when the script is piped through bash.
  if [[ -r /dev/tty ]]; then
    read -r reply </dev/tty || reply="$def"
  else
    read -r reply || reply="$def"
  fi
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
  mkdir -p "$LANTERN_HOME"
  ok "Data directory: $LANTERN_HOME"

  # Pick a sensible PATH directory for the symlink. Honor LANTERN_PREFIX if set;
  # otherwise prefer Homebrew Apple Silicon (/opt/homebrew/bin), then Intel/Linux
  # /usr/local/bin, then user-local ~/.local/bin. We do NOT assume /usr/local/bin
  # exists — fresh Apple Silicon Macs without Homebrew don't have it.
  if [[ -n "${LANTERN_PREFIX:-}" ]]; then
    : # honor caller
  elif [[ -d /opt/homebrew/bin ]]; then
    LANTERN_PREFIX=/opt/homebrew/bin
  elif [[ -d /usr/local/bin ]]; then
    LANTERN_PREFIX=/usr/local/bin
  else
    LANTERN_PREFIX="$HOME/.local/bin"
  fi
  ok "Symlink target: ${LANTERN_PREFIX}/lantern"
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
  # skip download. We still call install_symlink at the end so the PATH
  # link is repaired even on a no-op re-run.
  if [[ -x "$target" && "${LANTERN_REINSTALL:-0}" != "1" ]]; then
    if existing_sha=$(sha256_of "$target" 2>/dev/null); then
      info "Existing binary at $target (sha256 $(echo "$existing_sha" | cut -c1-12)...)"
      info "Skipping download (LANTERN_REINSTALL=1 to force)"
      ok "Using existing binary"
      install_symlink "$target"
      return
    fi
  fi

  # Mirror chain. GitHub releases is the canonical source (the repo is
  # public). The dl-lantern.reiers.io mirror is a soft fallback for users
  # behind networks that block raw GitHub asset CDN.
  if [[ "$LANTERN_VERSION" == "latest" ]]; then
    declare -a urls=(
      "https://github.com/Reiers/lantern/releases/latest/download/${bin_name}"
      "https://dl-lantern.reiers.io/latest/${bin_name}"
    )
  else
    declare -a urls=(
      "https://github.com/Reiers/lantern/releases/download/${LANTERN_VERSION}/${bin_name}"
      "https://dl-lantern.reiers.io/${LANTERN_VERSION}/${bin_name}"
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
  local dir; dir="$(dirname "$link")"

  # Make sure the prefix dir exists. If it's the user-local fallback we own it,
  # otherwise we leave it to the user (we should have picked an existing system dir
  # in preflight, but just in case).
  if [[ ! -d "$dir" ]]; then
    if [[ "$dir" == "$HOME"* ]]; then
      mkdir -p "$dir"
      ok "Created $dir"
    else
      warn "$dir does not exist; falling back to \$HOME/.local/bin"
      LANTERN_PREFIX="$HOME/.local/bin"
      link="${LANTERN_PREFIX}/lantern"
      dir="$LANTERN_PREFIX"
      mkdir -p "$dir"
    fi
  fi

  if [[ -L "$link" || -e "$link" ]]; then
    local existing
    existing=$(readlink "$link" 2>/dev/null || echo "$link")
    if [[ "$existing" == "$target" ]]; then
      ok "Symlink already in place: $link → $target"
      return
    fi
    info "Replacing existing $link (was: $existing)"
  fi

  if [[ -w "$dir" ]]; then
    ln -sf "$target" "$link"
    ok "Symlink: $link → $target"
  else
    info "Need sudo to write $link (system dir, not user-owned)"
    if sudo ln -sf "$target" "$link"; then
      ok "Symlink: $link → $target"
    else
      warn "Could not create $link. Falling back to user-local install."
      LANTERN_PREFIX="$HOME/.local/bin"
      link="${LANTERN_PREFIX}/lantern"
      mkdir -p "$LANTERN_PREFIX"
      ln -sf "$target" "$link"
      ok "Symlink: $link → $target"
    fi
  fi

  # PATH check + actionable hint.
  if ! command -v lantern >/dev/null 2>&1 \
     || [[ "$(command -v lantern 2>/dev/null)" != "$link" && "$(readlink "$(command -v lantern 2>/dev/null)" 2>/dev/null)" != "$target" ]]; then
    if [[ ":$PATH:" != *":$LANTERN_PREFIX:"* ]]; then
      warn "$LANTERN_PREFIX is not in your PATH yet."
      info "Add this line to your shell profile (~/.zshrc or ~/.bashrc):"
      info "    export PATH=\"$LANTERN_PREFIX:\$PATH\""
      info "Then reload with:  exec \$SHELL -l"
      info "Until then, invoke with the full path:  $target"
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

  printf '    How should Lantern run?\n'
  printf '      %sb%s) Background service (launchd / systemd user)\n' "$CLR_BOLD" "$CLR_RESET"
  printf "      %sf%s) Foreground only (start manually with 'lantern daemon')\n" "$CLR_BOLD" "$CLR_RESET"
  printf "      %ss%s) Skip — I'll decide later\n" "$CLR_BOLD" "$CLR_RESET"
  if [[ "${LANTERN_YES:-}" == "1" ]]; then
    choice="b"
  else
    printf '    Choice [default: %s]: ' "${default_choice:0:1}"
    if [[ -r /dev/tty ]]; then
      read -r choice </dev/tty || choice=""
    else
      read -r choice || choice=""
    fi
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

  # Resolve the canonical command the user should type. If `lantern` is in PATH
  # via the symlink we just made, use the short form; otherwise show the full path
  # so the closing copy is always actionable.
  local cmd="lantern"
  if ! command -v lantern >/dev/null 2>&1; then
    cmd="$bin"
  fi

  printf '\n'
  printf '  %s%s✓ Lantern is installed.%s\n' "$CLR_GREEN" "$CLR_BOLD" "$CLR_RESET"
  printf '\n'
  printf '  %sBinary:%s        %s%s%s\n' "$CLR_DIM" "$CLR_RESET" "$CLR_BOLD" "$bin" "$CLR_RESET"
  printf '  %sLantern home:%s  %s%s%s\n' "$CLR_DIM" "$CLR_RESET" "$CLR_BOLD" "$LANTERN_HOME" "$CLR_RESET"
  printf '\n'
  printf '  %sStart the daemon:%s  %s%s daemon%s\n' "$CLR_DIM" "$CLR_RESET" "$CLR_BOLD" "$cmd" "$CLR_RESET"
  printf '  %sStatus:%s            %s%s info%s\n' "$CLR_DIM" "$CLR_RESET" "$CLR_BOLD" "$cmd" "$CLR_RESET"
  printf '  %sChain head:%s        %s%s chain head%s\n' "$CLR_DIM" "$CLR_RESET" "$CLR_BOLD" "$cmd" "$CLR_RESET"
  printf '  %sRefresh trust:%s     %s%s repair%s\n' "$CLR_DIM" "$CLR_RESET" "$CLR_BOLD" "$cmd" "$CLR_RESET"
  if [[ -n "$token" ]]; then
    printf '\n'
    printf '  %sConnect Curio / Boost:%s\n' "$CLR_DIM" "$CLR_RESET"
    printf "    export FULLNODE_API_INFO='%s:/ip4/127.0.0.1/tcp/1234/http'\n" "$token"
  fi
  printf '\n'
  printf '  %sDocs:%s   %shttps://golantern.io%s\n' "$CLR_DIM" "$CLR_RESET" "$CLR_CYAN" "$CLR_RESET"
  printf '  %sSource:%s %shttps://github.com/Reiers/lantern%s\n' "$CLR_DIM" "$CLR_RESET" "$CLR_CYAN" "$CLR_RESET"
  printf '  %sLogs:%s   tail -f %s/lantern.log\n' "$CLR_DIM" "$CLR_RESET" "$LANTERN_HOME"
  printf '\n'

  # Closing line (rotating).
  local quotes=(
    '"The lighter the node, the brighter the chain."'
    '"Trust the math, not the gateway."'
    '"Five sources agree; the chain has spoken."'
    '"No CGo. No snapshot. No third-party trust."'
  )
  local idx=$((RANDOM % ${#quotes[@]}))
  printf '  %s%s%s\n\n' "$CLR_DIM" "${quotes[$idx]}" "$CLR_RESET"
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
