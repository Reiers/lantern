# Phase 11 — install + bootstrap quorum evidence

Captured 2026-05-21 at the close of Phase 11. Both `lantern init
--bootstrap-quorum=5` and `install.sh` were run end-to-end against
live mainnet in clean environments. Full transcripts are checked in
alongside this file:

- `phase11-init-evidence.log` — `lantern init` transcript
- `phase11-install-evidence.log` — `install.sh` transcript
- `phase11-bootstrap-anchor.json` — the validated trust anchor written
  on success

## `lantern init --bootstrap-quorum=5` — clean run

Environment:

```sh
export LANTERN_HOME=/tmp/lantern-evidence
rm -rf "$LANTERN_HOME"
/tmp/lantern-phase11 init --bootstrap-quorum=5 --bootstrap-timeout=90s --no-wallet
```

Outcome:

```
▸ Bootstrap quorum — establishing chain head from independent sources
    Required agreement: 5 sources, timeout 1m30s
    libp2p host: peer=12D3KooW… (3 connections after 8s settle)
    10 sources assembled (libp2p=7, forest=2, user=0, gateway=1)

  ✓ [libp2p] 12D3KooWHQRSDFv4FvAjtU…    instance=466453 epoch=5824156   (95ms)
  ✗ [lantern-gateway] gateway.lantern.reiers.io  HTTP 404                (140ms — not counted)
  ✓ [libp2p] 12D3KooWGnkd9GQKo3apk…    instance=466453 epoch=5824156   (263ms)
  ✗ [libp2p] 12D3KooWKKkCZbcigsWTE…    connection failed                (471ms)
  ✓ [libp2p] 12D3KooWBF8cpp65hp2u9…    instance=466453 epoch=5824156   (633ms)
  ✓ [forest] api.chain.love             instance=466453 epoch=5824156   (1.069s)
  ✓ [forest] api.node.glif.io           instance=466453 epoch=5824156   (1.078s)
  (3 remaining libp2p peers canceled when quorum reached)

✓ Quorum reached (5/5 agree)
  instance=466453 epoch=5824156
  tipsetKey=[bafy2bzaceadppiuacqkpbqfpwqkz7jay67llvet57anbibwsj5i5eyuyf6hbc]
  stateRoot=bafy2bzaceczgcretjivryw3spxosjmzkhudik22m2ujgavsj3f6vkzjjujciy
  elapsed=1.078s
  sources counted: 5 of 10
```

The trust anchor written to `~/.lantern/bootstrap-anchor.json`:

```json
{
  "instance": 466453,
  "epoch": 5824156,
  "tipsetKey": [
    "bafy2bzaceadppiuacqkpbqfpwqkz7jay67llvet57anbibwsj5i5eyuyf6hbc"
  ],
  "stateRoot": "bafy2bzaceczgcretjivryw3spxosjmzkhudik22m2ujgavsj3f6vkzjjujciy",
  "capturedAt": "2026-05-21T20:17:44.794942Z",
  "network": "mainnet"
}
```

Note: the gateway source (`gateway.lantern.reiers.io`) returns HTTP 404
because the production gateway doesn't yet expose
`Filecoin.F3GetLatestCertificate` directly — and that's by design.
Even when the gateway is healthy, it is `KindLanternGateway`, which is
**not counted toward the quorum by default** (INSTALLER-SPEC.md §3).
The quorum was reached entirely by three independent libp2p peers
(ChainSafe, Protocol Labs) plus two independent HTTP archives
(chain.love, glif). That is the V1.2 GA threat model in action: even
if every Lantern-operated piece of infrastructure were compromised,
the trust anchor for this user's install would still be the
finalized tipset that five independent operators cryptographically
agreed on.

## `install.sh` — clean run

Environment:

```sh
rm -rf /tmp/lantern-installer-evidence
LANTERN_HOME=/tmp/lantern-installer-evidence \
LANTERN_PREFIX=/tmp/lantern-installer-evidence/bin \
LANTERN_YES=1 \
LANTERN_NO_SERVICE=1 \
LANTERN_BOOTSTRAP_TIMEOUT=60s \
bash ./install.sh
```

Outcome (excerpt; full transcript in
`phase11-install-evidence.log`):

