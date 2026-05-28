#!/usr/bin/env bash
# ─── Lantern installer ───
#   curl -fsSL https://get.golantern.io | bash
#
#   Installs Lantern: downloads a pure-Go Filecoin light node, anchors it
#   to the current F3-finalized chain head via a multi-source quorum,
#   wires it up on PATH, optionally installs it as a background service.
#
#   Safe by default: asks before destructive steps. Idempotent — re-running
#   skips what's already done.
#
#   Environment variables:
#     LANTERN_VERSION       Tag to install (default: latest)
#     LANTERN_HOME          Data + binary directory (default: ~/.lantern)
#     LANTERN_PREFIX        Where to symlink the binary (default: auto-detect)
#     LANTERN_REINSTALL=1   Force re-download even if binary exists
#     LANTERN_REANCHOR=1    Force re-run of the bootstrap quorum
#     LANTERN_YES=1         Non-interactive; assume defaults (background service)
#     LANTERN_NO_SERVICE=1  Skip the OS service installation step
#     LANTERN_BOOTSTRAP_QUORUM   Sources required to agree (default: 5)
#     LANTERN_BOOTSTRAP_TIMEOUT  How long to wait for quorum (default: 90s)
#     LANTERN_PEERS         Comma-separated libp2p multiaddrs for extra trust sources
#     NO_COLOR=1            Disable ANSI colors

set -euo pipefail

# ─── colors + banners ────────────────────────────────────────────────────
if [[ -t 1 ]] && [[ "${TERM:-}" != "dumb" ]] && [[ -z "${NO_COLOR:-}" ]]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; RESET=$'\033[0m'
  BLUE=$'\033[38;5;39m'; CYAN=$'\033[38;5;87m'
  AMBER=$'\033[38;5;215m'; CREAM=$'\033[38;5;230m'
  GREEN=$'\033[38;5;71m'; RED=$'\033[38;5;203m'
  INK=$'\033[38;5;240m'
else
  BOLD=''; DIM=''; RESET=''
  BLUE=''; CYAN=''; AMBER=''; CREAM=''
  GREEN=''; RED=''; INK=''
fi

print_banner() {
  cat <<EOF

${INK}      ┌──┐${RESET}              ${BOLD}${CREAM}Lantern${RESET}
${INK}      ├──┤${RESET}              ${DIM}Pure-Go Filecoin light node.${RESET}
${INK}     ╱│  │╲${RESET}
${INK}    │ │${AMBER}◆◆${INK}│ │${RESET}             ${INK}one-line install · mainnet + calibration${RESET}
${INK}     ╲│  │╱${RESET}
${INK}      ├──┤${RESET}
${INK}      └╴╶┘${RESET}

EOF
}

