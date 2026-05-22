# Changelog

All notable changes to Lantern.

## v1.2.1 (2026-05-22)

The SP-failover release. Lantern stays in sync with the network in real
time, answers cold state queries in single seconds, can produce blocks
for a Storage Provider via an opt-in VM bridge, and ships an operator
dashboard. Deployed on mainnet against `f03678816` (sp.reiers.io).

### The headline numbers

Measured against a live `lotus v1.36` on the same box:

- **Sync lag: 0 epochs** ~65% of the time, 1 epoch during epoch
  transitions (down from a stable 1-epoch lag in V1.2.0). The remaining
  lag is a fraction-of-a-second propagation window, not a polling
  artifact.
- **`StateMinerInfo` cold-cache: 0.09–1.75 seconds** depending on miner
  HAMT depth (down from 30s timeouts in V1.2.0).
- **`MinerCreateBlock` end-to-end: 0.22 seconds** when the VM bridge
  is wired (was 30s timeout).

### Added

- **Embedded operator dashboard** (`cmd/lantern/dashboard/`,
  `cmd/lantern/dashboard.go`). Three-mode UI — Client / SP / Dev — with
  a brand mark, a friendly chain-status sentence in plain English,
  sparklines for peer count and bandwidth, a stacked bar for block
  source distribution, copy-to-clipboard CIDs, and prefers-color-scheme
  dark mode. Single embedded HTML/JS/CSS file via `go:embed` (~34 KB).
  No build step, no framework dependency. Activated whenever
  `--metrics` is set; served at `/dashboard/` alongside `/metrics` and
  `/healthz`. Four read-only JSON endpoints under
  `/api/dashboard/*` provide the data.

- **VM bridge wiring + CLI flags.** Four new daemon flags:
  `--vm-bridge-rpc <url>` (upstream Forest/Lotus JSON-RPC),
  `--vm-bridge-token <jwt>` (or env `LANTERN_VM_BRIDGE_TOKEN`),
  `--vm-bridge-timeout <duration>`, and `--allow-block-submit`. The
  block-submit gate refuses to start without a bridge configured.
  `ForestBridge` is the JSON-RPC client; it calls
  `Filecoin.StateCompute` to obtain the post-execution state root and
  per-message receipts. Eight new unit tests, including positive +
  negative wire-format guards.

- **Race-fetch in the combined blockstore** (`net/combined/fetcher.go`).
  New `Source.Race=true` flag fires gateway + Bitswap concurrently for
  cold IPLD blocks. First CID-verified response wins; the slow source
  is cancelled via ctx. Glif stays as the sequential last-resort
  fallback. Three new tests cover happy path, fall-through, and total
  failure.

- **Gossipsub block ingestor with inline parent backfill**
  (`cmd/lantern/gossip_block.go`). Subscribes to `/fil/blocks/testnetnet`
  and installs new heads as they arrive over the mesh, instead of
  waiting for the next polling cycle. Inline RPC backfill (default cap
  3 epochs) fills small parent gaps without round-tripping through the
  polling Sync. Eight unit tests covering dedupe, height fence, parent
  fence, channel overflow, nil guards, and inline backfill paths.

- **Filecoin-shape gossipsub configuration** (`net/libp2p/pubsub.go`).
  Stock libp2p gossipsub defaults are tuned for IPFS; Lantern's mesh
  was effectively isolated from the Filecoin one because of mismatched
  message-ID hashing and overlay constants. This release lifts the
  full Lotus/Forest gossipsub init: `GossipSubD=8`, `Dhi=12`, `Dlo=6`,
  `Dlazy=12`, blake2b message IDs, `WithFloodPublish(true)`,
  `WithPeerExchange(true)`, Lotus peer-score parameters, and
  `pubsub.WithDirectPeers` for the bootstrap nodes so we mesh-pin with
  ChainSafe, chain.love, filincubator, and devtty.

