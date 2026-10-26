#!/usr/bin/env bash
# scripts/repro-build.sh
#
# Rebuild Lantern release binaries in the pinned container toolchain and
# compare them byte-for-byte against the SHA-256 manifests published on
# the GitHub release. This is the same recipe .github/workflows/release.yml
# uses to produce the published binaries, so a match shows the published
# bytes came from the tagged source.
#
#   ./scripts/repro-build.sh v1.11.0 linux/amd64
#   ./scripts/repro-build.sh v1.11.0 linux/amd64 linux/arm64 darwin/arm64
#
# Requirements: bash, git, curl, docker (Docker Engine, Docker Desktop or
# colima; any host architecture). No local Go installation is needed.
#
# Exit codes: 0 all targets match, 1 at least one mismatch or missing
# published manifest, 2 usage or build error.
#
# See docs/REPRODUCIBLE-BUILDS.md for what this does and does not prove.

set -euo pipefail

# ---------------------------------------------------------------------------
# Pinned recipe. Changing anything in this block changes the output bytes,
# so treat it as a release-engineering change and bump it in its own PR.
# ---------------------------------------------------------------------------
# golang:1.26.4 multi-arch index (linux/amd64, linux/arm64, ...). Pinned by
# digest so a re-pushed tag can not silently change the toolchain.
DEFAULT_IMAGE="golang:1.26.4@sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609"
GO_VERSION_EXPECTED="go1.26.4"
# GOPATH (and therefore GOMODCACHE) inside the container. It MUST be a fixed
# absolute path: -trimpath does not rewrite assembler #include paths that
# point outside the including package's directory, and
# github.com/consensys/gnark-crypto includes shared .s files from a sibling
# directory. Those absolute module cache paths end up in .gopclntab, so a
# different GOMODCACHE gives different bytes. /go is the golang image's own
# GOPATH.
CONTAINER_GOPATH="/go"
CONTAINER_SRC="/src"
RELEASE_TARGETS=(linux/amd64 linux/arm64 darwin/arm64)
REPO_URL="${LANTERN_REPO_URL:-https://github.com/Reiers/lantern.git}"
RELEASE_BASE="${LANTERN_RELEASE_BASE:-https://github.com/Reiers/lantern/releases/download}"

usage() {
  cat <<'EOF'
Usage: scripts/repro-build.sh [options] <tag> [os/arch ...]

Rebuilds the Lantern binary for <tag> in the pinned golang container and
compares it with the published <asset>.sha256 from the GitHub release.
If no os/arch is given, all release targets are built:
  linux/amd64 linux/arm64 darwin/arm64

Options:
  --source DIR   Build the commit checked out in the local git repository
                 DIR instead of cloning <tag> from GitHub. Uncommitted
                 changes are ignored (the commit is cloned). Unless
                 --no-verify is set, HEAD must be <tag>.
  --out DIR      Where to write binaries and .sha256 files
                 (default: ./repro-out). Must be shareable with docker
                 (with colima: somewhere under your home directory).
  --no-verify    Do not compare with published manifests; only build and
                 write <asset> and <asset>.sha256 (the release workflow
                 uses this).
  --image REF    Override the container image (default: pinned digest).
                 Output only matches if the toolchain is identical.
  --gopath PATH  Override the container GOPATH (default: /go). Only useful
                 for checking releases made before the pinned recipe; see
                 "Older releases" in docs/REPRODUCIBLE-BUILDS.md.
  -h, --help     Show this help.

Environment:
  DOCKER_DEFAULT_PLATFORM  e.g. linux/amd64 to force an (emulated) toolchain
                           architecture. Not required: amd64-hosted and
                           arm64-hosted toolchains produce identical bytes.
EOF
}

die() { echo "repro-build: $*" >&2; exit 2; }

