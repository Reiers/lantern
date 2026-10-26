# Building Lantern from source

This guide takes a clean machine to a verified `lantern` binary. It needs no C compiler, no Rust and no `filecoin-ffi`: Lantern is pure Go and builds with `CGO_ENABLED=0`.

When you have a binary, continue with [RUN-PDP.md](RUN-PDP.md) to run it as the chain node for a stock Curio-PDP storage provider.

- [1. Prerequisites](#1-prerequisites)
- [2. Install Go 1.26.4](#2-install-go-1264)
- [3. Clone and check out a release tag](#3-clone-and-check-out-a-release-tag)
- [4. Build](#4-build)
- [5. Verify](#5-verify)
- [6. Optional: check the build against the published release](#6-optional-check-the-build-against-the-published-release)
- [Expected timings](#expected-timings)
- [Alternative: the one-line installer](#alternative-the-one-line-installer)
- [Troubleshooting](#troubleshooting)

## 1. Prerequisites

| | |
|---|---|
| OS | Linux x86_64 (amd64) or arm64, or macOS (Apple silicon or Intel) |
| Tools | `git`, `curl`, `tar`, CA certificates |
| Disk | about 1 GB for the Go toolchain plus about 1 GB for the module cache and build cache |
| Memory | 4 GB is comfortable for the build |
| Network | outbound HTTPS to `go.dev`, `github.com` and `proxy.golang.org` |

On a fresh Debian or Ubuntu machine:

```sh
sudo apt-get update && sudo apt-get install -y git curl ca-certificates
```

On macOS, `git` comes with the Xcode command line tools (`xcode-select --install`); `curl` is preinstalled.

## 2. Install Go 1.26.4

Release binaries are built with **Go 1.26.4** (see `GO_VERSION` in `.github/workflows/release.yml`). Use exactly this version if you want to compare your binary with the published one.

Pick the tarball for your platform:

| Platform | Tarball |
|---|---|
| Linux x86_64 | `go1.26.4.linux-amd64.tar.gz` |
| Linux arm64 | `go1.26.4.linux-arm64.tar.gz` |
| macOS Apple silicon | `go1.26.4.darwin-arm64.tar.gz` |
| macOS Intel | `go1.26.4.darwin-amd64.tar.gz` |

**Without sudo** (installs into your home directory):

```sh
GO_TARBALL=go1.26.4.linux-amd64.tar.gz      # pick yours from the table
mkdir -p "$HOME/.local"
curl -fsSL "https://go.dev/dl/$GO_TARBALL" | tar -C "$HOME/.local" -xz
export PATH="$HOME/.local/go/bin:$PATH"     # add this line to ~/.bashrc or ~/.zshrc
go version                                   # go version go1.26.4 linux/amd64
```

**System-wide** (needs sudo):

```sh
GO_TARBALL=go1.26.4.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
curl -fsSL "https://go.dev/dl/$GO_TARBALL" | sudo tar -C /usr/local -xz
export PATH="/usr/local/go/bin:$PATH"
go version
```

If you already have Go 1.21 or newer, `go` reads the `toolchain go1.26.4` line in `go.mod` and downloads 1.26.4 for you on the first build (unless `GOTOOLCHAIN=local` is set). Installing 1.26.4 directly is more predictable.

## 3. Clone and check out a release tag

Build a release tag, not `main`. Find the newest one at <https://github.com/Reiers/lantern/releases/latest> (release candidates such as `v1.10.1-rc1` are listed as pre-releases on <https://github.com/Reiers/lantern/releases>).

```sh
git clone https://github.com/Reiers/lantern.git
cd lantern
TAG=v1.10.1-rc1                              # replace with the tag you want
git checkout "$TAG"
```

<!-- TODO-VERIFY: set TAG to the release the M2 operator should build. It must include NV29 (Solstice, PR #172) and the bridge-off receipt fix; v1.10.0 does NOT work with Curio-PDP on calibration or mainnet since NV29. -->

> **Network upgrade note.** Calibration (epoch 4109133) and mainnet (epoch 6470279) run network version 29 (Solstice, builtin-actors v19). Lantern v1.10.0 and older predate it and serve FEVM contracts as "no code", so Curio-PDP fails. Use a release that lists NV29 support in [CHANGELOG.md](CHANGELOG.md).

## 4. Build

This is the exact command the release workflow runs (same flags, same `versionTag`):

```sh
CGO_ENABLED=0 go build   -trimpath   -ldflags "-s -w -X main.versionTag=$TAG"   -o lantern   ./cmd/lantern
```

The first build downloads all Go modules (most of the time is spent here and in compiling the dependency graph). The result is a single static binary of about 43 MB.

Put it on your PATH:

```sh
mkdir -p "$HOME/.local/bin"
install -m 0755 lantern "$HOME/.local/bin/lantern"
export PATH="$HOME/.local/bin:$PATH"         # if not already on PATH
```

## 5. Verify

```sh
lantern version
# lantern v1.10.1-rc1 (<12-char commit>) Lantern+mainnet

go version -m "$(command -v lantern)" | grep -E 'go1|vcs.revision|vcs.modified|CGO_ENABLED|-trimpath'
```

- The version must match `$TAG` and the commit must match `git rev-parse --short=12 HEAD`.
- `vcs.modified=false` means you built a clean checkout.
- `Lantern+mainnet` is only the compiled-in default. Every command takes `--network mainnet|calibration` at run time.

`lantern help` lists every command.

## 6. Optional: check the build against the published release

Lantern release binaries are meant to be reproducible: the same tag, Go version and flags should give the same bytes. The procedure, and a script that does the comparison for you, live in [docs/REPRODUCIBLE-BUILDS.md](docs/REPRODUCIBLE-BUILDS.md).

A quick manual comparison (Linux x86_64 shown):

```sh
curl -fsSLO "https://github.com/Reiers/lantern/releases/download/$TAG/lantern-linux-amd64.sha256"
echo "published: $(awk '{print $1}' lantern-linux-amd64.sha256)"
echo "local:     $(sha256sum lantern | awk '{print $1}')"     # macOS: shasum -a 256 lantern
```

Compare only the hash column: the `.sha256` file names the CI path (`dist/lantern-linux-amd64`), so `sha256sum -c` against a differently named local file reports "No such file".

<!-- TODO-VERIFY: during the internal dry run the v1.10.0 linux-amd64 clean build did NOT match the published hash although the embedded build info was identical (friction F1, being fixed in a separate PR). Update this section once docs/REPRODUCIBLE-BUILDS.md and scripts/repro-build.sh land, and record the result in docs/VERIFICATION-M2.md. -->

## 7. First run

Every command takes `--network mainnet|calibration` (default mainnet).

```sh
LANTERN_PASS='' lantern init --network mainnet --no-wallet
lantern daemon --network mainnet
```

`init` writes the trust anchor only when at least 3 distinct operators agree on the current F3 finality and no source disagrees. **`lantern daemon` refuses to start without that anchor**; run `lantern init` (or `lantern repair`, which re-anchors from a fresh quorum) first. If `init` fails it prints which sources failed, how many operators answered, and the flags that help (`--peer <url>`, `--no-libp2p`, `--bootstrap-quorum N`); `lantern doctor --network <net>` re-runs the probe read-only. `--allow-rpc-anchor` lets the daemon boot from a weaker gateway+RPC head instead, with a warning. See [TRUST-MODEL.md](TRUST-MODEL.md) section 2.9.

## Expected timings

Measured on a clean `ubuntu:24.04` container on an 8-core / 16-thread Xeon E5-2667 v2 with a home fibre connection:

| Step | Time |
|---|---|
| `apt-get install git curl ca-certificates` | 21 s |
| Download and unpack Go 1.26.4 | 9 s |
| `git clone` + checkout | 2 s |
| `go build` (cold module and build cache) | 52 s |
| **Total** | **about 85 s** |

Rebuilds with a warm cache take a few seconds. Slower CPUs or connections scale the build step roughly linearly.

## Alternative: the one-line installer

If you do not need to build from source, the installer downloads the latest stable release binary for your platform, checks its SHA-256, anchors it with the multi-source quorum and offers to install a background service:

```sh
curl -fsSL https://get.golantern.io | bash
```

Useful variables: `LANTERN_VERSION=<tag>` (for example a release candidate; default is the latest stable release), `LANTERN_NODE_TYPE=pdp`, `LANTERN_NO_SERVICE=1`, `LANTERN_YES=1` (non-interactive). The installer anchors **mainnet** only. For the calibration Curio-PDP walkthrough, install with `LANTERN_NO_SERVICE=1` and then follow [RUN-PDP.md](RUN-PDP.md) from step 3 (`lantern init --network calibration`).

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `go: command not found` | `PATH` does not include `$HOME/.local/go/bin` (or `/usr/local/go/bin`). Export it and open a new shell. |
| `go: downloading go1.26.4` then a network error | An older Go tried to fetch the pinned toolchain. Install 1.26.4 directly (step 2) or allow access to `proxy.golang.org`. |
| `lantern version` prints `dev` | You built without `-ldflags "... -X main.versionTag=$TAG"`. Rebuild with the exact command in step 4. |
| `go version -m` shows `vcs.modified=true` | The checkout has local changes. `git status`, then `git stash` or a fresh clone. |
| `lantern daemon` exits with `no trust anchor` | Run `lantern init --network <net>` (or `lantern repair --network <net>`) with the same `LANTERN_HOME`. If the quorum fails, follow its printed hints or run `lantern doctor --network <net>` (step 7). |
| `sha256sum -c` says "No such file or directory" | The published `.sha256` names `dist/lantern-<os>-<arch>`. Compare the hash column directly (step 6). |
| Local hash differs from the published hash | Check Go version, flags, tag and `vcs.modified` first, then follow [docs/REPRODUCIBLE-BUILDS.md](docs/REPRODUCIBLE-BUILDS.md). If it still differs, file an [operator report](https://github.com/Reiers/lantern/issues/new?template=operator-report.yml). |