- **DHT protocol prefix fix.** The daemon used `/fil/kad/1.0.0`; the
  beacon used the default `/ipfs/kad/1.0.0`. Filecoin mainnet actually
  speaks `/fil/kad/testnetnet/kad/1.0.0` (network name baked into the
  prefix). Every peer that connected previously failed the DHT
  handshake and got evicted from the routing table, keeping peer count
  stuck at 4 forever. Fix: derive the prefix from
  `build.MainnetNetworkName`. Post-fix peer count climbs 4 → 80 in 3
  minutes and stabilizes around `MinPeers=50` via connmgr trim.

- **Beacon cert-exchange responder (B-11-01).** New package
  `chain/f3/certexch` wraps `go-f3/certexchange.Server` around an
  in-process `certstore.Store` seeded from the embedded F3 trust
  anchor. A poll loop pulls verified certs forward from an upstream
  Lotus-compatible JSON-RPC source (Glif by default) and inserts them
  after `chain/f3.VerifyCertChain` validation. Wired into `lantern
  beacon` so the responder shares the beacon's existing libp2p host;
  controlled by `--certexch`, `--certexch-upstream`, `--certexch-poll`.

- **`LanternBeaconSource` goes live.** Replaces the V1.2.0
  `ErrNoBeaconBackend` stub with a real `certexchange.Client` that
  dials a beacon's peer over libp2p and returns a `Finality` shaped
  to match the quorum's equality contract. Counts toward the quorum
  by default (independent operators, not the project itself).
  `sources.DefaultLanternBeacons` ships with the Hetzner reference
  beacon.

- **Version string cleanup.** `Filecoin.Version` and `lantern version`
  now report `<tag> Lantern+<network>` (e.g. `v1.2.1 Lantern+mainnet`),
  driven by `internal/buildinfo` and the `-ldflags -X main.versionTag=...`
  injection point. The legacy `lantern/0.4.0 (lotus-compat)` constant
  is gone; untagged dev builds report `dev Lantern+mainnet`.

- **Peer-count lift (PHASE11-PEER-COUNT-ASK.md Fix 1 + Fix 2).**
  Adds an explicit `connmgr.NewConnManager(MinPeers, MaxPeers, 20s)`
  to the libp2p host (daemon: 50/200, beacon: 100/200) and runs two
  DHT discovery loops on top of the client-mode DHT:
  `GetClosestPeers(self)` every 5 minutes to populate the routing
  table, plus a routing-table-walk that dials up to 25 unknown peers
  every 10 minutes.

### Fixed

- **`ForestBridge` Root CID wire format.** Lotus encodes the
  `StateCompute` Root as the IPLD-link JSON shape `{"/":"<cid>"}`,
  not as a bare string. The original decoder used `Root string` and
  would have silently returned `cid.Undef` against a real Lotus, producing
  invalid block headers. Caught during the live deploy; fixed with a
  small `cidLink` struct + two new tests that guard both directions of
  the wire format.

### Notes

- No CGo. No new external dependencies beyond `go-f3` (already pinned)
  and `golang.org/x/crypto/blake2b` (already pulled in by other deps).
- 71/71 Curio FULLNODE_API method coverage unchanged.
- Issue tracker: #1 sync lag (closed), #3 StateMinerInfo timeout
  (closed), #4 VM bridge (closed), #5 web dashboard (closed). Open:
  #2 passphrase fallback hardening, #6 beacon swarm-first defaults,
  #7 JWT expiry, #8 brand mark.

## v1.2.0 (planned — Phase 11 / V1.2 GA)

The headline V1.2 GA release: one-line install, multi-source trust
quorum at cold start, and a native macOS menu-bar app for non-CLI
users.

### Added

- **`lantern init --bootstrap-quorum=N`**: multi-source quorum
  bootstrap. Asks N independent sources for the latest F3 finality
  cert and refuses to write a trust anchor unless ≥N agree on the
  same `(instance, tipsetKey, stateRoot)`. Default 5; lower with care.
