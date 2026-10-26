# Changelog

All notable changes to Lantern.

## Unreleased

### Mainnet bootstrap works out of the box (F10, F11)
A fresh `lantern init --filecoin-network mainnet` (now `--network mainnet`) failed with "quorum not reached: no single tipset reached 5 sources (countable=9, counted=4)": most libp2p bootstrap peers were dead, and the 5-source default could not be met. The bar is now counted in distinct operators, which is both reachable and stronger.

- **Operator-aware quorum.** `chain/bootstrap` counts DISTINCT OPERATORS, not endpoints. Default: 3 operators must agree on the same F3 finality (mainnet and calibration), and **any conflicting answer for the same instance fails the probe** (`ErrConflict`, never retried, never outvoted). The probe keeps listening 3 s after the threshold so a late conflict is still seen. Different-instance answers are lag, not conflict.
- **Glif is chain.love.** api.node.glif.io and api.calibration.node.glif.io are served from chain.love infrastructure (same IP as api.chain.love, `x-chainlove-lb-*` headers, same Lotus build, HEAD redirects to filecoin.chain.love; observed 2026-10-26). `bootstrap.OperatorOf` now folds `glif.io` onto `chain.love` (and `chainsafe.dev` onto `chainsafe-fil.io`). The old 5-source default could be met by two operators; the `--bootstrap-quorum 4` workaround meant two. The #153 "independent RPC" for mainnet (boot anchor + headcheck) moves from chain.love to Filfox.
- **Bootstrap peers refreshed from lotus v1.38.3** (`build/bootstrap/*.pi` verbatim). `/dnsaddr` roots are now resolved (new `net/dnsaddr`, embedded snapshot as offline fallback) for the libp2p host, DHT, gossipsub direct peers, the #80 trusted floor and the quorum. Removed: the filincubator Venus peer (no DNS) and devtty.eu (TCP/QUIC time out). A peer listed with several addresses is one quorum source, not two.
- **More operators.** Mainnet RPC sources add Filfox and Ankr; calibration adds Filfox and Ankr testnet. Default sets now span 4 operators per network (chain.love, ChainSafe, Filfox, Ankr), 3 required.
- **HTTP User-Agent.** RPC clients send `lantern/<version>`. Filfox returns 403 to Go's default User-Agent, which silently dropped it from the quorum and the boot anchor.
- **Failure guidance.** init/doctor/repair print which sources failed (with operator), how many distinct operators answered vs required, and the flags that change the outcome (`--peer`, `--no-libp2p`, `--bootstrap-quorum N`). A conflict prints a separate explanation and does not suggest lowering the quorum. `lantern doctor` says when no anchor exists yet and what that means for the daemon.
- **F11: no anchor, no daemon.** `lantern daemon` no longer silently anchors via gateway+RPC agreement when init never wrote `bootstrap-anchor.json`; it refuses with instructions. `--allow-rpc-anchor` restores the old path with a loud WARN. Devnet and `--insecure-anchor` are unchanged. `lantern reset --chain-state` now points at `lantern repair` before restarting.
- **F11: stale F3 handled honestly.** Mainnet F3 is stuck at instance 466453 (epoch 5824156, ~231 days behind). The EC head-agreement fallback ignores a stale certificate instead of treating it as a cross-check (it made the single-operator "F3-consistent heaviest" rule reachable), re-samples on disagreement and compares independent operators at a common epoch when their heads straddle an epoch boundary.
- **Installer** passes no `--bootstrap-quorum` by default (uses lantern's default); `LANTERN_BOOTSTRAP_QUORUM` still overrides, now in operators.
- Reasoning and the source/operator table: TRUST-MODEL.md section 2.9.
### Receipt live path: bounded lookups, no false nulls, Cancun opcodes
- **Root cause of the `logs: []` receipt for 0x468f...01d4:** Lantern published that tx at 14:17:51 UTC while still running ecf21fa, before #173. Curio stored the receipt from the old builder, which never decoded logs and hashed the first block CID as `blockHash`. The #173 path, run against the same chain data, now produces a receipt identical to Glif's: same canonical JSON bytes, logs included. A fixture test covers it.
- **Receipt / tx-by-hash lookups can no longer hang.** Each lookup has a time budget (default 20s; `LANTERN_ETH_TX_LOOKUP_TIMEOUT` / `ChainAPI.EthTxLookupTimeout`). Each block fetch gets one retry within 8s. Message bodies for the eth-hash match are fetched 16 at a time. Progress is cached per tipset, so the next poll picks up where the last one stopped. This is what made 0xfd05 take more than 5 minutes after a restart.
- **No false nulls.** `null` is returned only when every tipset in the window was fully checked. If the budget runs out, or some tipset's blocks or message bodies can't be fetched, the lookup returns an error so the caller retries. A tipset that can't be checked no longer aborts the walk, so a match further back is still found.
- **`eth_call`: MCOPY (0x5e), TLOAD (0x5c), TSTORE (0x5d)** are now in the local FEVM interpreter. These were the recurring prod `eth_call` local misses (`unsupported opcode 0x5e` on PDPVerifier `VERSION()` and SP registry `0x2335bde0`). Both now match Glif `eth_call`.

### Bridge-off eth receipts for stock Curio-PDP
Stock Curio-PDP against a bridge-off Lantern registered and sent txs fine, but data-set creation never completed: Curio's `message_waits_eth` receipt carried `logs: []`, so `DataSetCreated event not found in receipt` / `failed to extract pieces from receipt`, and some receipts never resolved at all.

- **Local `eth_getTransactionReceipt` with real logs, Lotus parity.** Served from the canonical chain walked by parent links from head (bounded, default 2000 epochs, `LANTERN_ETH_TX_LOOKBACK` / `ChainAPI.EthTxLookback`). The receipt comes from the child tipset's `ParentMessageReceipts` (CID-verified) and logs from the receipt's events AMT: emitter resolved to its f410 address, `logIndex` counted over every event in the tipset, `blockHash` = Lotus tipset-key hash of the inclusion tipset, Lotus `transactionIndex`, `effectiveGasPrice` from `ComputeGasOutputs`, `cumulativeGasUsed` 0x0, `root`, `logsBloom`, `contractAddress` for EAM creates. Verified field-for-field against Glif on two real calibration Curio txs (fixture test).
- **Works for txs we did not publish** (e.g. after a Lantern restart): matched by recomputing the eth tx hash of delegated-signature messages, or by the CID form of the hash for native messages. Not-yet-included returns `null` like Lotus; bridge-off unknown hashes return `null` instead of `FEVM method requires --vm-bridge-rpc`.
- **Partial header-store tipsets no longer hide txs.** The store can hold only some blocks of a tipset; inclusion tipsets are now rebuilt from the child's `Parents` key (missing headers fetched by CID and verified). Previously a tx in a block the node never received resolved as pending forever.
- **`eth_getTransactionByHash`** uses the same path and returns the full Lotus tx object (accessList, real blockHash / transactionIndex).
- **Message execution order fixed** (`chain/msgsearch`): per block, BLS then secp (Lotus `ApplyBlocks` / `MessagesForTipset`), not all-BLS-first. Affects receipt indexing in `StateSearchMsg` and `eth_getLogs` on multi-block tipsets. Order is cross-checked against the receipt count, with the Lotus nonce rule applied on mismatch.
- **`eth_call` local-miss logging.** Each local miss logs at INFO, rate-limited per (to, selector, reason), with the reason (exec fault text, malformed input, ...), so bridge-off F8 gaps are visible in prod.
- **`Filecoin.EthAddressToFilecoinAddress`** implemented (Curio calls it).
### Byte-reproducible releases
- **Root cause of non-reproducible v1.10.0 binaries found.** `-trimpath` does not rewrite assembler `#include` paths outside the including package, and gnark-crypto includes shared `.s` files from a sibling directory, so the runner's absolute module cache path (`/home/runner/go/pkg/mod`) was baked into `.gopclntab`. A rebuild with any other `GOMODCACHE` differed by millions of bytes with identical `go version -m` output.
- **Pinned container toolchain.** Release binaries are now built by `scripts/repro-build.sh` in `golang:1.26.4` pinned by digest, with `GOTOOLCHAIN=local`, a fixed `GOMODCACHE`, a fresh clone of the tag and an explicit build environment. All targets are built on both an amd64 and an arm64 runner and the release is refused unless both are byte-identical.
- **`.sha256` manifests contain only the asset filename,** so `sha256sum -c lantern-<os>-<arch>.sha256` works next to the download (previously `dist/lantern-...` broke it).
- **`scripts/repro-build.sh <tag> [os/arch ...]`**: one docker command for operators; prints computed vs published SHA-256 and exits non-zero on mismatch. `docs/REPRODUCIBLE-BUILDS.md` rewritten to match, including what is not guaranteed and how v1.10.0 can still be checked.
### Operator docs (Milestone 2)
- **BUILD.md**: clean machine to a verified binary. Go 1.26.4 install with or without sudo, release tag checkout, the exact release build command (`-trimpath -ldflags "-s -w -X main.versionTag=<tag>"`), version and build-info checks, measured timings, pointer to docs/REPRODUCIBLE-BUILDS.md, the one-line installer as an alternative.
- **RUN-PDP.md**: end-to-end calibration walkthrough for stock Curio-PDP (`filecoin/curio-pdp:calibnet`) with Lantern as its only chain node: init, PDP tier, daemon as a systemd user service on the docker0 bridge (with the `--allow-remote-rpc` / token security model), the Curio-PDP compose stack without Forest, admin GUI, storage, PDP wallet and funding, public HTTPS (Cloudflare Tunnel or Curio TLS), FWSS registration, a client data set and proof checks, plus a troubleshooting table of every error hit in the dry run.
- **deploy/curio-pdp/**: `docker-compose.lantern.yaml` (removes skiff's Forest dependency), `env.lantern.example`, `lantern.service` (systemd user unit), `upload-test.mjs` + `package.json` (synapse-sdk client that creates a data set on your provider and reads proving status from PDPVerifier).
- **docs/VERIFICATION-M2.md** template and an **operator report** issue form (`.github/ISSUE_TEMPLATE/operator-report.yml`).
- README no longer pins a "current release" version (links to releases/latest) and links BUILD.md / RUN-PDP.md.

### CLI UX
- **`--network` is the one network flag.** `init`, `doctor`, `repair` and `reset` take `--network mainnet|calibration` like `daemon`, `info`, `node-type` and `wallet`. `--filecoin-network` stays as an alias; passing both with different values is an error; a non-network `--network` value is still accepted as a legacy F3 network name override.
- **`lantern init` next steps carry the network.** After `init --network calibration` the suggested commands are `lantern daemon --network calibration` etc. Previously a bare `lantern daemon` was printed, which would start mainnet.
- **`lantern service install --network <net>` and extra daemon flags.** `lantern service install --network calibration --listen 172.17.0.1:1234 -- --allow-remote-rpc` writes a unit (systemd) or plist (launchd) that runs the daemon with those flags. Previously the service always ran a mainnet daemon with only `--listen`. Arguments are quoted for systemd.
- `lantern help` banner: "lantern, a pure-Go Filecoin node" (was "Filecoin light node (Phase 4)"); usage lists `--network` on every command and documents `node-type`.

### NV29 (Solstice) support
Mainnet (epoch 6470279) and calibration (epoch 4109133) are both on network version 29 with builtin-actors v19. v1.10.0 stopped at actors v18 and NV27, so on both networks every FEVM contract (PDPVerifier, FWSS, ServiceProviderRegistry, USDFC, ...) was served as having no code by local `eth_getCode`, and stock Curio-PDP failed with `GetPDPConfig: no contract code at given address`.

- **Actors v19 bundles.** `state/actors/bundles.go` is regenerated from `lotus@v1.38.3 build/builtin_actors_gen.go` (mainnet + calibrationnet, actors v8..v19) by a new reproducible generator: `go run ./state/actors/internal/genbundles -lotus <tag>` (also wired as `go generate ./state/actors`).
- **go-state-types v0.19.1** (what lotus v1.38.3 uses). v19 decoders added to every versioned loader: account, init, verifreg, datacap, reward, multisig, system, miner, market, power, paych, evm, plus the VM method tables and `StateReadState` system-actor decoding. v19 miner sector records convert to the v18 shape at the boundary (same CBOR layout; the FIP-0118 `FULL_QA_POWER` flag bit is carried through).
- **Live network version.** `StateNetworkVersion` no longer returns a hardcoded NV27. It resolves the tipset height (empty key = head) through the mainnet / calibration upgrade schedule in `build/upgrades.go` (lotus v1.38.3 heights, Lotus boundary semantics: the upgrade epoch still reports the old version). Devnet reports the version recorded by `devnet-init`, else NV27 as before.
- **`StateActorCodeCIDs` mapping fixed.** nv -> actors version now uses go-state-types `actors.VersionForNetwork`. The old hand-written table was off by one from nv25 (it returned the v18 manifest for nv27).
- **Unknown actor code is loud, never a wrong answer.** Local `eth_getCode`, `eth_getStorageAt` and `eth_call` now return "no code" / empty slot only when the actor's code CID is known to be a non-EVM kind. An unknown code CID (a network upgrade this build predates), an unsupported actors version or a head fetch/decode fault falls back to the bridge (or errors bridge-off) instead of answering `0x`, and the first sighting of each unknown code CID logs a WARN naming it. The local FEVM paths also use the accessor's registry, so devnet-registered bundles apply there too.

## v1.10.0 (2026-09-29) - Milestone 1: trustless multi-source head quorum

Lantern no longer trusts a single source for its chain head, neither at boot nor while running. Every head is corroborated by independent operators and libp2p peer groups, checked for fork choice and weight, and refused if it diverges. Upgrade from v1.9.x is drop-in; no config changes are required.

Install / upgrade: `curl -fsSL https://get.golantern.io | bash`

### Running-head quorum (the M1 deliverable)
- **#152** headcheck corroborates the **tipset key** at head-lookback, not just the height, and runs cross-source fork choice (most independent voters, then heaviest ParentWeight). A same-height eclipse fork is now a DIVERGE.
- **#153** Independence is counted per **upstream operator**. The gateway (which proxies Glif) and Glif are one voter. The boot anchor and headcheck add a differently operated RPC (chain.love on mainnet, Filfox on calibration). The gateway advertises `upstream` in `/state/root`.
- **#154** Bridge-off (`--no-fallback-rpc`) gets a running quorum from libp2p peer groups (Hello + gossip forwarders, grouped by IPv4 /16 and IPv6 /32), so an RPC-free node still has independent corroboration.
- **#155** Backfill paths only persist ancestors; head adoption (fork choice + divergence gate) is one guarded decision on the tip. Previously a lighter or diverged candidate could walk the head onto its ancestors during backfill.
- **#156** Weight-monotonic guard: a child's ParentWeight must exceed its parent's.
- **#160** The standalone `lantern daemon` (what production runs) now starts the headcheck monitor and adoption gate. Previously only the embedded daemon did. New flags: `--head-check-rpc`, `--no-head-check`.
- **#162** The adoption gate no longer deadlocks a fresh or lagging node. New `behind` status; local head = max(store head, gossip head).

### Boot anchor / installer
- **#167** Fresh installs work while mainnet F3 finality is stale (the latest F3 certificate has been stuck at epoch 5824156, ~203 days behind the head). `init`, `repair`, the dashboard renew action and the bridge-off auto-stale-reset now share one anchor gate: an F3 quorum winner more than 24h behind the wall-clock head falls back to EC multi-source head agreement (independent operators, #54/#153) at the live head, or is refused. Before this, `init` (and so `install.sh`) failed, and the other three paths could persist the stale anchor. `bootstrap-anchor.json` records `source` (`f3-quorum` / `ec-multi-source`). `doctor` warns on stale F3 finality. `--allow-stale-anchor` is unchanged.

### Observability
- **#156** `lantern version` prints the tag and VCS commit. `/metrics` exports `lantern_build_info`, `lantern_headcheck_status`, `lantern_headcheck_voters{outcome}`, `lantern_headcheck_rounds_total`, `lantern_headcheck_diverged_total` and `lantern_head_rejected_total{reason}`.

### Release process
- **#166** Tags containing `-` (rc/beta/alpha) publish as GitHub prereleases and are never marked latest, so `install.sh` keeps serving the last stable release. Release notes point at the canonical installer `get.golantern.io`.

### Verification
- Mainnet soak of the M1 code on production (census + gateway + beacon, v1.10.0-rc1) from 2026-09-23 to 2026-09-29: 0 restarts, headcheck `agree` throughout, 16,548 quorum rounds, 0 diverges in the journal, 150+ libp2p peers.
- `install.sh` tested on macOS arm64 and Linux amd64 (Mac Pro) from a clean home: anchor, daemon boot, head matching Glif, headcheck `agree` with 40-60 agreeing voters and 0 disagreeing.

Pull requests: #157 #158 #159 #161 #163 #164 #165 #166 #168.

## v1.9.0 .. v1.9.2 (2026-07-23)

_Notes below were written as "Unreleased (v1.9.0)" and shipped in v1.9.0; v1.9.1/v1.9.2 were fix releases (#143-#151)._

### Setup UX / bootstrap reliability

- **Cleaner install output: gateway is no longer probed as an F3 source by default.**
  The Lantern gateway (`gateway.lantern.reiers.io`) serves `/state/root` +
  `/block/<cid>` (hsync-shaped), not the Filecoin JSON-RPC surface, so probing
  it as an rpcSource always 404s. Its real job at runtime is cold-block +
  state-root fallback via `net/hsync`, not F3-quorum corroboration. Previously
  every `lantern init` printed a scary red `✗ [lantern-gateway] ... HTTP 404`
  even on successful installs. The gateway is now elided from the probe unless
  the operator opts in explicitly with `--count-gateway` (the pre-existing flag,
  which now also flips inclusion in the source list). No behavior change for
  the quorum tally itself: the gateway was non-counting by default anyway.

- **Higher `--libp2p-settle` default (8s → 15s).**
  On first-try cold boot, libp2p bootstrap connections sometimes hadn't
  finished dialing when the F3-cert-exchange probe fired, causing quorum to
  fail with a spurious "peer connection failed" from a peer that would have
  responded five seconds later. 15s brings calibration first-try quorum from
  ~2/3 attempts to ~4/5 without user intervention. Higher settle also helps
  mainnet on flaky networks. Applies to `init`, `doctor`, and `repair`.

- **Auto-retry once on transient probe failure.**
  `runBootstrapQuorum` now retries the probe once after a 6s backoff when the
  first attempt fails with `ErrQuorumNotReached` or `ErrInsufficientSources`.
  Second failure remains fatal. Only quorum-shape errors trigger retry;
  context cancellation and arg errors do not. In smoke testing this closes the
  remaining gap between best-case and worst-case first-boot reliability on
  calibration (only 1 public RPC + ~4 libp2p peers available) without adding
  more infrastructure. Log line: `retry 2/2: transient peer flake on first
  pass, probing again...`.

### Devnet parity (closes [#123](https://github.com/Reiers/lantern/issues/123))

All ten devnet-mode RPC parity gaps against `make docker/devnet` lotus
(post-[#122](https://github.com/Reiers/lantern/pull/122)) are addressed.
Every change is gated on `network == "devnet"`; mainnet/calibration paths
are byte-identical to pre-change behaviour.

- **`eth_chainId` + `net_version` now return the devnet's actual EIP-155
  chainId** instead of the mainnet fallback `0x13a` / `"314"`. The
  curio-fork docker devnet uses `31415926` (`0x1df5e76`); custom setups
  may pick different values. `lantern devnet-init` reads `eth_chainId`
  from the running devnet lotus and persists it into `devnet-config.json`
  as `ethChainID`. This was the P0 finding: an EVM client signing
  transactions with the wrong chainId gets "invalid signature" back
  from lotus and takes hours to diagnose.

- **`Filecoin.Version.BlockDelay` now returns the devnet's actual
  cadence** instead of the hardcoded `30`. `lantern devnet-init` reads
  `Filecoin.Version.BlockDelay` from the running devnet lotus and
  persists it as `blockDelaySecs`. Consumers computing epoch-time (curio
  schedulers) get the right value on a 4s-cadence devnet.

- **`Filecoin.StateNetworkName` now returns the devnet's localnet-uuid**
  instead of the literal string `"devnet"`. `devnet-init` already
  discovered the correct name (from `Filecoin.StateNetworkName` on the
  running lotus) and wrote it to `devnet-config.json`; the RPC handler
  now reads from that config instead of returning a literal. Fixes
  gossipsub topic identity (`/fil/blocks/<name>`) and DHT protocol prefix
  alignment.

- **`Filecoin.ChainGetGenesis` on devnet now returns the persisted
  genesis CID** instead of the not-implemented error. Compile-time genesis
  constants exist only for mainnet + calibration; devnet's genesis is
  generated at boot, so the handler now sources the CID from
  `devnet-config.json`.

- **`Filecoin.MpoolGetConfig` returns a stable snapshot** instead of
  HTTP 500. Values chosen to match lotus defaults so consumers (curio at
  boot) don't have to special-case Lantern. `PriorityAddrs` is `nil`
  (marshals to JSON `null`, matching lotus's wire shape byte-for-byte);
  an empty slice would marshal as `[]` and break consumers that check
  `== null`.

- **`Filecoin.MpoolPushMessage` now works on single-node devnet.** The
  gossipsub mesh can't form on a single-node docker devnet, so the
  standard `mpool.Pool` had nothing to publish onto. The daemon now
  wires a Pool whose `Config.Sink` POSTs directly to the devnet lotus
  via `Filecoin.MpoolPush`. All other Pool semantics (persist journal,
  pending set, nonce derivation, [#47](https://github.com/Reiers/lantern/issues/47)
  reconcile/rebroadcast loop, [#121](https://github.com/Reiers/lantern/pull/121)
  restart-persist) work identically; only the wire transport changes.
  Trust posture is unchanged from [#122](https://github.com/Reiers/lantern/pull/122):
  devnet is single-source by design (operator owns both the devnet
  lotus and the Lantern client). Guarded on `network == build.Devnet`.

- **Devnet mode auto-wires the devnet lotus as the VM bridge** when the
  operator did not pass `--vm-bridge-rpc` explicitly. Fixes
  `eth_getCode` / `eth_call` / `eth_getStorageAt` timeouts on cold state
  (a single-node devnet has no libp2p, so bitswap can't fetch state
  blocks; the daemon fell through to `errBridgeUnconfigured`).

- **New `mpool.Config.Sink`** hook lets the Pool run with a nil pubsub
  instance. When `Sink` is non-nil and `ps` is nil, `New()` skips the
  gossipsub join+subscribe entirely and Publish/Rebroadcast route
  through the sink. Only used by devnet mode; mainnet/calibration paths
  continue to require a pubsub instance and reject nil-pubsub construction.
  5 new unit tests in `net/mpool/sink_test.go` cover: nil-pubsub requires
  Sink; Publish invokes Sink instead of gossipsub with byte-identical
  raw; persist journal entry matches published raw; Close is a no-op on
  nil topic/subscription; sink error surfaces from Publish without
  recording pending.

- **`net/glif.Client.EthChainID`, `.BlockDelaySecs`, `.MpoolPush`** — three
  new methods on the shared JSON-RPC client, used by both `devnet-init`
  (for the boot-time discovery) and the daemon (for the devnet mpool sink).

- **`build.DevnetConfig`** gained two fields: `EthChainID` (uint64) and
  `BlockDelaySecs` (uint64). Older devnet configs written before this
  change have zero values; handlers that need them return an error
  hinting `lantern devnet-init --force` to re-capture from the running
  lotus.

Not in this PR: standalone `cmd/lantern` mpool wiring baseline (a
pre-existing gap discovered during the live-smoke run for this PR).

### Devnet: `--devnet-head-poll-interval` knob (closes [#123](https://github.com/Reiers/lantern/issues/123) findings 8+9)

- **Devnet header-store poll interval now defaults to match block
  cadence instead of the mainnet 30s.** On a single-node docker devnet
  the gossipsub mesh can't form, so head arrives ONLY via RPC polling;
  the mainnet-oriented 30s cadence lags a 4s-epoch devnet by ~7x block
  time (finding 8+9 originally documented lags of ~80 epochs = ~320s).
  On devnet, the daemon now derives the poll interval from
  `build.GetDevnetConfig().BlockDelaySecs` (captured at devnet-init time
  by [#125](https://github.com/Reiers/lantern/pull/125)), falling back to 4s if unavailable.
  Live smoke: cold-boot head lag dropped from ~80 epochs to 4-7 epochs
  sustained (~10x improvement).

- **New `--devnet-head-poll-interval` flag** (cmd/lantern) +
  `Config.DevnetHeadPollInterval` field (pkg/daemon) let operators
  override the derived cadence. Zero (default) means auto: read from
  `BlockDelaySecs`. Ignored on mainnet / calibration (the gossip-primary
  cadence logic stays untouched there).

### Devnet: bridge-first read ordering for eth_* (closes [#123](https://github.com/Reiers/lantern/issues/123) finding 7)

- **`eth_getCode`, `eth_getStorageAt`, and `eth_call` on devnet now bypass
  the local hamt walk and go straight to the auto-wired VMBridge.** On a
  single-node docker devnet the local walk always burns its full retry
  budget (no bitswap peers to fetch cold storage-trie blocks from), then
  falls through to the bridge anyway. The devnet lotus IS the source of
  truth for the devnet chain, so short-circuiting is faster (verified
  live: 34 ms vs 15 s timeout for `eth_getCode`) and semantically identical.
  Guarded on `NetworkName == "devnet" && Bridge != nil` so mainnet /
  calibration paths are unchanged, and so an operator who explicitly
  drops the bridge on devnet still gets a clear `errBridgeUnconfigured`
  instead of a silent hang. 5 new unit tests in
  `rpc/handlers/devnet_bridge_first_test.go` lock in the routing table.

### Added

- **FRC-0089 EC finality calculator** ([#96](https://github.com/Reiers/lantern/issues/96)).
  New `chain/ecfinality` package (ported from lotus, math verbatim so the
  reference test vectors apply unchanged) computes an upper bound on reorg
  probability from **observed** block counts per epoch: on a healthy chain
  the 2^-30 guarantee is met around depth ~30 (~15 min) instead of the
  static worst-case 900 epochs. On-demand + cached per head (an idle node
  pays nothing). Windowed-store aware: a node with a shallow observed
  window reports "not computable" instead of an over-confident number.
  Surfaced on the dashboard dev page (new "EC finality" card) and via
  `Daemon.ECFinality()` for embedded consumers. Semantics for consumers:
  `finalized = max(ec-finalized, f3-finalized)` (matches lotus v2 API).
  New dep: `github.com/rvagg/go-skellam-pmf` (tiny, pure Go, same as
  lotus). TRUST-MODEL.md section 2.7 documents what it does and does NOT
  guarantee.

- **VM-bridge cross-check auditor** ([#98](https://github.com/Reiers/lantern/issues/98)).
  New opt-in `--vm-crosscheck` (requires `--vm-bridge-rpc`): once a minute
  Lantern asks the operator's own Forest/Lotus bridge node for the
  canonical tipset at head-3 and compares it against the local header
  store. A mismatch (state-root-level attack, eclipse, or bug) raises a
  loud `DIVERGE` alarm (log + counters + dashboard card) but is strictly
  **observe-only**: reads stay 100% local, the head path is never blocked
  or rewritten, and bridge lag / null rounds are skipped, never
  false-alarmed. No new trust: the bridge is already trusted for block
  production; this reuses the connection as an auditor. Pragmatic
  precursor to full pure-Go re-execution (#89).

- **Wallet import/export + import-from-Lotus** ([#93](https://github.com/Reiers/lantern/issues/93)).
  `lantern wallet export <addr>` / `wallet import [hex|-]` speak Lotus's
  hex-KeyInfo wire format, so keys round-trip between Lotus and Lantern
  in either direction (pipe `lotus wallet export` straight in). `lantern
  wallet import-lotus <repo>` bulk-imports every `wallet-*` key from a
  Lotus repo keystore (base32 filenames, KeyInfo JSON). Everything lands
  in Lantern's passphrase-protected keystore; nothing is written to disk
  unencrypted.

### Security / head-path hardening

- **Head-source corroboration** ([#80](https://github.com/Reiers/lantern/issues/80) part 2).
  The ingestor now (optionally) requires a candidate head to have been
  forwarded by **N distinct gossip peers** before adopting it. Gossipsub
  deduplicates messages before subscribers see them, so the corroboration
  votes are counted where every copy IS visible: a pubsub `RawTracer`
  records the forwarding peer of the first delivery and of every duplicate,
  per block CID (`blockpub.CorroborationTracker`). A trusted floor peer
  (the connmgr-protected bootstrap/beacon/direct set from #80 part 1)
  counts as a super-vote, and the requirement clamps to the connected-peer
  count so a small node never wedges; an uncorroborated block is retried
  briefly (duplicates arrive within ~1s on a healthy mesh), then left to
  the polling Sync safety net. Off by default on Light/PDP; the Full tier
  defaults to 2 distinct sources (`--head-corroboration-peers`, embedded:
  `Config.HeadCorroborationPeers`). New `held_uncorrob` ingestor stat on
  the dashboard dev page (plus previously-invisible `rejected_lighter` /
  `held_diverged`).
- **Per-topic gossipsub score parameters** ([#97](https://github.com/Reiers/lantern/issues/97)).
  Lantern set Lotus's global peer-score params (incl. IP-colocation
  penalties) but left the per-topic map empty, so a peer that never
  usefully delivered anything scored the same as one that consistently
  delivered blocks first. The `/fil/blocks` + `/fil/msgs` topics now carry
  Lotus's exact `TopicScoreParams` (first-delivery credit, invalid-message
  penalties). This is what makes #80's "distinct **scored** peers"
  meaningful against Sybil swarms: freshly-dialed attacker peers have no
  delivery history, score ~0, and decay out of the mesh.

## v1.8.5 (2026-06-30)

**Head catch-up + bridge-off PDP proving robustness, and head fork-choice
hardening.** A bridge-off node could wedge its head ~10-20 epochs behind the
chain tip and then fail PDP proving with "cannot draw randomness from future
epoch" (reported on v1.8.4-m). Root-caused to two stacked bugs plus a
fork-choice gap on the running head.

### Fixed

- **Head catch-up no longer wedges behind the tip** ([#83](https://github.com/Reiers/lantern/issues/83)).
  The #71 gossip-fresh skip suspended the polling Sync whenever gossip
  installed any block recently — but a node whose gossip is fresh-but-lagging
  (installing some blocks while skipping head+N>1 blocks it can't backfill)
  would then have neither path reach the tip. The skip is now **lag-aware**:
  it consults the gossip-observed tip (`blockingest.ObservedHead()`) and only
  skips the catch-up poll when the store head is within a small tolerance of
  it, so a lagging node resumes catch-up. Uses the gossip-observed tip, so
  no extra upstream RPC — #71's 429-protection is preserved at the tip.

- **Bridge-off PDP prove randomness** ([#82](https://github.com/Reiers/lantern/issues/82)).
  `tipsetForRandomness` compared the requested epoch against the frozen boot
  anchor (`Trusted.Epoch`, which never advances) instead of the live header
  head, and the only fallback was bridge-only — so bridge-off nodes failed
  outright and burned the prove task's retry budget. The ceiling is now the
  live head, and when the requested epoch is within a small window above it
  (normal sync catch-up) the call waits briefly (bounded) for the header sync
  to reach it, then draws locally. No bridge required. Genuinely-future
  epochs still error promptly.

### Added

- **Heaviest-ParentWeight fork choice on the running gossip head**
  ([#79](https://github.com/Reiers/lantern/issues/79)). The head ingestor
  advanced head on height-fence + parent-linkage only, so an eclipsed peer
  table could feed parent-linked, height-advancing blocks on a
  valid-but-lighter fork and walk a node onto it. Lantern now applies
  Filecoin's fork-choice rule: adopt a candidate as head only when its
  `ParentWeight` strictly exceeds the current head's. A competing lighter
  fork is rejected (counted as `rejectedLighter`). Pure header arithmetic —
  no proof verification, no ffi. Raises the eclipse cost from "spin up N
  sybil peers" to "out-weight the real chain." Does not fully close the
  un-finalized-tip split against an adversary with real power — that's F3.

- **Un-evictable trusted peer floor** ([#80](https://github.com/Reiers/lantern/issues/80)).
  The trusted bootstrap/beacon/direct-peer set is now `connmgr`-protected,
  so the connection-manager trim path can never drop it. An attacker
  flooding the node with dials can no longer fully replace the peer table
  with peers it controls — the honest floor survives, keeping the heavier
  canonical chain reachable. Complements #79 (which makes a lighter fork
  unadoptable). Head-source diversity (≥N distinct peers) is a tracked
  follow-up.

- **Prefetch-on-send** ([#50](https://github.com/Reiers/lantern/issues/50)).
  When `eth_sendRawTransaction` publishes a tx locally, its message/receipt
  blocks are warmed into the Bitswap cache in the background so the
  follow-up receipt poll resolves locally instead of racing a cold
  cross-peer fetch. Closes the residual write-confirm availability gap.

- **Explicit bridge-off head source** ([#50](https://github.com/Reiers/lantern/issues/50)).
  New `FallbackRPC` (override the upstream RPC, e.g. your own Forest) and
  `NoFallbackRPC` (remove the upstream RPC entirely — head from gossipsub,
  cold blocks from gateway+Bitswap). Previously a bridge-off node silently
  fell back to Glif when gossip stalled, a hidden third-party dependency;
  now a stall surfaces as an observable stalled head. Refuses to start if
  `NoFallbackRPC` is set without libp2p (no head source).

### Changed

- **`TRUST-MODEL.md` §2.1 corrected for accuracy**
  ([#81](https://github.com/Reiers/lantern/issues/81)). The section claimed
  election-proof and weight were re-verified on every header; it now states
  precisely what is verified (CID integrity, signature shape, parent linkage,
  heaviest-ParentWeight fork choice) vs what is not (election-proof VRF,
  winning-PoSt, message re-execution) and why (no-ffi design), and points at
  F3 + the boot quorum as what carries head trust.

## v1.8.4 (2026-06-29)

**Fix: standalone `lantern daemon` could not fetch cold blocks over Bitswap.**
The standalone CLI constructed its Bitswap client without the Filecoin protocol
prefix, so it negotiated the boxo/IPFS default `/ipfs/bitswap/...` instead of
Filecoin's `/chain/ipfs/bitswap/...`. Every mainnet/calibration peer rejected
the stream ("protocols not supported: /ipfs/bitswap/..."), so Bitswap served
zero blocks and the HTTP gateway carried the entire cold-block tail. The
embedded daemon (`pkg/daemon`, used by Curio Core / maxboom) already set the
prefix; only the standalone path was affected.

### Fixed

- **Bitswap protocol prefix on the standalone daemon** — set
  `ProtocolPrefix: network.BitswapProtocolPrefix()` (`/chain`) on the
  `cmd/lantern` Bitswap client. Verified on mainnet: Bitswap now serves cold
  state blocks from the swarm (0 → tens of blocks under a 300-read load), and
  Glif stays at zero. ([#50](https://github.com/Reiers/lantern/issues/50))

### Added

- **Dashboard: live Bitswap detail on the dev page.** The "Block source
  counters" card now shows Bitswap blocks served, bytes in, and want failures
  (5s auto-refresh) alongside the per-source hit table, so operators can
  confirm the swarm is carrying cold blocks rather than the gateway.

## v1.8.3 (2026-06-29)

**Bridge-off RPC parity for stock Curio.** Upstream's PDP-only Curio build
("maxboom", filecoin-project/curio#1311) embeds Lantern over plain JSON-RPC
with **no `--vm-bridge-rpc`**. Several RPC methods still forwarded to the VM
bridge and therefore hard-failed bridge-off. (Curio Core was never affected:
it reads and writes through the embedded VMBridge seam.) This release serves
the remaining PDP read / event / write methods from local state, so a
bridge-off node runs the PDP hot path with zero Glif. No wire/protocol
changes; the `pkg/daemon` and `wallet` public APIs are unchanged.

### Added (local-first, all with graceful bridge fallback)

- **#74 — local `eth_getCode`.** Resolves contract bytecode from live-head
  EVM actor state (CID + hash verified). Returns `0x` for EOAs / non-EVM /
  unknown addresses. Unblocks every `ethclient.CodeAt` contract-presence
  check (PDPVerifier / FWSS / ServiceProviderRegistry / USDFC) bridge-off.
- **#73 — local `eth_getLogs`.** Decodes per-receipt event AMTs into ETH
  logs (the `t1..t4` / `d` recipe, matching Lotus), so PDP settlement and
  dataset watchers plus FilecoinPay rail indexing run with no bridge.
  Bounded to a 24h block range; falls back for older/gapped ranges.
- **#75 — local `eth_getStorageAt` + `eth_getBlockByHash`.** Storage slots
  from the contract KAMT; recent blocks resolved by hash over a bounded
  header-store window.
- **#47 — mpool pending → confirm → rebroadcast loop.** A published-but-
  unmined message is now rebroadcast (identical bytes, same nonce/CID) once
  past a confidence window, dropped when it lands, and surfaced as **failed**
  (with an `OnFailed` callback) after max retries — instead of silently
  stalling and blocking the sender's later nonces. No re-signing, no RBF
  (explicitly out of scope). Wired on the embedded daemon head tick.

### Fixed

- **#71 — redundant Glif `ChainHead` poll under live gossipsub.** A
  standalone node with gossipsub healthy still polled Glif every 6s and got
  rate-limited (HTTP 429). The polling Sync now skips its upstream poll when
  gossip is keeping the store head fresh, and the standalone daemon relaxes
  its sync interval to 30s when libp2p is enabled (matching the embedded
  daemon). Glif is a true catch-up fallback again.
- **#50 — message/receipt block availability bridge-off.** `StateSearchMsg`
  wraps its block fetch in the retrying getter, and the #47 reconcile loop
  warms each in-flight tx's message/receipt blocks on every head, so the
  write-confirm path resolves without Glif.

### Internal

- `state/amt`: `ForEachRaw` (v4 AMT value iteration) for the events AMT.
- `chain/msgsearch`: exported `OrderedMessageCIDs` + `FindChild` so the logs
  path pairs receipts/events with tx hashes in canonical application order.

Tracking umbrella: #76. gofmt + `go vet` + full build clean; hermetic suite
green; `net/mpool` race-clean.

## v1.8.2 (2026-06-29)

Read-path coverage fix. Field-reported: a node running **stock upstream Curio**
behind Lantern with no VM bridge failed its `Settle` task and provider lookups
with `FEVM method requires --vm-bridge-rpc pointing at a Forest/Lotus node`. No
protocol/wire changes, no new external dependencies.

### Fixed

- **#69 — built-in per-network FEVM warm-set.** The state prefetcher only
  warmed the contract addresses its *consumer* injected via
  `Config.FEVMPrefetchAddrs`. **curio-core injects that set itself**
  (`cmd/curio-core/fevm_prefetch.go` — PDPVerifier, FWSS, ServiceProviderRegistry,
  USDFC), which is why bridge-off reads have always worked under curio-core.
  **Stock upstream Curio injects nothing**, so the prefetcher never started, and
  every contract `eth_call` (e.g. the Settle task's
  `ServiceProviderRegistry.getProviderByAddress` / `isRegisteredProvider`)
  local-missed, fell back to the bridge, and — with no bridge configured —
  errored. Lantern now ships its own built-in per-network warm-set of the
  well-known PDP contract proxies (PDPVerifier, FWSS, ServiceProviderRegistry,
  USDFC; mainnet + calibration), merges it with any consumer-supplied addresses
  (consumer wins ordering, de-duped by canonical form), and starts the
  prefetcher whenever the merged set is non-empty. The zero-Glif read path now
  works for **any** Lotus-API consumer with no wiring and no `--vm-bridge-rpc`.
  Built-in addresses are kept in sync with `filecoin-project/curio`
  `pdp/contract/addresses.go`. Distinct from the v1.8.1 sync fixes (#53/#68):
  those addressed desync, this is read-path coverage.

## v1.8.1 (2026-06-29)

Sync resilience + chain-watcher fixes. Two field-reported bugs that together
made a node fall behind live head and then stall its consumer's chain watcher.
No protocol/wire changes, no new external dependencies.

### Fixed

- **#53 — header backfill off the Glif critical path.** Parent backfill (for
  blocks landing at `head+N`) was served by Glif `FetchBlock` with an 8s timeout
  in both the sync agent and the gossip ingestor; a slow/rate-limited Glif drove
  `backfillFail` up and desynced the node (surfacing downstream as e.g.
  `cannot draw randomness from future epoch ... (head ...)`). Backfill is now
  served from the combined bitswap+gateway content-addressed fetcher, Glif as
  last resort; `HeadEpoch`/`TipsetCIDsByHeight` stay RPC-shaped (gossipsub
  supplies live heads). Fetcher resolved lazily to see the bitswap-enabled
  fetcher rebuilt after libp2p start.
- **#68 — `ChainGetTipSet(key)` served from the header store.** It previously
  resolved only the synthetic current head and returned `ErrTipSetNotFound` for
  any other key even with a populated header store, so Curio's `message/watch.go`
  / `deps/apiinfo.go` looking up recent non-head tipset keys looped on
  `tipset not in local store (only current head is cached in V1)`. New
  `Store.GetTipSet(key)` reassembles any persisted tipset by key; `ChainGetTipSet`
  falls through to it after the head fast-path. Distinct from #53 (this surfaces
  even on a healthy synced node).

## v1.8.0 (2026-06-26)

Security hardening: trust model, bootstrap, and auth. Self-audit (#60) of the
trust/bootstrap/auth surface. No protocol/wire changes; the content-addressed
data-integrity model was already sound — these close the gaps around it. No new
external dependencies.

### Security

- **#54 — verified boot anchor.** The boot trusted-root is no longer accepted
  from a single source on faith. It now requires multi-source agreement
  (gateway + Glif) on `(StateRoot, TipSetKey)`, cross-checks the latest F3
  finality certificate (rejecting fork-below-finality), prefers heavier
  `ParentWeight` only when F3-safe, and otherwise refuses to boot.
  `--insecure-anchor` restores single-source boot for dev.
- **#56 — RPC write-path auth.** `eth_sendRawTransaction`/`eth_sendTransaction`
  now require the `sign` permission (they previously fell through to the
  unauthenticated read default because `eth_*` names bypassed the perm switch).
  The RPC refuses a non-loopback bind without `--allow-remote-rpc`.
- **#58 — keystore fail-loud** (also closes #2). An empty passphrase on a
  keystore that already holds keys is refused unless `LANTERN_ALLOW_EMPTY_PASS=1`
  / `--allow-empty-passphrase`.
- **#55 — HTTPS gateway.** Plain-`http://` gateway URLs are refused unless
  `--insecure-gateway` (loopback exempt).
- **#57 — dashboard auth.** The dashboard/metrics listener refuses a non-loopback
  bind without `--allow-remote-dashboard` and requires a `LANTERN_DASHBOARD_TOKEN`
  Bearer when so bound.
- **#59 — beacon eclipse resistance.** A built-in trusted beacon floor seeds
  cert-exchange before DHT discovery warms; the DHT-discovered pool is capped.

## v1.7.24 (2026-06-24)

Complete `eth_getTransactionReceipt` fields — strict-client compatibility and
fee computation off locally-served receipts.

### Fixed

- **`eth_getTransactionReceipt` now returns `from`, `to`, `contractAddress`
  and `effectiveGasPrice`.** The locally-served receipt (zero-Glif write path)
  previously omitted them, so strict eth clients (`cast receipt`, ethers,
  web3) failed to deserialize (`missing field 'from'`) and transaction fee
  (`effectiveGasPrice × gasUsed`) could not be computed locally. `from`/`to`
  come from the locally-tracked sent-tx record; `to` is `null` for contract
  creation. `contractAddress` is present (currently `null`; V1 does not yet
  reconstruct the created-contract address). `effectiveGasPrice` uses the tx
  `maxFeePerGas` as a conservative upper-bound stand-in (V1 does not
  reconstruct the per-tipset base fee at receipt time). Bridge-forwarded
  receipts are unchanged.

## v1.7.23 (2026-06-22)

Keystore & service-start fix — stop re-prompting for a passphrase and start
cleanly as a background service.

### Fixed

- **Passphrase no longer re-prompts on every start (#3).** Lantern holds no
  signing keys of its own (signing lives in the Curio / curio-core wallet), so
  its keystore is empty. The old build read an empty keystore as a brand-new
  node on every boot and re-asked to set a passphrase; it now records the
  choice once and stays quiet on subsequent starts.
- **Background-service install starts cleanly on a fresh node (#1).** Same root
  cause: a first boot with no terminal attached (systemd/launchd) hit the
  "no passphrase, no TTY" path and the daemon exited before it could persist
  anything, so the service died and only a hand-run in `screen`/`tmux` worked.
  A keyless keystore + no terminal now defaults to an unencrypted keystore,
  records it, and starts.

### Changed

- Release workflow assembles the GitHub release body from
  `RELEASE-NOTES-<tag>.md`.

## v1.7.22 (2026-06-22)

Background-service install hardening — the bug a background-mode tester hit.

### Fixed

- **`lantern service install` now wires the keystore passphrase into the
  generated unit/plist.** Previously the generated systemd unit / launchd
  plist omitted `LANTERN_PASS`, so a daemon started by the service manager
  hard-errored on a non-TTY stdin (no way to prompt for the passphrase).
  Now: `--passphrase-file <path>` for an encrypted keystore (read at install
  time, kept out of the unit via `EnvironmentFile=` on systemd / XML-escaped
  inline on launchd), and an explicit `LANTERN_PASS=""` default for the
  common unencrypted read-only backup node so it starts cleanly in the
  background. Env/secret files written `0600`. New `resolveServicePassphrase`
  + `plistEscape`, with unit tests.

### Changed

- README bumped to v1.7.22; repo housekeeping.

## v1.7.21 (2026-06-21)

Zero-Glif write-path correctness — makes local `eth_call` / `estimateGas`
correct for write-shaped calls (DEX swaps, ERC-20 `transferFrom`, payment
rails), the keystone for running an SP fully bridge-off.

### Fixed

- **`vm/evm`: execute `SSTORE`/`LOG` during `eth_call` via an ephemeral
  overlay** (go-ethereum / Lotus semantics) instead of false-reverting.
  Threaded through nested calls; chain state is untouched.
- **`vm`: Glif-class 1B-gas ceiling for FEVM `InvokeContract(Delegate)`** so
  `estimateGas` no longer under-counts heavy calls (was 75M; real swaps use
  100–300M) and `SenderETH` stops building out-of-gas transactions.

## v1.7.19 (2026-06-18)

Secrets isolation + self-healing restarts (#51).

### Added

- **On-disk secrets isolation.** Keystore, admin JWT, and auth tokens move
  to `~/.lantern/<net>/secrets/` (auto-migrated on first start). The daemon
  auto-backs-up `secrets/` to `backups/` on every start (keeps the last 7),
  so recovery operations structurally cannot delete keys. Also fixed
  `auth rotate`/`auth list` writing to the wrong directory.

### Fixed

- **Stale-restart auto-heal (v1.7.18).** After a long downtime the embedded
  sync re-anchors near the live head instead of trying to contiguously
  replay days of lag. Key-safe `lantern reset --chain-state` added.
- Two pre-existing data races (headnotify deliver/close TOCTOU; a passphrase
  test that swapped `os.Stderr`).

## v1.7.0 – v1.7.16 (2026-06-15)

**The zero-Glif sprint.** Read path, then write path, served entirely from
locally verified state and libp2p — no third-party RPC. Built and live-verified
bridge-off on calibration the same day. Engine stays pure-Go, CGO-free; the
only new external dependency across the whole arc is `holiman/uint256`.

### Read path (v1.7.0–v1.7.2, builds on the v1.6.x eth_call engine)

- **Local FEVM `eth_call` made reliable (#43, #44).** Pure-Go EVM execution
  against verified state, an on-head-advance contract-state prefetcher
  (warms PDPVerifier, FWSS, ServiceProviderRegistry, USDFC), and a
  retry-on-miss wrapper that removes the last "block not found" bridge
  fallback for embedded curio-core / filcensus. Adaptive warming: an
  `eth_call` miss feeds the missed address back into the prefetcher, so the
  read path self-heals. Live sample: 100% local, zero bridge fallback.

### Write path (v1.7.4–v1.7.7, #45)

- **Local nonce + gas (v1.7.4).** `eth_getTransactionCount` and
  `eth_estimateGas` served locally and live-head-anchored (caught + fixed a
  boot-anchor staleness bug that risked a nonce collision). estimateGas uses
  a conservative ceiling — never under-counts vs the gateway.
- **ETH-tx codec (v1.7.5).** New CGO-free `chain/ethtx`: minimal RLP codec,
  `ParseSignedEIP1559`, sender recovery via ecrecover, and
  `ToSignedFilecoinMessage()` — no go-ethereum dependency.
- **`eth_sendRawTransaction` → MpoolPush and `eth_getTransactionReceipt`
  → StateSearchMsg, both local (v1.7.7).** Caught + fixed a wiring gap:
  `ChainAPI.Mpool` was never attached in the embedded daemon, so sends
  silently fell back to the bridge. Now mounted on the same gossipsub host
  used for head-tracking (`/fil/msgs/<network>` topic).

### Bitswap + correctness fixes that made bridge-off real

- **v2 AMT decode (v1.7.12, #49) — the keystone.** Block message AMTs and
  the `ParentMessageReceipts` AMT are `go-amt-ipld` **v2** (3-field root,
  implicit width 8); Lantern's `state/amt` used v4. Added `LookupV2` /
  `ForEachV2CIDs` so `StateSearchMsg` resolves receipts locally. FEVM
  contract state stays v4. (Durable: message/receipt AMTs are v2, top byte
  `0x83`; contract state is v4, `0x84`.)
- **`eth_getTransactionByHash` served locally (v1.7.13).**
- **Bitswap as an embedded block source (v1.7.14–v1.7.16, #50).** Mounted
  the libp2p Bitswap client as a high-priority source so message/receipt
  blocks come from the gossip-connected peer set instead of Glif. The
  keystone fix: Filecoin's bitswap protocol prefix is `/chain/ipfs/bitswap`,
  not boxo's IPFS default — any boxo bitswap client talking to Filecoin
  peers must set it. Widened the msgsearch retry budget so each attempt
  contains a full bitswap round. Live bridge-off proof: balance, nonce, and
  send all local, zero Glif block-fetch.

### Result

- Both read and write paths verified **byte-identical to the gateway** with
  the VM bridge disabled, end to end (send → land → receipt). This is the
  release line that makes running an SP fully bridge-off possible.

## v1.6.0 (2026-06-15)

Local FEVM `eth_call` engine (#43, Part B). Execute read-only contract calls
against locally verified state with no Glif: a pure-Go EVM actor loader, a
KAMT state reader, a pure-Go interpreter, and `eth_call` integration with a
VMBridge fallback for safety. Foundation for the v1.7.x zero-Glif line.

## v1.5.8 (2026-06-03)

The "embedded mode grows up" release. Five fixes/features, driven largely
by the curio-core integration and by external adopters running Lantern
standalone (thanks @beck-8). The headline: the embedded daemon
(`pkg/daemon`, used by curio-core) can no longer fall behind the chain,
and now tracks head over gossipsub at the same 0-1 epoch latency as the
standalone daemon.

### Added

- **`eth_subscribe("logs")`** over WebSocket, alongside the existing
  `newHeads` subscription (#32). Together these cover ~95% of
  wallet/dapp usage. `logs` is bridge-backed: on each new head Lantern
  queries the VM bridge's `eth_getLogs` scoped to that block with the
  caller's address/topics filter and pushes each match as an
  `eth_subscription` notification (one per log, matching Geth). Transient
  bridge errors keep the subscription alive; a client write failure
  self-terminates and cleans up. Subscriptions previously had **zero**
  test coverage; this release adds 8 tests (push/skip/unsubscribe/
  self-clean/WS-required for newHeads, per-head filtering/error-survival/
  bridge-required for logs). `newPendingTransactions` + `syncing` remain
  deferred (low usage).
- **Gossipsub head-tracking in `pkg/daemon`** (#40). The embedded daemon
  can now mount a libp2p host + Kademlia DHT + the gossipsub block
  ingestor and track head over `/fil/blocks/<network>` at 0-1 epoch
  latency, with no upstream-RPC dependency for head-following — the same
  path the standalone daemon has always used. When libp2p is enabled
  (`P2PListen != "" && !NoLibp2p`), gossipsub is the primary head source
  and the polling Sync drops to a 30s relaxed cadence as the catch-up
  fallback. The ingestor was extracted from `cmd/lantern` (package main)
  into a new importable package `net/blockingest`; the standalone daemon
  is byte-identical in behaviour. New `Daemon.Host()` and
  `Daemon.GossipStats()` accessors for observability.
- **`lantern info --token-only` and `--network`** (#34, #35).
  `--token-only` (already documented in the README, never actually
  implemented) emits just the raw admin token for scripting
  `FULLNODE_API_INFO`. `--network` lets you inspect a calibration install.

### Fixed

- **Embedded head store could fall behind the chain and stall** (#33,
  curio-core#62). The `pkg/daemon` polling catch-up, on a transient Glif
  error or a deep lag during a long wait (e.g. a ProveTask proving
  window), could leave the head pointer stale until daemon restart — a
  ~70-epoch lag in the field. The Sync agent is now hardened: catch-up
  resumes **contiguously** from `currentHead+1` and is bounded per-poll
  by a new `CatchUpChunk` (never skips epochs); the head pointer **never
  advances past a fetch hole** (a failed-fetch epoch is distinguished
  from a null round and blocks head-advance until retried); and a single
  backfill error is **non-fatal** (stop-and-retry next poll instead of
  aborting the whole cycle). Embedded `MaxBacktrack` raised 60 → 900
  (~7.5h at 30s blocks) to cover the deepest realistic single-wait lag.
- **`lantern info` reported "not initialised" for initialised installs**
  (#35, reported by @beck-8). `info` read the admin token from the
  top-level data dir, but since v1.3 `init` + the daemon mint it under
  the per-network dir (`~/.lantern/<network>/token`). Now reads
  per-network with a legacy top-level fallback for un-migrated installs.
- **`lantern info` advertised the wrong RPC port** (#34, reported by
  @beck-8). The `FULLNODE_API_INFO` line and the `/healthz` probe
  hardcoded `127.0.0.1:1234`, so a daemon on another port showed the
  wrong endpoint and a `404` healthz. The daemon now persists its actual
  `--listen` address to `<netDir>/rpc-listen`, and `info` reports/probes
  that (overridable with `info --listen`). 1234 stays the
  Lotus-compatible default, but the daemon now warns loudly at startup
  when 1234 is already in use (the common local-Lotus collision), with a
  concrete `--listen` alternative.
- **`StateNetworkName` returned the internal network value, not the
  well-known name** (#36, fixed by @beck-8). On calibration it returned
  `"calibration"` instead of `"calibrationnet"`, which made Curio reject
  the chain node with `Network mismatch ... node is on calibration`.

### Notes

- curio-core still runs with `NoLibp2p: true`, so it keeps using the
  (now-hardened) polling path until it opts into gossipsub head-tracking.
  That opt-in plus a 0-1 epoch latency soak is tracked in curio-core#74.