# Pick a quirky line for the install finale.
lantern_quote() {
  local quotes=(
    "The lighter the node, the brighter the chain."
    "Trust the math, not the gateway."
    "Five sources agree. The chain has spoken."
    "No CGo. No snapshot. No third-party trust."
    "A node small enough to ship inside other programs."
    "BLS, F3, DRAND, IPLD — four anchors, no oracle."
    "Verifies every byte. Holds no opinions."
    "Forty megabytes that argue with the network."
    "One binary. Your keys. Your truth."
  )
  local n=${#quotes[@]}
  local i=$(( $(od -An -N2 -tu2 /dev/urandom 2>/dev/null | tr -d ' ' || echo 0) % n ))
  printf '%s' "${quotes[$i]}"
}

step()  { printf "\n${BOLD}${BLUE}▸${RESET} ${BOLD}%s${RESET}\n" "$*"; }
ok()    { printf "  ${GREEN}✓${RESET} ${DIM}%s${RESET}\n" "$*"; }
warn()  { printf "  ${AMBER}!${RESET} %s\n" "$*"; }
fail()  { printf "\n  ${RED}✗${RESET} ${BOLD}%s${RESET}\n\n" "$*"; exit 1; }
info()  { printf "  ${DIM}%s${RESET}\n" "$*"; }

ask() {
  local prompt="$1" default="${2:-y}" answer
  if [[ "${LANTERN_YES:-}" == "1" ]]; then
    [[ "$default" == "y" ]] && return 0 || return 1
  fi
  if [[ ! -r /dev/tty ]]; then
    [[ "$default" == "y" ]] && return 0 || return 1
  fi
  if [[ "$default" == "y" ]]; then
    printf "  ${CYAN}?${RESET} %s ${DIM}[Y/n]${RESET} " "$prompt"
  else
    printf "  ${CYAN}?${RESET} %s ${DIM}[y/N]${RESET} " "$prompt"
  fi
  read -r answer </dev/tty 2>/dev/null || answer=""
  answer="${answer:-$default}"
  [[ "$answer" =~ ^[Yy]$ ]]
}

# choose "prompt" default options...
# returns the chosen single-letter to stdout.
choose() {
  local prompt="$1"; shift
  local default="$1"; shift
  local answer
  if [[ "${LANTERN_YES:-}" == "1" ]] || [[ ! -r /dev/tty ]]; then
    echo "$default"; return
  fi
  printf "  ${CYAN}?${RESET} %s ${DIM}[default: %s]${RESET} " "$prompt" "$default"
  read -r answer </dev/tty 2>/dev/null || answer=""
  echo "${answer:-$default}"
}

# Spinner for a long-running command. Usage: spinner "Doing the thing" cmd arg1 arg2...
spinner() {
  local label="$1"; shift
  local frames=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
  local tmp; tmp=$(mktemp)
  ("$@" >"$tmp" 2>&1) &
  local pid=$!
  local i=0
  while kill -0 "$pid" 2>/dev/null; do
    printf "\r  ${BLUE}%s${RESET} %s" "${frames[$((i % 10))]}" "$label"
    i=$((i + 1))
    sleep 0.1
  done
  wait "$pid"; local status=$?
  if [[ $status -eq 0 ]]; then
    printf "\r  ${GREEN}✓${RESET} ${DIM}%s${RESET}%*s\n" "$label" 20 ''
    rm -f "$tmp"
  else
    printf "\r  ${RED}✗${RESET} ${BOLD}%s${RESET}\n" "$label"
    cat "$tmp" | sed 's/^/    /'
    rm -f "$tmp"
    exit "$status"
  fi
}

# Spinner with a label that updates as background output arrives.
# Used during bootstrap quorum so we don't dump 10 lines of libp2p noise.
spinner_with_progress() {
  local label="$1"; shift
  local progress_pattern="$1"; shift  # regex to extract progress count from output
  local frames=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
  local tmp; tmp=$(mktemp)
  ("$@" >"$tmp" 2>&1) &
  local pid=$!
  local i=0 count=0
  while kill -0 "$pid" 2>/dev/null; do
    count=$(grep -cE "$progress_pattern" "$tmp" 2>/dev/null || echo 0)
    printf "\r  ${BLUE}%s${RESET} %s  ${DIM}(%s/5 sources agreed)${RESET}" \
      "${frames[$((i % 10))]}" "$label" "$count"
    i=$((i + 1))
    sleep 0.1
  done
  wait "$pid"; local status=$?
  if [[ $status -eq 0 ]]; then
    count=$(grep -cE "$progress_pattern" "$tmp" 2>/dev/null || echo 5)
    printf "\r  ${GREEN}✓${RESET} ${DIM}%s${RESET}  ${DIM}(%s/5 sources agreed)${RESET}%*s\n" \
      "$label" "$count" 20 ''
    rm -f "$tmp"
  else
    printf "\r  ${RED}✗${RESET} ${BOLD}%s${RESET}\n" "$label"
    cat "$tmp" | sed 's/^/    /'
    rm -f "$tmp"
    exit "$status"
  fi
}

# ─── preflight ───────────────────────────────────────────────────────────
print_banner

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

# Tooling check.
have_shasum=0; have_sha256sum=0
command -v shasum    >/dev/null 2>&1 && have_shasum=1
command -v sha256sum >/dev/null 2>&1 && have_sha256sum=1
[[ "$have_shasum" == "1" || "$have_sha256sum" == "1" ]] || fail "Need either shasum or sha256sum for SHA-256 verification"
for cmd in curl tar mktemp; do
  command -v "$cmd" >/dev/null 2>&1 || fail "Missing required command: $cmd"
done
ok "Tools available (curl, tar, sha256)"

LANTERN_HOME="${LANTERN_HOME:-$HOME/.lantern}"
mkdir -p "$LANTERN_HOME"
ok "Data directory: ${BOLD}${LANTERN_HOME}${RESET}"

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
ok "Symlink target: ${BOLD}${LANTERN_PREFIX}/lantern${RESET}"

# ─── download ────────────────────────────────────────────────────────────

sha256_of() {
  if [[ "$have_shasum" == "1" ]]; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

download_binary() {
  step "Downloading Lantern"

  LANTERN_VERSION="${LANTERN_VERSION:-latest}"
  local bin_name="lantern-${OS}-${ARCH}"
  local target="${LANTERN_HOME}/lantern"

  # Idempotence: if a binary already exists, skip download. We still call
  # install_symlink so the PATH link is repaired on re-runs.
  if [[ -x "$target" && "${LANTERN_REINSTALL:-0}" != "1" ]]; then
    if existing_sha=$(sha256_of "$target" 2>/dev/null); then
      ok "Binary present: ${DIM}sha256 ${existing_sha:0:12}…${RESET}  ${DIM}(LANTERN_REINSTALL=1 to force)${RESET}"
      install_symlink "$target"
      return
    fi
  fi

  # Mirror chain. GitHub releases is the canonical source. The dl-lantern.reiers.io
  # mirror is a fallback for users behind networks that block GitHub asset CDN.
  local urls
  if [[ "$LANTERN_VERSION" == "latest" ]]; then
    urls=(
      "https://github.com/Reiers/lantern/releases/latest/download/${bin_name}"
      "https://dl-lantern.reiers.io/latest/${bin_name}"
    )
  else
    urls=(
      "https://github.com/Reiers/lantern/releases/download/${LANTERN_VERSION}/${bin_name}"
      "https://dl-lantern.reiers.io/${LANTERN_VERSION}/${bin_name}"
    )
  fi

  local tmp_dir; tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' EXIT

  local bin_url=""
  local frames=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
  for candidate in "${urls[@]}"; do
    # Show a spinner while curling. We capture the http code separately.
    (curl -fsSL -o "$tmp_dir/$bin_name" -w "%{http_code}" "$candidate" >"$tmp_dir/code" 2>"$tmp_dir/err") &
    local pid=$! i=0
    while kill -0 "$pid" 2>/dev/null; do
      printf "\r  ${BLUE}%s${RESET} Fetching ${DIM}%s${RESET}" "${frames[$((i % 10))]}" "${candidate##*/}"
      i=$((i + 1))
      sleep 0.1
    done
    wait "$pid" || true
    local http_code; http_code=$(<"$tmp_dir/code" 2>/dev/null) || http_code=000
    if [[ "$http_code" == "200" ]] && [[ -s "$tmp_dir/$bin_name" ]]; then
      printf "\r  ${GREEN}✓${RESET} ${DIM}Fetched ${candidate##*/}${RESET}%*s\n" 20 ''
      bin_url="$candidate"
      break
    else
      printf "\r  ${AMBER}!${RESET} ${DIM}Mirror returned HTTP ${http_code:-?}, trying next…${RESET}%*s\n" 10 ''
    fi
  done

  if [[ -z "$bin_url" ]]; then
    warn "No release binary available from any mirror."
    info "Falling back to local source build (requires Go 1.25+)."
    build_from_source "$target"
    return
  fi

  local sha_url="${bin_url}.sha256"
  if curl -fsSL -o "$tmp_dir/$bin_name.sha256" "$sha_url" 2>/dev/null; then
    local expected actual
    expected=$(cut -d' ' -f1 < "$tmp_dir/$bin_name.sha256")
    actual=$(sha256_of "$tmp_dir/$bin_name")
    if [[ "$expected" != "$actual" ]]; then
      fail "SHA-256 mismatch!  expected=$expected  actual=$actual"
    fi
    ok "SHA-256 verified  ${DIM}${expected:0:12}…${RESET}"
  else
    warn "SHA-256 manifest unavailable; skipping integrity check."
  fi

  chmod +x "$tmp_dir/$bin_name"
  mv "$tmp_dir/$bin_name" "$target"
  ok "Installed to ${BOLD}$target${RESET}"

  install_symlink "$target"
}

build_from_source() {
  local target="$1"
  if ! command -v go >/dev/null 2>&1; then
    fail "go not installed and no release binary available. Install Go 1.25+ from https://go.dev/dl"
  fi
  if [[ -f "go.mod" && -d "cmd/lantern" ]]; then
    spinner "Building from $(pwd)" \
      env CGO_ENABLED=0 go build -o "$target" ./cmd/lantern
    chmod +x "$target"
    install_symlink "$target"
    return
  fi
  fail "No release artifact and no source tree found to build from."
}

install_symlink() {
  local target="$1"
  local link="${LANTERN_PREFIX}/lantern"
  local dir; dir="$(dirname "$link")"

  # Make sure the prefix dir exists. If it's the user-local fallback we own it,
  # otherwise we leave it to the user (preflight should already have picked an
  # existing system dir, but defensive).
  if [[ ! -d "$dir" ]]; then
    if [[ "$dir" == "$HOME"* ]]; then
      mkdir -p "$dir"
    else
      warn "$dir does not exist — falling back to \$HOME/.local/bin"
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
      ok "Symlink already in place: ${DIM}${link} → ${target}${RESET}"
      check_path
      return
    fi
  fi

  if [[ -w "$dir" ]]; then
    ln -sf "$target" "$link"
    ok "Symlink: ${DIM}${link} → ${target}${RESET}"
  else
    info "Need sudo to write ${link} (system dir, not user-owned)…"
    if sudo ln -sf "$target" "$link" 2>/dev/null; then
      ok "Symlink: ${DIM}${link} → ${target}${RESET}"
    else
      warn "Could not write to ${dir}. Falling back to user-local install."
      LANTERN_PREFIX="$HOME/.local/bin"
      link="${LANTERN_PREFIX}/lantern"
      mkdir -p "$LANTERN_PREFIX"
      ln -sf "$target" "$link"
      ok "Symlink: ${DIM}${link} → ${target}${RESET}"
    fi
  fi

  check_path
}

check_path() {
  if [[ ":$PATH:" != *":$LANTERN_PREFIX:"* ]]; then
    warn "${BOLD}${LANTERN_PREFIX}${RESET} is not in your PATH."
    info "  Add this to your ~/.zshrc or ~/.bashrc:"
    info "    ${BOLD}export PATH=\"$LANTERN_PREFIX:\$PATH\"${RESET}"
    info "  Then: ${BOLD}exec \$SHELL -l${RESET}"
  fi
}

# ─── trust bootstrap ─────────────────────────────────────────────────────

trust_bootstrap() {
  step "Anchoring to the chain (multi-source quorum)"

  local q="${LANTERN_BOOTSTRAP_QUORUM:-5}"
  local t="${LANTERN_BOOTSTRAP_TIMEOUT:-90s}"
  local extra_peers=""
  if [[ -n "${LANTERN_PEERS:-}" ]]; then
    IFS=',' read -r -a _peers <<< "$LANTERN_PEERS"
    for p in "${_peers[@]}"; do extra_peers+=" --peer $p"; done
  fi

  # If we already have an anchor and the user hasn't asked us to refresh, skip.
  if [[ -s "${LANTERN_HOME}/bootstrap-anchor.json" && "${LANTERN_REANCHOR:-0}" != "1" ]]; then
    local epoch
    epoch=$(grep -oE '"epoch":[[:space:]]*[0-9]+' "${LANTERN_HOME}/bootstrap-anchor.json" | head -1 | grep -oE '[0-9]+')
    ok "Existing anchor at epoch ${BOLD}${epoch:-?}${RESET}  ${DIM}(LANTERN_REANCHOR=1 to refresh)${RESET}"
    return
  fi

  info "Asking $q+ independent sources for the current F3-finalized head."
  info "Refusing to anchor if they disagree."

  local bin="${LANTERN_HOME}/lantern"
  spinner_with_progress \
    "Probing trust sources" \
    '✓ \[' \
    env LANTERN_HOME="$LANTERN_HOME" "$bin" init \
        --bootstrap-quorum="$q" \
        --bootstrap-timeout="$t" \
        --no-wallet $extra_peers || \
    fail "Quorum bootstrap failed. Try 'lantern doctor' for per-source diagnostics."

  if [[ -s "${LANTERN_HOME}/bootstrap-anchor.json" ]]; then
    local epoch
    epoch=$(grep -oE '"epoch":[[:space:]]*[0-9]+' "${LANTERN_HOME}/bootstrap-anchor.json" | head -1 | grep -oE '[0-9]+')
    ok "Anchored at epoch ${BOLD}${epoch}${RESET}"
  fi
}

# ─── wallet ──────────────────────────────────────────────────────────────

wallet_setup() {
  step "Wallet"
  local bin="${LANTERN_HOME}/lantern"
  if "$bin" wallet list 2>/dev/null | grep -q '^\*'; then
    ok "Wallet already configured"
    return
  fi
  # `lantern wallet new` reads its passphrase from stdin. If stdin isn't a TTY
  # (which is the case under `curl ... | bash` AND under `bash install.sh </dev/null`),
  # we cannot prompt safely. Skip wallet creation rather than failing the whole
  # installer; the user can always run `lantern wallet new --type=bls` later in
  # a real terminal.
  if [[ ! -t 0 ]] && [[ -z "${LANTERN_PASS+x}" ]]; then
    info "Stdin is not a terminal; skipping interactive wallet creation."
    info "Create later in a terminal: ${BOLD}lantern wallet new --type=bls${RESET}"
    info "Or non-interactive: ${BOLD}LANTERN_PASS=... lantern wallet new --type=bls${RESET}"
    return
  fi
  if ask "Create a fresh BLS wallet now?" y; then
    if "$bin" wallet new --type=bls 2>&1 | sed 's/^/    /'; then
      ok "Wallet created"
    else
      warn "Wallet creation did not complete. Run later: ${BOLD}lantern wallet new --type=bls${RESET}"
    fi
  else
    info "Skipped. Create later: ${BOLD}lantern wallet new --type=bls${RESET}"
  fi
}

# ─── service ─────────────────────────────────────────────────────────────

service_setup() {
  if [[ "${LANTERN_NO_SERVICE:-}" == "1" ]]; then
    info "LANTERN_NO_SERVICE=1 set; skipping service installation."
    return
  fi
  step "How should Lantern run?"
  info "  ${BOLD}b${RESET}  Background service (launchd / systemd user)"
  info "  ${BOLD}f${RESET}  Foreground (start manually with 'lantern daemon')"
  info "  ${BOLD}s${RESET}  Skip — decide later"
  echo

  local choice
  if [[ "${LANTERN_YES:-}" == "1" ]]; then
    choice="b"
  else
    choice="$(choose 'Choice' f)"
  fi

  case "$choice" in
    b|B|background)
      if spinner "Installing background service" \
           "${LANTERN_HOME}/lantern" service install; then
        ok "Service installed and started"
      fi
      ;;
    f|F|foreground)
      ok "Foreground mode. Start with: ${BOLD}lantern daemon${RESET}"
      ;;
    *)
      info "Skipped. Install later: ${BOLD}lantern service install${RESET}"
      ;;
  esac
}

