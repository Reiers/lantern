# Changelog

All notable changes to Lantern.

## v1.2.1 (in progress)

The V1.2 trust-model completion: Lantern beacons can now answer F3
cert-exchange queries, turning `LanternBeaconSource` from a stub into
a live quorum source.

### Added

- **Beacon cert-exchange responder (B-11-01).** New package
  `chain/f3/certexch` wraps `go-f3/certexchange.Server` around an
  in-process `certstore.Store` seeded from the embedded F3 trust
  anchor. A poll loop pulls verified certs forward from an upstream
  Lotus-compatible JSON-RPC source (Glif by default) and inserts them
  after `chain/f3.VerifyCertChain` validation. Wired into `lantern
  beacon` so the responder shares the beacon's existing libp2p host;
  controlled by `--certexch`, `--certexch-upstream`, `--certexch-poll`.
- **`LanternBeaconSource` is live.** Replaces the V1.2.0
  `ErrNoBeaconBackend` stub with a real `certexchange.Client` that
  dials a beacon's peer over libp2p and returns a `Finality` shaped
  to match the quorum's equality contract. Counts toward the quorum
  by default (independent operators, not the project itself).
- **`sources.DefaultLanternBeacons`.** Curated set of known-good
  Lantern beacons that speak cert-exchange. Ships with the Hetzner
  reference beacon. `BuildDefaultSources` wires this into the default
  quorum source list when a libp2p host is available.
- **Tests.** Unit test (`chain/bootstrap/sources/lantern_beacon_test.go`)
  uses a mocknet + `certexchange.Server` to assert
  `LanternBeaconSource.LatestFinality()` decodes a seeded cert into the
  expected `Finality`. Integration test
  (`chain/f3/certexch/server_test.go`) brings up the full responder +
  source pair over an in-process libp2p mocknet and round-trips a cert.

### Notes

- No CGo, no new external dependencies beyond `go-f3` (already
  pinned). 71/71 Curio FULLNODE_API method coverage unchanged.

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
