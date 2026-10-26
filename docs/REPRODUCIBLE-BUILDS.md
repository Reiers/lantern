# Reproducible builds

Anyone with git and docker should be able to take a Lantern release tag,
rebuild it, and get byte-identical binaries to the ones published on
GitHub Releases. This document describes the recipe, the one-command
script that runs it, and exactly what is and is not guaranteed.

## Status

| Releases | Byte-reproducible with this recipe? |
|---|---|
| The first release tagged after this document landed (the next `v1.x.y` or `v1.x.y-rcN`), and every release after it | **Yes.** The release workflow builds with exactly `scripts/repro-build.sh`, and refuses to publish unless an amd64-hosted and an arm64-hosted build agree byte-for-byte. |
| v1.10.0 and earlier | **No**, not with the recipe in this document. See [Older releases](#older-releases) for why and for the narrow way v1.9.0 to v1.10.0 can still be checked. |

## Verify a release (one command)

Requirements: bash, git, curl, and docker (Docker Engine on Linux, Docker
Desktop or colima on macOS). Any host architecture works. You do not need
Go installed.

```sh
git clone https://github.com/Reiers/lantern.git
cd lantern
./scripts/repro-build.sh v1.x.y linux/amd64
```

Output looks like:

```
repro-build: tag      v1.x.y
repro-build: commit   <40-hex commit of the tag>
repro-build: image    golang:1.26.4@sha256:f96cc555...
repro-build: targets  linux/amd64
lantern-linux-amd64    computed   <sha256>
                       published  <sha256>
                       result     MATCH
```

The script exits `0` if every requested target matches its published
`.sha256`, `1` on any mismatch (or a missing manifest), and `2` on usage
or build errors. Omit the target to check all release targets
(`linux/amd64 linux/arm64 darwin/arm64`). Run `--help` for the options
(`--out`, `--source`, `--no-verify`, `--image`, `--gopath`).

The script clones the tag from GitHub by itself; the checkout you run it
from only provides the script. Use `--source .` to build the commit
checked out locally instead (it must be the tag unless `--no-verify`).

On macOS with colima, `--out` (default `./repro-out`) must be under your
home directory, because colima only shares `$HOME` with the VM.

## Verify a download without rebuilding

Each binary has a `.sha256` manifest next to it, in standard
`sha256sum` format with just the asset filename:

```sh
curl -fsSLO https://github.com/Reiers/lantern/releases/download/v1.x.y/lantern-linux-amd64
curl -fsSLO https://github.com/Reiers/lantern/releases/download/v1.x.y/lantern-linux-amd64.sha256
sha256sum -c lantern-linux-amd64.sha256      # macOS: shasum -a 256 -c ...
```

This only checks that the download is intact and matches the manifest
published in the same place. The rebuild above is what ties the bytes
to the source.

## The recipe

Everything is in `scripts/repro-build.sh` (that file is the source of
truth; this is a summary):

- **Toolchain:** `golang:1.26.4` container, pinned by multi-arch index digest
  `sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609`.
  `GOTOOLCHAIN=local`, and the script aborts if `go env GOVERSION` is not
  `go1.26.4`, so the toolchain can never auto-switch.
- **Source:** a fresh `git clone` of the tagged commit, mounted read-only at
  `/src`. Building from a fresh clone keeps `vcs.modified=false` in the
  embedded build info no matter what is lying around in your working tree.
- **Module cache:** `GOPATH=/go`, so `GOMODCACHE=/go/pkg/mod` (see below for
  why this must be a fixed path). Modules come from `proxy.golang.org` and
  are checked against `go.sum` and `sum.golang.org`.
- **Environment:** `CGO_ENABLED=0`, `GOFLAGS=` (empty), `GOEXPERIMENT=` (empty),
  `GOFIPS140=off`, `GOAMD64=v1`, `GOARM64=v8.0`.
- **Command,** per target:

  ```sh
  GOOS=<os> GOARCH=<arch> go build -trimpath -buildvcs=true \
    -ldflags "-s -w -X main.versionTag=<tag>" \
    -o lantern-<os>-<arch> ./cmd/lantern
  ```

- **Manifest:** `sha256sum lantern-<os>-<arch> > lantern-<os>-<arch>.sha256`,
  run in the output directory so the manifest holds only the filename.

All targets, including `darwin/arm64`, are cross-compiled from Linux. That
is safe because the build is pure Go (no cgo, internal linker, and the Go
linker writes the ad-hoc macOS code signature itself), and was checked:
a Linux cross-compile of v1.10.0 is byte-identical to the published
macOS-runner build once the module cache path matches.

### Why the module cache path matters

Go's `-trimpath` rewrites the directory of each package being built, but
it does not rewrite assembler `#include` paths that point outside that
package's directory. `github.com/consensys/gnark-crypto` (pulled in for
BLS12-381) includes shared `.s` files from a sibling directory, so two
absolute paths such as

