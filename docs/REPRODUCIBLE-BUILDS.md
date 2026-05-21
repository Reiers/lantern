# Reproducible builds

Lantern's V1.2 GA promise is that anyone with a clean checkout at a
release tag can produce a byte-identical binary to the one published on
GitHub Releases. This document is the recipe.

## The promise

Given:

- Go toolchain version pinned to `1.25.7` (see `.github/workflows/release.yml`)
- Module versions pinned via `go.sum`
- `CGO_ENABLED=0` (no native code)
- `-trimpath` (strips host path prefixes)
- `-ldflags "-s -w -X main.versionTag=<TAG>"` (strips debug info, embeds tag)

…the resulting binary at every supported (GOOS, GOARCH) pair is
bit-identical to the one published on the GitHub release page. We
publish SHA-256 manifests alongside each artifact so any third party
can independently verify the reproduction.

## How to reproduce

```sh
# 1. Check out the exact tag.
git clone https://github.com/Reiers/lantern.git
cd lantern
git checkout v1.2.0   # or whichever release you want to verify

# 2. Install the pinned Go toolchain.
#    (use https://go.dev/dl or your distro's Go 1.25.7)
go version
# expected: go version go1.25.7 ...

# 3. Build with the exact flags the release workflow uses.
CGO_ENABLED=0 \
GOOS=darwin \
GOARCH=arm64 \
go build \
  -trimpath \
  -ldflags "-s -w -X main.versionTag=v1.2.0" \
  -o lantern-darwin-arm64 \
  ./cmd/lantern

# 4. Compare against the published SHA-256.
shasum -a 256 lantern-darwin-arm64
curl -fsSL https://github.com/Reiers/lantern/releases/download/v1.2.0/lantern-darwin-arm64.sha256
# The two SHA-256 values must match.
```

## Why this works

- **`CGO_ENABLED=0`**: no native code means no toolchain-version
  variability from clang/gcc/glibc/musl.
- **`-trimpath`**: removes the absolute path of your working tree from
  the binary, so `/home/alice/code/lantern` and `/Users/bob/lantern`
  produce identical bytes.
- **`-ldflags "-s -w"`**: strips debug info and symbol tables that may
  differ in unimportant ways (line tables, etc).
- **Pinned Go version**: different Go versions can emit different code
  for the same source. The version stays pinned for an entire release
  cycle.
- **`go.sum`**: every imported module's content hash is recorded, so
  module proxy compromises can't slip in different code.

## Caveats

- The source tarball published alongside each release
  (`lantern-vX.Y.Z-source.tar.gz`) is produced by `git archive`, which
  is byte-deterministic when invoked on the same commit. Verify with
  `sha256sum` against the published `.sha256`.
- Macros that read env-time at compile time (e.g. `time.Now()` baked
  into a `var BuildTime = time.Now()`) would break reproducibility. The
  Lantern code base explicitly avoids these; if you see one in a PR,
  reject it.
- Linker resolution order can in rare cases be sensitive to the host
  Go module cache layout. If you see a discrepancy and the build flags
  match, please open an issue with both `sha256sum`s and your Go
  build environment (`go env`).

## Why we care

The Lantern threat model assumes the build pipeline is one of two
trust roots (the other being the multi-source bootstrap quorum, see
[INSTALLER-SPEC.md §3](../INSTALLER-SPEC.md)). Reproducible builds let
any third party independently verify that the binary distributed via
GitHub Releases is the same code that's in the git tag. This means:

- A compromised GitHub Releases page can't ship a backdoored binary
  for long: the first reproducer who can't match the published SHA-256
  raises the alarm.
- The bootstrap quorum's trust foundation extends back to "you can read
  the source," not "you trust the maintainer."

## Release process

The repository's `.github/workflows/release.yml` runs on every
`v*` tag push:

1. Build all four target platforms with the exact flags above.
2. Emit `.sha256` for each binary.
3. Produce the deterministic source tarball.
4. Publish the GitHub release with all artifacts attached.

The release workflow is itself part of the source tree, so a release
audit can verify that the published binaries match what the workflow
would have produced for the same tag.