SOURCE_DIR=""
OUT_DIR="./repro-out"
VERIFY=1
IMAGE="$DEFAULT_IMAGE"
GOPATH_OVERRIDE=""
POSITIONAL=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source) [[ $# -ge 2 ]] || die "--source needs a directory"; SOURCE_DIR="$2"; shift 2 ;;
    --out) [[ $# -ge 2 ]] || die "--out needs a directory"; OUT_DIR="$2"; shift 2 ;;
    --no-verify) VERIFY=0; shift ;;
    --image) [[ $# -ge 2 ]] || die "--image needs a reference"; IMAGE="$2"; shift 2 ;;
    --gopath) [[ $# -ge 2 ]] || die "--gopath needs a path"; GOPATH_OVERRIDE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    --) shift; POSITIONAL+=("$@"); break ;;
    -*) die "unknown option: $1 (see --help)" ;;
    *) POSITIONAL+=("$1"); shift ;;
  esac
done

[[ ${#POSITIONAL[@]} -ge 1 ]] || { usage >&2; exit 2; }
TAG="${POSITIONAL[0]}"
TARGETS=("${POSITIONAL[@]:1}")
[[ ${#TARGETS[@]} -gt 0 ]] || TARGETS=("${RELEASE_TARGETS[@]}")

[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || die "tag must look like v1.2.3 or v1.2.3-rc1, got: $TAG"
for t in "${TARGETS[@]}"; do
  [[ "$t" =~ ^[a-z0-9]+/[a-z0-9]+$ ]] || die "target must look like os/arch, got: $t"
done
if [[ -n "$GOPATH_OVERRIDE" ]]; then
  [[ "$GOPATH_OVERRIDE" == /* ]] || die "--gopath must be absolute"
  CONTAINER_GOPATH="$GOPATH_OVERRIDE"
fi

for bin in git docker; do
  command -v "$bin" >/dev/null 2>&1 || die "$bin is required"
done
if [[ "$VERIFY" == 1 ]]; then
  command -v curl >/dev/null 2>&1 || die "curl is required (or pass --no-verify)"
fi
docker info >/dev/null 2>&1 || die "docker daemon is not reachable"

mkdir -p "$OUT_DIR"
OUT_DIR="$(cd "$OUT_DIR" && pwd -P)"
WORK="$OUT_DIR/.repro-work.$$"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT
mkdir -p "$WORK"

# ---------------------------------------------------------------------------
# 1. Get a pristine checkout. Always build from a fresh clone so untracked
#    files, local edits or the --out directory can never flip vcs.modified
#    in the embedded build info.
# ---------------------------------------------------------------------------
SRC="$WORK/src"
if [[ -n "$SOURCE_DIR" ]]; then
  git -C "$SOURCE_DIR" rev-parse --git-dir >/dev/null 2>&1 || die "--source $SOURCE_DIR is not a git repository"
  COMMIT="$(git -C "$SOURCE_DIR" rev-parse HEAD)"
  if [[ "$VERIFY" == 1 ]]; then
    TAG_COMMIT="$(git -C "$SOURCE_DIR" rev-parse -q --verify "refs/tags/$TAG^{commit}" || true)"
    [[ -n "$TAG_COMMIT" ]] || die "tag $TAG not found in $SOURCE_DIR (git fetch --tags?)"
    [[ "$TAG_COMMIT" == "$COMMIT" ]] || die "HEAD ($COMMIT) is not $TAG ($TAG_COMMIT); check out the tag or use --no-verify"
  fi
  if [[ -n "$(git -C "$SOURCE_DIR" status --porcelain --untracked-files=no)" ]]; then
    echo "repro-build: note: $SOURCE_DIR has uncommitted changes; building committed HEAD only" >&2
  fi
  git clone --quiet --no-checkout "$SOURCE_DIR" "$SRC"
  git -C "$SRC" -c advice.detachedHead=false checkout --quiet "$COMMIT"
else
  # (git warns that an annotated tag "is not a commit"; that is expected,
  # so stderr is only shown on failure.)
  if ! git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$TAG" "$REPO_URL" "$SRC" 2>"$WORK/clone.log"; then
    cat "$WORK/clone.log" >&2
    die "could not clone $TAG from $REPO_URL"
  fi
  COMMIT="$(git -C "$SRC" rev-parse HEAD)"
fi
[[ -z "$(git -C "$SRC" status --porcelain)" ]] || die "fresh clone is not clean (unexpected)"

echo "repro-build: tag      $TAG"
echo "repro-build: commit   $COMMIT"
echo "repro-build: image    $IMAGE"
echo "repro-build: targets  ${TARGETS[*]}"

# ---------------------------------------------------------------------------
# 2. Build inside the pinned container. Every variable that can influence
#    the output is set explicitly. docker does not forward the caller's
#    environment, so GOFLAGS and friends from the host can not leak in.
# ---------------------------------------------------------------------------
# shellcheck disable=SC2016  # expanded inside the container, not here
BUILD_SCRIPT='
set -eu
got="$(go env GOVERSION)"
if [ "$got" != "$GO_VERSION_EXPECTED" ]; then
  echo "repro-build: container has $got, expected $GO_VERSION_EXPECTED" >&2
  exit 2
fi
mkdir -p "$GOMODCACHE"
cd "$SRC_DIR"
for t in $TARGETS; do
  os="${t%/*}"; arch="${t#*/}"
  name="lantern-${os}-${arch}"
  GOOS="$os" GOARCH="$arch" go build \
    -trimpath \
    -buildvcs=true \
    -ldflags "-s -w -X main.versionTag=${TAG}" \
    -o "/out/${name}" \
    ./cmd/lantern
  (cd /out && sha256sum "$name" > "${name}.sha256")
  chown "$HOST_UID:$HOST_GID" "/out/${name}" "/out/${name}.sha256"
done
'

# Runs as root inside the container (so any --gopath can be created); the
# outputs are chowned back to the calling user.
docker run --rm \
  -v "$SRC:$CONTAINER_SRC:ro" \
  -v "$OUT_DIR:/out" \
  -e HOME=/tmp \
  -e GOCACHE=/tmp/gocache \
  -e GOPATH="$CONTAINER_GOPATH" \
  -e GOMODCACHE="$CONTAINER_GOPATH/pkg/mod" \
  -e GOTOOLCHAIN=local \
  -e GOFLAGS= \
  -e GOEXPERIMENT= \
  -e GOFIPS140=off \
  -e GOAMD64=v1 \
  -e GOARM64=v8.0 \
  -e CGO_ENABLED=0 \
  -e GOPROXY=https://proxy.golang.org,direct \
  -e GOSUMDB=sum.golang.org \
  -e GIT_CONFIG_COUNT=1 \
  -e GIT_CONFIG_KEY_0=safe.directory \
  -e GIT_CONFIG_VALUE_0="$CONTAINER_SRC" \
  -e GO_VERSION_EXPECTED="$GO_VERSION_EXPECTED" \
  -e SRC_DIR="$CONTAINER_SRC" \
  -e TAG="$TAG" \
  -e HOST_UID="$(id -u)" \
  -e HOST_GID="$(id -g)" \
  -e TARGETS="${TARGETS[*]}" \
  "$IMAGE" \
  sh -c "$BUILD_SCRIPT" \
  || die "container build failed"

# ---------------------------------------------------------------------------
# 3. Report and (unless --no-verify) compare with the published manifests.
# ---------------------------------------------------------------------------
fail=0
for t in "${TARGETS[@]}"; do
  name="lantern-${t%/*}-${t#*/}"
  computed="$(cut -d' ' -f1 < "$OUT_DIR/$name.sha256")"
  printf '%-22s computed   %s\n' "$name" "$computed"
  [[ "$VERIFY" == 1 ]] || continue
  published=""
  if manifest="$(curl -fsSL "$RELEASE_BASE/$TAG/$name.sha256" 2>/dev/null)"; then
    published="$(printf '%s\n' "$manifest" | head -n1 | cut -d' ' -f1)"
  fi
  printf '%-22s published  %s\n' "" "${published:-<not found>}"
  if [[ -z "$published" ]]; then
    printf '%-22s result     MISSING (no published manifest for %s)\n' "" "$TAG"
    fail=1
  elif [[ "$computed" == "$published" ]]; then
    printf '%-22s result     MATCH\n' ""
  else
    printf '%-22s result     MISMATCH\n' ""
    fail=1
  fi
done

echo "repro-build: binaries and manifests in $OUT_DIR"
exit "$fail"