```
/home/runner/go/pkg/mod/github.com/consensys/gnark-crypto@v0.20.1/field/asm/element_4w/element_4w_amd64.s
```

end up in the binary's `.gopclntab`. A different `GOMODCACHE` changes the
length of those strings, which shifts everything laid out after
`.gopclntab` and changes millions of bytes, while `go version -m` output
stays identical. Pinning the cache path inside the container removes the
problem for every operator.

### How the release workflow uses it

`.github/workflows/release.yml` on a `v*` tag push:

1. Runs `scripts/repro-build.sh --source . --no-verify <tag>` for all targets
   on an `ubuntu-24.04` (amd64) runner **and** on an `ubuntu-24.04-arm` runner.
   The two runners use different native builds of the same Go toolchain.
2. `cross-check` compares the two sets with `cmp` and runs `sha256sum -c`
   on every manifest. Any difference fails the release.
3. Builds the source tarball with `git archive`.
4. Publishes the amd64-hosted binaries, manifests and tarball.

Pull requests that change the workflow, the script, `go.mod` or `go.sum` run
steps 1 to 3 as a dry run (with version string `v0.0.0-dryrun`) and never
publish.

## What is guaranteed, and what is not

Guaranteed (for releases built with this recipe):

- Rebuilding a tag with `scripts/repro-build.sh` on any linux/amd64,
  linux/arm64 or macOS (Apple silicon or Intel, via docker) machine
  produces the published bytes for every release target.
- The published binary embeds `vcs.revision` of the tag and
  `vcs.modified=false` (check with `go version -m lantern-linux-amd64`).

Not guaranteed:

- **A plain `go build` outside the container.** It will match only if you
  reproduce every input, most importantly Go 1.26.4 exactly and
  `GOMODCACHE=/go/pkg/mod`. Your usual `~/go/pkg/mod` gives different bytes
  (see above). The container is the supported path.
- **Other Go versions.** Any other toolchain, including other 1.26.x patch
  releases, produces different code. The pin moves only in a dedicated
  PR; the image digest that built a release is the one in
  `scripts/repro-build.sh` at that tag.
- **Trust in the toolchain itself.** Reproducing the bytes shows the
  binary was built from the tagged source with the pinned
  `golang:1.26.4` image. It does not prove that image (or the Go
  distribution inside it) is honest. You can rebuild with a toolchain you
  trust more via `--image`; it must still be exactly Go 1.26.4.
- **The source tarball across git versions.** `git archive --format=tar.gz`
  is deterministic for a given commit and git version; a very different
  git may compress differently. The tarball is a convenience, not part of
  the binary reproducibility claim.
- **Dependencies being benign.** `go.sum` and the checksum database pin
  module content; they say nothing about whether that content is good.

## Older releases

v1.10.0 and earlier were built by a workflow that ran `go build` directly on
GitHub-hosted runners (via `actions/setup-go`), so their bytes depend on
the runners' module cache paths, `/home/runner/go/pkg/mod` on Linux and
`/Users/runner/go/pkg/mod` on macOS. Rebuilding them with the recipe
above, or with the plain `go build` this document used to describe, gives
a different SHA-256 than the published assets. Example for v1.10.0
`lantern-linux-amd64`: published
`72169c8084e66ec5c0eab41902a15e7b740c10e4873a6099e83fb1d02b7d097a`, rebuilt in a
clean ubuntu:24.04 container with `/root/go/pkg/mod`
`d5c0660850790547d29d2e00c87909a1afbdf8030196e984720aeb0622521cf3`.

For releases built with Go 1.26.4 by that workflow (v1.9.0 through
v1.10.0, including their release candidates) you can still reproduce the
published bytes by recreating the runner path inside the container:

```sh
./scripts/repro-build.sh --gopath /home/runner/go  v1.10.0 linux/amd64 linux/arm64
./scripts/repro-build.sh --gopath /Users/runner/go v1.10.0 darwin/arm64
```

This was done for v1.10.0 and all three binaries matched. v1.9.0 to
v1.9.2 and the v1.10.0 release candidates used the same workflow and
Go 1.26.4 and should match the same way, but have not been checked
one by one. Releases
before v1.9.0 used Go 1.25.x and are not covered by the pinned image;
they have to be checked with the matching toolchain via `--image`, and
with the old `dist/` path in their `.sha256` files a plain `sha256sum -c`
will not find the file (compare the hash by eye or with `cut -d' ' -f1`).

## Why we care

The Lantern threat model treats the build pipeline as one of two trust
roots (the other is the multi-source bootstrap quorum). Reproducible
builds let any third party check that the binary on GitHub Releases is
the code in the git tag:

- A compromised release page or CI run can not quietly ship a different
  binary: the first person whose rebuild does not match raises the alarm,
  and the release workflow itself already requires two independent hosts
  to agree.
- Trust extends back to "you can read the source," not "you trust the
  maintainer."

If your rebuild of a release made with this recipe does not match, open
an issue with the script output, `docker version`, and your host OS and
architecture.