# ─── closing ─────────────────────────────────────────────────────────────

closing() {
  local bin="${LANTERN_HOME}/lantern"
  local token=""
  if [[ -s "${LANTERN_HOME}/token" ]]; then
    token=$(cat "${LANTERN_HOME}/token")
  fi

  # Use the short command if 'lantern' resolved on PATH; otherwise full path.
  local cmd="lantern"
  if ! command -v lantern >/dev/null 2>&1; then
    cmd="$bin"
  fi

  local rule="────────────────────────────────────────────────────────"
  printf '\n'
  printf '  %s%s%s\n' "$INK" "$rule" "$RESET"
  printf '\n'
  printf '  %s%s🪔 Lantern is ready.%s\n' "$GREEN" "$BOLD" "$RESET"
  printf '\n'
  printf '  %sBinary:%s        %s%s%s\n' "$DIM" "$RESET" "$BOLD" "$bin" "$RESET"
  printf '  %sLantern home:%s  %s%s%s\n' "$DIM" "$RESET" "$BOLD" "$LANTERN_HOME" "$RESET"
  printf '\n'
  printf '  %sNext steps%s\n' "$BOLD" "$RESET"
  printf '  %s•%s  %s%s daemon%s         %sstart the node + open the dashboard%s\n' \
    "$BLUE" "$RESET" "$BOLD" "$cmd" "$RESET" "$DIM" "$RESET"
  printf '  %s•%s  %s%s info%s           %sprint status + FULLNODE_API_INFO%s\n' \
    "$BLUE" "$RESET" "$BOLD" "$cmd" "$RESET" "$DIM" "$RESET"
  printf '  %s•%s  %s%s chain head%s     %squery current head, verified locally%s\n' \
    "$BLUE" "$RESET" "$BOLD" "$cmd" "$RESET" "$DIM" "$RESET"
  printf '  %s•%s  %s%s repair%s         %sre-run trust quorum%s\n' \
    "$BLUE" "$RESET" "$BOLD" "$cmd" "$RESET" "$DIM" "$RESET"

  if [[ -n "$token" ]]; then
    printf '\n'
    printf '  %sConnect Curio / Boost:%s\n' "$DIM" "$RESET"
    printf "    %sexport FULLNODE_API_INFO='%s:/ip4/127.0.0.1/tcp/1234/http'%s\n" "$BOLD" "$token" "$RESET"
  fi
  printf '\n'
  printf '  %sDocs:%s   %shttps://golantern.io%s\n' "$DIM" "$RESET" "$CYAN" "$RESET"
  printf '  %sSource:%s %shttps://github.com/Reiers/lantern%s\n' "$DIM" "$RESET" "$CYAN" "$RESET"
  printf '  %sLogs:%s   %stail -f %s/lantern.log%s\n' "$DIM" "$RESET" "$DIM" "$LANTERN_HOME" "$RESET"
  printf '\n'
  printf '  %s%s%s\n\n' "$DIM" "$(lantern_quote)" "$RESET"
}

# ─── main ────────────────────────────────────────────────────────────────

main() {
  download_binary
  trust_bootstrap
  wallet_setup
  service_setup
  closing
}

main "$@"
