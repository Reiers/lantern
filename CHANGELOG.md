# Changelog

All notable changes to Lantern.

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
- No public API removals. `eth_subscribe`/`eth_unsubscribe`,
  `StateNetworkName`, and `lantern info` flags are additive or
  bug-fix-only.

## v1.5.7 (2026-05-28)

The "installer actually upgrades when a new release is out" release.

### Fixed

- **Installer was permanently stuck on whichever binary was installed first.**
  The skip-download path tested only `is the binary present?` and silently
  reused stale local binaries forever, even after `lantern` itself had
  shipped multiple new releases. Symptom: users on v1.2.x-era binaries who
  re-ran the installer kept getting v1.2.x behaviour (asks for a passphrase
  every daemon boot because that version writes its keystore under
  `~/.lantern/keystore` instead of the network-split `~/.lantern/<net>/keystore`
  used by v1.5+). Reported by Nicklas, 2026-05-28.
- **New behaviour:** when a local binary exists, the installer fetches the
  published `.sha256` for the requested version and compares. Match →
  skip. Differ → upgrade. Offline / sha unreachable → keep the legacy
  skip-with-warning so offline installs don't break.
- **Download loop simplified.** The earlier subshell+background+race-read
  pattern for capturing curl's exit code was flaky in some bash configs
  (the http_code file wasn't always flushed when read). Replaced with a
  synchronous curl + direct exit-code capture. Also prints the binary size
  on success.

### Other

- No source-code changes; binaries are byte-identical to v1.5.6.

## v1.5.6 (2026-05-28)

The "FilBucket-style installer rewrite" release. Installer-only.

### Changed

- **Full installer rewrite in the FilBucket pattern.** Three concrete things:
  - **Spinner during binary download.** Old behavior dumped lines like
    `Trying URL... HTTP 200` to the terminal as each mirror was tried. New
    behavior shows a single animated braille spinner with the mirror name
    in dim text, folding to a green ✓ on success.
  - **Spinner during the bootstrap quorum** showing a running `(N/5
    sources agreed)` counter instead of streaming every libp2p source
    result. Final result still shows the anchor epoch.
  - **Closing block in the FilBucket idiom**: a soft horizontal rule, the
    binary path, the data dir, a `Next steps` block with four commands,
    the Curio FULLNODE_API_INFO export, links to docs + source + logs,
    and a rotating one-line aphorism at the bottom.
- **Wallet creation guard.** If stdin isn't a TTY (curl|bash, or any
  installer run with stdin redirected), wallet creation is skipped rather
  than left to fail mid-passphrase-prompt. The installer prints both the
  interactive and non-interactive (LANTERN_PASS=...) recovery commands.
- **Lantern ASCII mini-banner** at the top of the installer, in the same
  weight as FilBucket's. Cream wordmark, ink-grey lantern, amber accent
  inside the lantern's body.

### Fixed

- **Closing block was leaking literal `${INK}` and `${BOLD}` strings**
  when the previous version's printf chains tried to embed shell vars
  inside the format string. New version uses `%s` substitution
  consistently.

### Other

- No source-code changes from v1.5.5; binaries are unchanged.
- `dl-lantern.reiers.io` mirror is still returning 404 from the Hetzner
  box; tracked separately.

## v1.5.5 (2026-05-28)

The "installer that doesn't look like raw escape codes, daemon that tells you where the webui is" release.

### Fixed

- **Installer color codes rendering as literal `\033[..]` text.** The
  banner and closing summary used `cat <<EOF` heredocs with `${CLR_X}`
  variables holding `'\033[..]'` string literals. `cat` outputs them
  verbatim; only `printf` interprets the escapes. Result: the banner
  showed `\033[0;36m\033[1m🪔 Lantern\033[0m` instead of a coloured
  banner. Fix: switch color variables to real ESC bytes via `$'\e[..]'`
  and convert all heredocs to `printf`. Now renders correctly in every
  output context.

- **Prompts didn't work when piped through `curl ... | bash`.** stdin
  is the curl response body, not the terminal. All `read` calls now
  pull from `/dev/tty` when available (with a fallback to stdin when
  it isn't, for non-interactive CI runs).

### Added

- **Dashboard on by default.** `lantern daemon` now starts a loopback
  listener on `127.0.0.1:9092` serving `/metrics` AND `/dashboard/`
  without any extra flags. The dashboard URL is printed prominently
  at the end of the startup banner, right before `Ready.`, in bold.
  New flag `--no-dashboard` for operators who want `/metrics` only.
  Set `--metrics=` (empty string) to disable both.

- **Tighter closing summary in installer.** Aligned column layout,
  binary path printed alongside the home dir, `Start the daemon:`
  surfaced as the first action, source + docs links, no more random
  whitespace drift from the heredoc.

### Other

- No CGo, no FFI, no source-side behavior change beyond the daemon
  default for `--metrics`. Existing `lantern daemon` invocations
  continue to work; the only visible change to existing operators is
  that `:9092` is now claimed by default (override with
  `--metrics 127.0.0.1:9099` or similar).

## v1.5.4 (2026-05-28)

The "installer actually works on a fresh Mac" release.

### Fixed

- **Installer PATH detection on Apple Silicon.** Previous default symlink
  target was `/usr/local/bin`, which doesn't exist on fresh Apple Silicon
  Macs without Homebrew. The installer now picks the first available of:
  caller-set `LANTERN_PREFIX` → `/opt/homebrew/bin` → `/usr/local/bin`
  → `~/.local/bin` (created on demand). When the chosen target is not on
  PATH, the installer prints the exact `export PATH=...` line to add to
  the shell profile. Reported by Nicklas, 2026-05-28.

- **Symlink self-heal on re-runs.** The symlink was only created from the
  fresh-download branch; users with an existing binary at `~/.lantern/lantern`
  but a broken (or missing) PATH symlink could re-run the installer and
  still end up with `command not found`. `install_symlink` is now invoked
  from both the download and the skip-download paths.

- **Closing summary always actionable.** The "Status / Chain head / Service"
  commands in the install summary now resolve to the short `lantern` form
  if the symlink succeeded, or the full path (`~/.lantern/lantern info`) if
  it didn't. The full binary path is always shown.

### Changed

- **Mirror order.** `github.com/Reiers/lantern/releases/...` is now the
  canonical download URL (the repo is public as of 2026-05-28). The
  `dl-lantern.reiers.io` mirror moves to fallback. The dl mirror is
  currently returning 404 from the Hetzner box; tracked separately.

### Other

- No source code changes; binaries are byte-identical to v1.5.3.

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

- **Peer-count lift.**
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