- **`chain/bootstrap/`**: new package implementing the `Source`
  interface and `Quorum()` driver. Built-in source kinds:
  - `KindLibp2p` (F3 `/f3/certexch/get/1/<nn>` cert-exchange)
  - `KindForest` (Lotus-compatible JSON-RPC `Filecoin.F3GetLatestCertificate`)
  - `KindUser` (user-supplied `--peer URL`, same shape as Forest)
  - `KindLanternGateway` (Lantern's own gateway; **not counted by
    default**, see INSTALLER-SPEC §3)
  - `KindLanternBeacon` (Lantern beacons over cert-exchange — stub in
    V1.2.0, made live in V1.2.1 by B-11-01)
- **`lantern doctor`**: re-runs the bootstrap quorum probe without
  writing anything; reports per-source health.
- **`lantern repair`**: re-runs the quorum probe and overwrites the
  persisted trust anchor.
- **`lantern service install|uninstall|start|stop|restart|status`**:
  cross-platform background-daemon lifecycle via launchd (macOS) and
  systemd user units (Linux).
- **`lantern stop` / `lantern restart`**: aliases for `service stop`
  and `service restart`.
- **`install.sh`**: one-line shell installer (`curl -fsSL
  https://get.lantern.reiers.io | bash`). Detects OS/arch, downloads
  the signed binary, runs the quorum bootstrap, prompts for
  background/foreground lifecycle, prints `FULLNODE_API_INFO`.
- **`.github/workflows/release.yml`**: tag-triggered cross-platform
  release pipeline. Builds `lantern-{darwin,linux}-{arm64,amd64}` with
  `CGO_ENABLED=0 -trimpath -s -w`, emits `.sha256` manifests, publishes
  a GitHub release including the deterministic source tarball.
- **`docs/REPRODUCIBLE-BUILDS.md`**: recipe + rationale for byte-
  identical reproduction of any release binary from the source tag.
- **`apps/mac/`**: SwiftUI menu-bar app. MVP shows chain head epoch +
  peer count + quorum status indicator in the menu bar.

### Changed

- `lantern init` now leads with the bootstrap quorum step; falls
  through to wallet + JWT setup only after the trust anchor is
  written. Pass `--bootstrap-quorum=0` to skip the quorum (NOT
  recommended outside testing).
- `cmd/lantern/main.go` `version` string bumped to `0.5.0 (Phase 11)`.
- `chain/bootstrap/sources` includes `api.chain.love` alongside
  `api.node.glif.io` in the default HTTP source set, raising the
  chance of reaching quorum=5 on cold start when only ~3 libp2p
  bootstrap peers happen to respond to cert-exchange.

### Constraints

- The Lantern gateway (`gateway.lantern.reiers.io`) is **never**
  counted in the quorum by default. Operators who explicitly trust
  the project can opt in via `--count-gateway`.
- `CGO_ENABLED=0 go build ./...` and `CGO_ENABLED=0 go test ./...`
  pass at every commit on Phase 11 main.
- No imports of `github.com/filecoin-project/lotus/...` or
  `filecoin-ffi`.

## v1.1.0 — Phase 10 (V1.1 GA, swarm-native)

Real libp2p host wired into `Net*` RPC methods; `boxo/bitswap` as
primary fetch source; `lantern beacon` subcommand; Curio webui Chain
Node Network panel renders live data. 71/71 method coverage.

See `PHASE10-BLOCKERS.md` for the full delivery report.

## v1.0.0 — Phases 1–9 (V1, working light client)

- Phase 1: trusted-root construction pipeline
- Phase 2: HTTP gateway client + Glif fallback
- Phase 3: wallet + JWT + RPC handlers
- Phase 4: Lotus-compatible JSON-RPC surface
- Phase 5: state walker + HAMT/AMT readers
- Phase 6: F3 cert-chain subscriber walking forward from embedded anchor
- Phase 7: FVM bridge for ETH-style state queries
- Phase 8: gossipsub block subscriber + DHT scaffolding
- Phase 9: header store + persistent sync + 71/71 Curio method coverage

See `PHASE{1..9}-BLOCKERS.md` files for per-phase delivery reports.