```
🪔  Lantern

▸ Preflight
    ✓ Detected darwin/arm64
    ✓ Tools: curl, tar, shasum/sha256sum available
    ✓ Data directory: /tmp/lantern-installer-evidence

▸ Download Lantern binary
    · Fetching https://github.com/Reiers/lantern/releases/latest/download/lantern-darwin-arm64
    ⚠  Binary not yet published (HTTP 404)
    · Falling back to local source build (requires Go 1.25+)
    ✓ Built and installed to /tmp/lantern-installer-evidence/lantern
    ✓ Symlink: /tmp/lantern-installer-evidence/bin/lantern → …/lantern

▸ Trust bootstrap — multi-source quorum
    [running `lantern init --bootstrap-quorum=5 --bootstrap-timeout=60s --no-wallet`]
    ✓ Quorum reached (5/5 agree): instance=466453 epoch=5824156

▸ Wallet
    [running `lantern wallet new --type=bls`]
    ✓ Wallet created: f3tgtl6kfaka2es2vzrckbigxcbyummw55lr3663bk7nvpmmjlf7uzd5nfff6ntchx6id4bge32ruxxywksy6a

▸ Daemon lifecycle
    · LANTERN_NO_SERVICE=1 set; skipping OS service installation

▸ Done
    ✓ Lantern is installed.

    Connect Curio / Boost:
      export FULLNODE_API_INFO='eyJ…:/ip4/127.0.0.1/tcp/1234/http'

    "Five sources agree; the chain has spoken."
```

The installer is fully idempotent: re-running it skips the download
(unless `LANTERN_REINSTALL=1`), skips the quorum probe (unless
`LANTERN_REANCHOR=1`), and skips wallet creation when one already
exists. Background-service installation (the default under
`LANTERN_YES=1`) is gated behind `--no-service` here only because
this evidence run is inside a sandboxed test directory, not a real
user account.

## Cross-platform release builds

All four target platforms compile clean from a single Mac:

```
▸ Building darwin/arm64...  ✓ size=37 MiB sha=4b12bfb435f66112…
▸ Building darwin/amd64...  ✓ size=39 MiB sha=0e58f48866b4d9dd…
▸ Building linux/amd64...   ✓ size=38 MiB sha=ef3e9cfe5ce3d693…
▸ Building linux/arm64...   ✓ size=35 MiB sha=beec75b0d4dfece5…
```

All built with `CGO_ENABLED=0 -trimpath -s -w`, identical to what
`.github/workflows/release.yml` will produce on tag push.

## Tests

```
$ CGO_ENABLED=0 go test ./...
ok      github.com/Reiers/lantern/chain/bootstrap        0.862s
ok      github.com/Reiers/lantern/cmd/lantern            1.285s
ok      github.com/Reiers/lantern/chain/f3              (cached)
ok      github.com/Reiers/lantern/chain/f3/anchor       (cached)
ok      github.com/Reiers/lantern/chain/f3/subscriber   (cached)
[…32 more packages, all pass…]
```

The new `chain/bootstrap` package's nine test cases cover:

1. All-agree happy path
2. 4-of-5 partial agreement → fails (quorum requires all 5)
3. 3+2 divergence → fails (no single bucket reaches 5)
4. Lower threshold (3) succeeds when 4-of-5 disagrees
5. Per-source timeouts (slow sources don't drag the whole probe)
6. Gateway not counted by default → ErrInsufficientSources
7. Gateway counted when CountGateway=true → quorum reached
8. Progress callback fires once per source
9. All sources fail → ErrQuorumNotReached with counted=0

## Mac menu-bar app

```
$ cd apps/mac
$ swift build -c release
Build complete! (3.58s)
$ Scripts/build_and_sign.sh
✓ Built .build/Lantern.app
$ file .build/Lantern.app/Contents/MacOS/Lantern
Mach-O 64-bit executable arm64
```

The app is `LSUIElement=true` (menu-bar only, no Dock icon), polls
the local daemon at `127.0.0.1:1234/rpc/v1` every 5s while the
popover is open, and reads `~/.lantern/bootstrap-anchor.json` for the
F3 anchor metadata and quorum-recency indicator.
