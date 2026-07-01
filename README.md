<div align="center">
  <img src="docs/assets/lantern-mark-256.png" alt="Lantern" width="96" />

  # Lantern

  **The Filecoin node that fits on your laptop.**

  *Boots in minutes. Verifies everything. No 76 GB snapshot. No third-party trust.*

  *Now in three tiers — Light, PDP, and a pure-Go Full node — chosen at install time.*

  [![CI](https://github.com/Reiers/lantern/actions/workflows/ci.yml/badge.svg)](https://github.com/Reiers/lantern/actions/workflows/ci.yml)
  [![License: Apache 2.0 OR MIT](https://img.shields.io/badge/license-Apache--2.0%20OR%20MIT-blue.svg)](#license)
  [![Release: v1.9.0-rc1](https://img.shields.io/badge/release-v1.9.0--rc1-0090ff.svg)](https://github.com/Reiers/lantern/releases)
  [![Go: 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

  **One line to install:**

  ```sh
  curl -fsSL https://get.golantern.io | bash
  ```

  Website: **[golantern.io](https://golantern.io)** · Source: **[github.com/Reiers/lantern](https://github.com/Reiers/lantern)**

</div>

---

## TL;DR

Lantern is a pure-Go Filecoin node. **~40 MB binary, zero CGo, no `filecoin-ffi`, no Rust toolchain.** It serves a Lotus-compatible JSON-RPC (71 / 71 of the Curio `FULLNODE_API` surface, plus the `eth_*` surface needed by FoC clients) and verifies every byte locally against BLS, F3, DRAND, and IPLD content addressing. No trusted RPC provider. No 76 GB snapshot.

**Lantern now installs in three tiers, chosen up front so a light client never carries a heavy footprint:**

| Tier | Footprint | For |
|------|-----------|-----|
| **Light Node** | ~1 GB, in-memory | Clients, wallets, chain reads, SP backup node |
| **PDP Node** (mid) | persistent 2–5 GB | PDP prove/settle + backup block producer |
| **Full Node** | tunable low-GB (not 76) | A real, snapshot-free full node — *in active development* |

> **🚧 Lantern is undergoing a major expansion.** It began as a light client; it is growing into a full node you can run on a Mac mini — snapshot-free boot, tunable single-digit-GB on-disk footprint, and pure-Go block validation with **no Rust**. See [the Full Node epic (#87)](https://github.com/Reiers/lantern/issues/87). The Light and PDP tiers are production-shape today; the Full tier is landing tier by tier.

**Current release:** [v1.8.4](https://github.com/Reiers/lantern/releases/tag/v1.8.4) on mainnet + calibration. **In production today** as the chain backend embedded in [Curio Core](https://curiocore.io) (which ran a full PDP hot-storage flow on Filecoin **mainnet** end-to-end on Lantern) and as a secondary node on the mainnet SP `f03678816` (sp.reiers.io).

## What is Lantern?

Most people who use Filecoin today rely on a remote provider (Glif, Ankr, web wallets) to tell them what's on chain. That works, but it's trust-by-handshake — the provider sees every query, knows every balance, and could in theory show you the wrong data.

The alternative used to be running a full node like Lotus or Forest. That works too, but it means a 76 GB snapshot, hours of sync, ongoing disk growth, and a Rust toolchain. For most users and most embedded use-cases that's overkill.

Lantern is the middle path: a real Filecoin node with real cryptographic verification, in a single static Go binary small enough to ship inside other programs. It joins the actual Filecoin peer-to-peer network, downloads only the blocks you ask about, and proves to itself that every byte is genuine before returning it.

### Who is this for?

- **🌱 Filecoin users.** Hold your own keys, send and receive FIL, look up balances and miners, without trusting Glif or any custodial RPC. Your wallet is your wallet. *(Light Node.)*
- **🪪 SP operators.** Run Lantern as a secondary chain node next to your primary Lotus or Forest. If the primary stalls during a WdPost window, Curio fails over to Lantern and your sectors stay current. *(Light or PDP Node.)*
- **🧱 PDP providers.** Run the PDP prove/settle loop on a persistent, restart-warm cache without standing up a 76 GB Lotus sidecar. *(PDP Node.)*
- **📐 SP / FoC client embedders.** Pull `pkg/daemon` into your Go binary and run a fully verified Filecoin chain backend in-process. [Curio Core](https://github.com/Reiers/curio-core) does exactly this.
- **🔧 Tooling developers.** A real Lotus-compatible RPC on your laptop, no rate limits. Run `eth_*` against a node you control.
- **🖥️ Full-node runners.** Want a real, snapshot-free Filecoin full node on a Mac mini, pure-Go, no Rust? That's the Full Node tier, in active development. *(Full Node.)*

---

## Quick start

**One-line install** (macOS or Linux):

```sh
curl -fsSL https://get.golantern.io | bash
```

This downloads the latest signed release binary, **asks which node tier you want (Light / PDP / Full)**, walks you through a multi-source trust quorum to anchor the node, sets up the wallet, and offers to install Lantern as a background service. Three minutes start to finish. The tier is chosen *at install time* (not a runtime flag) so a Light node genuinely stays light — only the tier you pick provisions the larger cache and footprint.

```
  l  Light Node  — ~1 GB. Clients, wallets, backup node. Smallest footprint. (default)
  p  PDP Node    — persistent 2–5 GB cache + prove/settle + backup block producer
  f  Full Node   — snapshot-free full node, serves the whole chain (in development)
```

Once it's running:

```sh
lantern info                          # status + RPC token
lantern wallet new --type=bls         # generate a key
lantern wallet balance <addr>         # check a balance, verified locally
lantern chain head                    # current chain head
```

And the dashboard at **<http://127.0.0.1:9092/dashboard/>** if you ran with `--metrics 127.0.0.1:9092`.

## Dashboard

Lantern ships an embedded operator dashboard that runs in your browser. It opens automatically whenever you start the daemon with `--metrics` enabled.

Three views via the pill switcher in the top right:

- **Client** (default). One sentence telling you whether the node is healthy, one big chain head number, three friendly stats. Built for someone who's curious about Filecoin but not running infrastructure.
- **SP**. Storage-provider readiness panel up front (block submit gate, VM bridge, RPC listen, lag), sparklines for peers and bandwidth, a stacked bar showing where state blocks are being served from.
- **Dev**. The full data dump. Daemon stats, sync stats, gossipsub ingestor stats, quorum anchor, full peer list with multiaddrs, raw fetcher counters.

Dark mode follows your OS setting. Zero JS framework dependencies, ~34 KB total, all embedded in the binary.

## Node tiers

Lantern is one binary that installs as one of three tiers. You pick the tier **at install time**; the daemon reads the persisted choice at start. This keeps a Light node genuinely light — the PDP and Full footprints are opt-in, not carried by everyone.

### Light Node
The original Lantern. ~1 GB, in-memory block cache. Wallets, chain reads, `eth_*`, the full 71-method Curio RPC surface, and an SP **backup node** role (fail-over chain backend next to a primary Lotus/Forest). Production-shape today.

### PDP Node (mid)
Everything Light does, plus a **persistent** 2–5 GB block cache whose warm PDP/payments/registry/USDFC contract state **survives restart** (a restarted node doesn't cold-fetch its whole warm set mid proving-window), plus the full write surface including **block production** — so it can run the PDP prove/settle loop and double as a backup block producer. Block production requires a VM bridge for a valid post-execution state root. Production-shape today.

### Full Node — *in active development*
A real, snapshot-free Filecoin full node that runs on a Mac mini:

- **Snapshot-free boot.** Anchors at a recent F3-finalized tipset via a multi-source quorum, then fills history over BlockSync + follows head over gossip. No 76 GB snapshot, no genesis re-execution, minutes not days. The anchor's multi-source, BLS-verified, F3-finalized quorum is a *stronger* boot trust than downloading a single-source snapshot.
- **Tunable low-GB footprint.** Keeps headers, F3 certs, DRAND, and actor code always; recent state/messages/receipts for a tunable number of finalities; drops older data (safely re-fetchable, CID-verified). Single-digit-GB steady state instead of ~76 GB, tunable up for archive roles.
- **Pure-Go block validation, no Rust.** Re-verifies each block's signature, election + ticket VRF, miner eligibility, and win-count in pure Go. The two proof-heavy pieces — WinningPoSt SNARK verify and FVM re-execution — are currently anchored to F3 finality (the network's own consensus), with pure-Go implementations of both on the roadmap so a Full node can eventually re-derive them itself with zero Rust.

Full-node work is tracked under [the Full Node epic (#87)](https://github.com/Reiers/lantern/issues/87) and lands tier by tier.

## How it works

Filecoin's state is a content-addressed IPLD merkle tree (HAMTs and AMTs all the way down). If you have a verified state root, you don't need the whole tree — you need the *path* from the root to the actor you care about. That path is a handful of nodes, KBs not GBs, and every node is self-verifying against its parent by CID.

Lantern's bet: most users care about a small slice of state, and that slice can be fetched on demand from any honest peer while the cryptography stays airtight.

## Do I need to run a Filecoin node?

**No.** Lantern is the node, in the sense that matters.

Filecoin's state is content-addressed: every IPLD block has a cryptographic CID, and any block claiming to be at a given CID is provable by hashing it. That means Lantern can ask **anyone** - our gateway, a public IPFS peer, a stranger's Forest node, a friend's Lotus - for the bytes it needs, hash them, and verify them against the CID. The peer that served the bytes cannot lie. They can refuse to answer (denial of service), but they cannot give you wrong data, because content addressing makes lying detectable.

So running Lantern is closer to running your own full node — in security terms — than to using an RPC provider. The difference is that Lantern downloads ~35 MB of binary, ~150 MB of steady-state working set (headers, F3 certs, DRAND beacons, actor code), and pulls state blocks on demand as you query them. No 76 GB snapshot, no hours of sync, no trust in any third party for the data you read.

### Does Lantern participate in F3?

Lantern is **F3-aware but not F3-participating**.

- ✅ **Verifies F3 finality certificates** — every cert's BLS aggregate signature is checked locally against the evolving power table. Lantern's trust anchor is an F3-finalized tipset.
- ✅ **Uses F3 fast-finality** — when you query the chain via Lantern, you get the F3-finalized view, not the EC-finalized view 7.5 hours behind.
- ❌ **Does not vote in GPBFT** — F3 participation requires on-chain storage power and is for Storage Providers only. Light clients have no power and cannot vote.
- ❌ **Does not publish finality certificates** — only GPBFT committee members publish certs.

If you're a Storage Provider running Curio + Lantern, your existing Curio still participates in F3 the normal way. Lantern verifies the certs it produces; it doesn't replace the participation path.

| Approach                                  | Trust model                                                                                  |
|-------------------------------------------|----------------------------------------------------------------------------------------------|
| Glif RPC, Ankr, web wallets               | Trust the provider's node. They control your view of the chain.                              |
| Running your own Lotus or Forest          | Trust the math, hold all the state (76 GB+, hours to sync).                                  |
| **Lantern**                               | **Trust the math, hold ~150 MB. Get data from any honest libp2p peer, verify everything.**   |

Lantern's gateway at `gateway.lantern.reiers.io` is a **last-resort convenience**, not a trust requirement. After Phase 10 Lantern is **swarm-native**: it joins the same Filecoin libp2p network every Lotus, Forest, and Curio node is on, and pulls state via Bitswap from any honest peer. Content-addressing means one honest peer is sufficient for correctness; multiple peers add availability and speed. The HTTP gateway is consulted only when Bitswap can't find the block. See [`SWARM-ARCHITECTURE.md`](SWARM-ARCHITECTURE.md) for the full design.

To strengthen the swarm, operators can run a **Lantern beacon** — a lightweight purpose-built state-serving node:

```sh
./lantern beacon \
    --cache-dir /var/lib/lantern-beacon \
    --cache-size 5GiB \
    --listen /ip4/0.0.0.0/tcp/4001
```

Beacons announce themselves in the DHT under `lantern/beacon/v1` and serve Bitswap requests for any CID in their cache. Forest and Lotus operators can also enable Bitswap advertisement in their existing nodes to become beacons at zero marginal cost. See SWARM-ARCHITECTURE.md §5.

### What are the soft trust points?

Two, named explicitly:

1. **The embedded genesis CID and F3 trust anchor** - baked into the binary at build time. If a malicious build is shipped, those could be wrong. Mitigations on the roadmap: reproducible builds, multiple independent maintainers re-pinning, community cross-validation of the anchor at install time.
2. **The optional FVM bridge** (fallback only) - FEVM contract *reads* (`eth_call`) execute locally in a pure-Go EVM interpreter against verified state, and the FEVM *write* path (nonce, `eth_estimateGas`, `eth_sendRawTransaction`, `eth_getTransactionReceipt`, `eth_getTransactionByHash`) now resolves locally too, Glif-parity proven bridge-off ([#45](https://github.com/Reiers/lantern/issues/45)). The bridge remains as an automatic fallback for the narrow set of VM operations the pure-Go shell doesn't yet cover (some complex builtin-actor methods) and while block-availability hardening lands ([#50](https://github.com/Reiers/lantern/issues/50)). When used, it delegates to a node you configure - documented as a soft-trust point in [`TRUST-MODEL.md`](TRUST-MODEL.md).

For everything else - wallet, balance queries, miner info, market deals, deal status, paych vouchers, state of any actor - zero third-party trust. Just BLS signatures, content-addressed blocks, and your local CPU verifying the math.

```
┌──────────────────────────────────────────────────────────────────┐
│ Lantern node (~1 GB boot, <100 MB steady state)                  │
│                                                                  │
│   Lotus-compat RPC ◄──── Curio / lotus CLI / your app            │
│         │                                                        │
│   State accessor (HAMT/AMT walker, proof-recording)              │
│         │                                                        │
│   Combined fetcher: cache → Bitswap (primary) → HTTP gateway     │
│         │                                                        │
│   Trusted root: (epoch, tipsetCID, stateRoot, F3 cert)           │
│         ▲                                                        │
│   ┌─────┴─────┐  ┌──────────┐  ┌─────────────┐  ┌────────────┐  │
│   │ Headers   │  │ F3 certs │  │ DRAND       │  │ Actor code │  │
│   │ (verified)│  │ (BLS agg)│  │ (verified)  │  │ (pinned)   │  │
│   └───────────┘  └──────────┘  └─────────────┘  └────────────┘  │
└──────────────────────────────────────────────────────────────────┘
                  ▲
                  │ libp2p (gossipsub, Bitswap) + HTTPS gateway
                  ▼
        Filecoin mainnet, gateway.lantern.reiers.io
```

---

## Building from source

**Go 1.25+, no CGo, no filecoin-ffi:**

```sh
git clone https://github.com/Reiers/lantern.git
cd lantern
CGO_ENABLED=0 go build -o lantern ./cmd/lantern

# First-run wizard — includes the multi-source bootstrap quorum.
./lantern init --bootstrap-quorum=5

# Run the daemon (Lotus-compatible RPC on 127.0.0.1:1234)
./lantern daemon
```

**Health-check the trust anchor anytime:**

```sh
lantern doctor             # read-only quorum probe
lantern repair             # re-anchor from a fresh successful quorum
```

**Run as a background service** (launchd on macOS / systemd user on Linux):

```sh
lantern service install          # unencrypted keystore (fine for a read-only backup node)
lantern service status
lantern service stop
```

The daemon needs a keystore-passphrase decision to start unattended. `service install`
defaults to an **unencrypted** keystore (`LANTERN_PASS=""`), which is the right choice for
a backup chain node that holds no funds. To run the service against an **encrypted**
keystore, point it at a passphrase file (kept out of the unit, `chmod 600`):

```sh
printf %s 'your-passphrase' > ~/.lantern/pass.txt && chmod 600 ~/.lantern/pass.txt
lantern service install --passphrase-file ~/.lantern/pass.txt
```

To run in the foreground without a passphrase prompt, set an explicit empty pass:
`LANTERN_PASS='' lantern daemon`.

Talk to it like Lotus:

```sh
export FULLNODE_API_INFO="$(./lantern info --token-only):/ip4/127.0.0.1/tcp/1234/http"

lotus chain head                   # works
lotus state get-actor f099         # works (verified via HAMT walk)
lotus wallet balance <addr>        # works
```

Or use Lantern's own CLI:

```sh
./lantern wallet new --type=bls
./lantern wallet list
./lantern wallet balance <addr>
./lantern chain head
./lantern state get-actor f099
./lantern info                     # status + FULLNODE_API_INFO
```

---

## Status

**Current release: [v1.8.4](https://github.com/Reiers/lantern/releases/tag/v1.8.4)** — production on mainnet + calibration.

What works today:

- **Sync stays current with Lotus.** Lag = 0 epochs most of the time on mainnet. Gossipsub mesh tuned to Filecoin's exact wire format.
- **Cold state queries in single seconds.** `StateMinerInfo` against a previously-unseen miner returns in 0.1–1.8s.
- **Lotus-compatible JSON-RPC.** 71 / 71 of the Curio `FULLNODE_API` surface. Real Curio binaries bind transparently.
- **`eth_*` surface.** `getBlockByNumber`, `getBalance`, `call`, `estimateGas`, `sendRawTransaction`, `getTransactionCount`, `getTransactionReceipt`, `feeHistory`, `newHeads` subscriptions. Works with wallets, dapps, and the FoC `synapse-sdk` stack.
- **Local FEVM `eth_call` — zero-Glif reads.** A pure-Go EVM interpreter executes contract reads against locally-verified state (no `filecoin-ffi`, no Rust). An adaptive prefetcher warms contract storage on each head advance and self-expands on misses. Proven on calibration: a 220-read sample served **100% locally, zero bridge fallback.**
- **Embeddable `pkg/daemon`.** Mint an admin JWT in process, serve `/rpc/v1` inline. This is the API [Curio Core](https://curiocore.io) builds on.
- **VM bridge as optional fallback.** FEVM reads and writes run locally by default; the bridge to an operator's own Forest/Lotus is kept as an automatic safety net during rollout and can be disabled. The remaining work to retire it entirely for *writing* providers is block-availability hardening, tracked in [#45](https://github.com/Reiers/lantern/issues/45) / [#50](https://github.com/Reiers/lantern/issues/50).
- **Embedded operator dashboard.** Three-mode UI (client / SP / dev), follows OS dark-mode, served on the same listener as `/metrics`.

Validated against a real `lotus v1.36` CLI binding to a live Lantern daemon on mainnet — every state read tested matched Glif byte-for-byte.

### Used in production

- **[Curio Core](https://github.com/Reiers/curio-core)** — beta Filecoin Onchain Cloud hot-storage provider. Lantern is the embedded chain backend. The full PDP hot-storage flow (SP registration → self-funded USDFC → payments → dataset → addPieces → live proving cycle) ran **end-to-end on Filecoin mainnet** from a single machine on Lantern (mainnet dataset #1311, provider 31); earlier calibration soak: 8 prove cycles + 5 on-chain USDFC settles. Lantern's `pkg/daemon` is what makes this single-binary deployment possible.
- **`f03678816` / sp.reiers.io** — mainnet Storage Provider running Lantern as a secondary chain node next to Lotus. Curio polls Lantern every ~5s as failover.
- **`https://gateway.lantern.reiers.io`** — public Lantern-backed HTTP IPLD-block gateway. Live since 2026-05-22.

### Release history (condensed)

| Release | What landed |
|---|---|
| v1.8.4 (current) | **Fix: standalone `lantern daemon` Bitswap couldn't fetch cold blocks** — it negotiated the IPFS-default protocol instead of Filecoin's `/chain/...` prefix, so the swarm served zero blocks and the gateway carried the whole cold tail ([#50](https://github.com/Reiers/lantern/issues/50)). Now Bitswap serves cold state blocks from the mainnet swarm. Plus a live Bitswap detail panel on the dashboard dev page. |
| v1.8.3 | **Bridge-off RPC parity for stock Curio** ([#76](https://github.com/Reiers/lantern/issues/76)). Local `eth_getLogs` ([#73](https://github.com/Reiers/lantern/issues/73)), `eth_getCode` ([#74](https://github.com/Reiers/lantern/issues/74)), `eth_getStorageAt`/`eth_getBlockByHash` ([#75](https://github.com/Reiers/lantern/issues/75)); mpool pending→confirm→rebroadcast ([#47](https://github.com/Reiers/lantern/issues/47)); message/receipt block availability ([#50](https://github.com/Reiers/lantern/issues/50)); gossip-aware ChainHead poll skip ([#71](https://github.com/Reiers/lantern/issues/71)). The PDP read+write+event hot path now runs with **no `--vm-bridge-rpc`** and zero Glif — what upstream's PDP-only Curio build ("maxboom") needs. |
| v1.8.2 | **Built-in FEVM warm-set** ([#69](https://github.com/Reiers/lantern/issues/69)). Per-network default warm-set (PDPVerifier, FWSS, ServiceProviderRegistry, USDFC) merged with consumer-supplied addresses, so the zero-Glif read path works for **any** Lotus-API consumer with no wiring. |
| v1.8.1 | **Sync resilience + chain-watcher fixes** (both field-reported). Header backfill moved off the Glif critical path onto the bitswap+gateway fetcher ([#53](https://github.com/Reiers/lantern/issues/53)) — fixes the desync that surfaced as `cannot draw randomness from future epoch`. `ChainGetTipSet(key)` now served from the header store ([#68](https://github.com/Reiers/lantern/issues/68)) — fixes Curio's chain watcher looping on `tipset not in local store`. |
| v1.8.0 | **Security hardening** ([#54](https://github.com/Reiers/lantern/issues/54)–[#59](https://github.com/Reiers/lantern/issues/59)). Verified boot anchor (multi-source + F3 finality cross-check), https-only gateway by default, `eth_*` write-path gate + non-loopback RPC bind guard, dashboard auth, fail-loud on empty keystore passphrase, trusted beacon floor + DHT peer cap. No protocol/wire changes. |
| v1.7.24 | **Complete `eth_getTransactionReceipt` fields** (`from`/`to`/`contractAddress`/`effectiveGasPrice`) — fixes `cast receipt` "missing field from" and unblocks gas-cost display. |
| v1.7.21 | **FEVM write-path fixes proven on mainnet.** `eth_call` now executes write-path opcodes (SSTORE/LOG) via an ephemeral overlay so DEX-swap / `transferFrom` simulations match on-chain, and the FEVM contract-invoke gas estimate gets a Glif-class ceiling so real swaps don't build out-of-gas txs. Both verified by the Curio Core mainnet PDP e2e (WFIL→USDFC swap + addPieces). |
| v1.7.20 | Tester-flow install fixes found in a clean smoke test (install one-liner / first-run hardening). |
| v1.7.19 | **Secrets isolation + auto-backup** ([#51](https://github.com/Reiers/lantern/issues/51) Stage 2). Keystore, JWT secret, and API tokens move into a dedicated `secrets/` directory, physically separated from rebuildable chain state (auto-migrated from older installs). The daemon writes a rolling backup of `secrets/` on every start (last 7 kept). Recovery operations now *structurally* cannot delete keys. |
| v1.7.18 | **Stale-restart auto-heal + key-safe recovery** ([#51](https://github.com/Reiers/lantern/issues/51)). A node stopped for more than a day no longer freezes on its old head: the header-store sync detects an un-backfillable lag and re-anchors near the live head automatically. New `lantern reset --chain-state` is the supported recovery path and **never touches keys**; docs + dashboard stop pointing anyone at `rm -rf ~/.lantern`. |
| v1.7.17 | Mainnet fallback gateway URL for head catch-up ([#50](https://github.com/Reiers/lantern/issues/50)). |
| v1.7.13 – v1.7.16 | **Bitswap as the block source — Glif eliminated from the block path** ([#50](https://github.com/Reiers/lantern/issues/50)). libp2p Bitswap mounted as a high-priority fetcher source using the Filecoin **`/chain/ipfs/bitswap`** protocol prefix (the bitswap analogue of the `/fil/kad/<net>` DHT prefix). `eth_getTransactionByHash` served locally. Live bridge-off: balance + nonce + send all local, **zero Glif block fetches**. |
| v1.7.4 – v1.7.12 | **Local FEVM write path** — local `eth_getTransactionCount` (live-head-anchored nonce), `eth_estimateGas`, a pure-Go EIP-1559 tx codec (`chain/ethtx`), `eth_sendRawTransaction` via gossipsub mempool, and `eth_getTransactionReceipt` via `StateSearchMsg`, **byte-identical to Glif bridge-off** ([#45](https://github.com/Reiers/lantern/issues/45)). Keystone fix: block-message + receipt AMTs are go-amt-ipld **v2**, not v4 ([#49](https://github.com/Reiers/lantern/issues/49)). |
| v1.6.x – v1.7.2 | **Local FEVM `eth_call` — the zero-Glif read keystone** ([#43](https://github.com/Reiers/lantern/issues/43)). Pure-Go EVM interpreter over verified KAMT storage; embedded state-block prefetcher with adaptive warming + deep-trie walk retry ([#44](https://github.com/Reiers/lantern/issues/44)). Read path proven 100% local on calibration. |
| v1.5.8 | Embedded `pkg/daemon` gossipsub head-tracking (0-1 epoch, #40). `eth_subscribe("logs")` over WS (#32). Hardened header-store catch-up so embedded mode can't stall (#33). `lantern info` per-network token + real RPC port + `--token-only`/`--network` (#34, #35). `StateNetworkName` returns the well-known name (#36). |
| v1.5.7 | Installer upgrades when the published SHA differs (was permanently stuck on whichever binary was installed first). No source changes. |
| v1.5.5 | Dashboard on by default at `http://127.0.0.1:9092/dashboard/`. Installer color-rendering hotfix (real ESC bytes). Installer prompts work when piped through `curl \| bash`. |
| v1.5.4 | Installer PATH-detection hotfix for fresh Apple Silicon Macs. Symlink self-heal on re-runs. No source code changes from v1.5.3. |
| v1.5.3 | `web3_clientVersion` + CORS on `/rpc/v1`. `eth_subscribe newHeads`. VMBridge fallback for `StateGetRandomnessDigestFromBeacon`. |
| v1.5.2 | `StateGetRandomnessDigestFromBeacon` for PDP ProveTask. ETH-shape miner+hash fields in `eth_getBlockByNumber`. |
| v1.5.0 | Embeddable `pkg/daemon`. Inline JSON-RPC. AdminToken minting. The release that made Curio Core possible. |
| v1.4.0 | Full `eth_*` coverage. `net_version`. Clean SIGTERM wind-down. |
| v1.3.0 | Calibration support. Per-network data dir + migration. |
| v1.2.1 | The SP-failover release. Sync lag 0 epochs ~65% of the time. DHT prefix fix (peer count 4 → 80). |

Full history: [CHANGELOG.md](CHANGELOG.md).

The one method that stays hard-gated: `SyncSubmitBlock` is never lit without explicit `AllowBlockSubmit=true` plus a configured bridge. See [`docs/SAFETY-CHECKLIST.md`](docs/SAFETY-CHECKLIST.md) §1.

---

## Trust model

- **What you trust:** the cryptographic primitives (BLS, secp256k1, blake2b, SHA-256, DRAND), the embedded genesis CID, and the embedded F3 trust anchor (a recent finalized power table - re-pinned per release).
- **What you verify:** every header (BLS aggregate sig), every F3 certificate (BLS aggregate over ≥2/3 of committee power), every IPLD block fetched (CID hash match), every DRAND beacon, every actor state path you query.
- **What you do not trust:** any RPC provider, any peer serving state, any gateway. They can refuse to serve, but they cannot lie. Wrong bytes fail the hash check and get discarded.

This is strictly stronger than the "trust your RPC" model that most lightweight Filecoin tooling falls back to.

## VM honesty

The Phase 7 VM is a **gas-accurate execution shell**, not a complete FVM. It:

- **Dispatches** every message to the correct built-in actor method (v17/v18 method tables)
- **Accounts gas** using the canonical Filecoin gas schedule - numbers match Lotus byte-for-byte for builtin calls
- **Executes Send** (account-to-account FIL transfer) end-to-end against real state
- **Returns sensible defaults** (ExitCode=0, conservative GasUsed) for every other builtin call
- **Refuses to compute** state-mutating actor methods that would produce a state root the network doesn't agree with
- **Refuses to publish** blocks unless `AllowBlockSubmit=true` is explicitly set

The shell alone is enough for: wallet sends, gas estimation, mempool participation, `StateWaitMsg`, and most of Curio's read-path operations.

## FVM bridge (fallback only)

FEVM contract reads (`eth_call` / `eth_estimateGas`) now execute **locally** in Lantern's pure-Go EVM interpreter against verified KAMT state, so the bridge is no longer required for the read path. For the residual VM operations the pure-Go path doesn't yet cover, Lantern supports an opt-in **side-channel bridge** to a trusted Forest or Lotus node, bounded to:

1. Non-`Send` `StateCall` execution **not yet served locally** (kept as an automatic fallback during rollout). The FEVM write path is no longer bridge-dependent ([#45](https://github.com/Reiers/lantern/issues/45)); the residual is block-availability hardening for freshly-produced message blocks on sparse test networks ([#50](https://github.com/Reiers/lantern/issues/50))
2. `ParentStateRoot` computation for block production

The bridge is **not** used for header verification, F3 finality, state reads (HAMT lookups), DRAND randomness, local FEVM `eth_call`, or any of Lantern's core trust-critical paths - those remain entirely self-verified.

Default daemon has no bridge configured. Operators who want it configure their own trusted node and accept that node as a documented soft-trust point. See [`TRUST-MODEL.md`](TRUST-MODEL.md) for the full attacker analysis.

---

## Why no CGo?

The reference Filecoin VM (`ref-fvm`) is in Rust and ships as `filecoin-ffi`. Pulling that in means:
- A Rust toolchain on every developer machine
- A native build per OS/arch
- Provider-specific quirks in CI, in containers, on macOS Apple Silicon
- Binary size and start-up cost dominated by linkage

Lantern's CGo-free promise is what makes "download one file and run it" tractable. We swap Lotus's CGo BLS for [gnark-crypto](https://github.com/consensys/gnark-crypto)'s pure-Go BLS12-381, lift the (already pure-Go) chain types, beacon verifier, and HAMT/AMT code, and reimplement the runtime pieces that historically lived behind CGo.

`go-state-types`, `go-f3`, `go-libp2p`, `go-jsonrpc`, `drand/v2`, `gnark-crypto`, and `dgraph-io/badger` are the major external deps. All pure Go.

---

## Repo layout

```
chain/         block headers, beacons, F3 certs, trusted root
crypto/sigs/   BLS (gnark), secp256k1, delegated (f4)
state/         HAMT walker, AMT walker, state accessor, actor decoders
net/           libp2p host, gossipsub mempool, Bitswap, gateway client, Glif fallback
rpc/           Lotus-compatible JSON-RPC server + handlers
wallet/        encrypted-at-rest keystore + Wallet facade
api/           Lotus-shape FullNode interface
cmd/
  lantern/                  user-facing CLI / daemon
  lantern-gateway/          HTTP gateway server (deployed at gateway.lantern.reiers.io)
  lantern-f3-anchor/        tool that captures a fresh F3 trust anchor from a Forest/Lotus node
  lantern-lotus-compat-test/ Lotus RPC client compatibility checker
examples/
  historical/phase{1,2,5,6,7}/ archived per-phase end-to-end build-up demos

SCOPE.md              full project scope
MODULES.md            module layout + lift-vs-reimplement decisions
CURIO-RPC-SURFACE.md  the 71-method Curio compatibility target with call-site map
TRUSTED-ROOT.md       trusted-root data structure spec
CHANGELOG.md          release history (per-release notes live on the GitHub releases)
```

---

## Troubleshooting

### "Node is behind" after the daemon was stopped for a while

If Lantern was stopped for more than a day and the dashboard shows
`Status: Reconnecting` with a chain head that's hours or days old,
the daemon now **heals itself**: on restart it detects that the
persisted header store is too far behind to catch up contiguously and
re-anchors near the live head automatically. Just restart the daemon
and watch the head jump to current within a minute.

If you ever want to force a clean chain-state reset (e.g. a corrupted
header store), use:

```sh
lantern stop                 # if it's running as a service
lantern reset --chain-state  # clears header store + trust anchor ONLY
lantern daemon               # re-syncs from live head
```

`lantern reset --chain-state` removes **only** rebuildable chain state
(the header store and the bootstrap anchor). It **never** touches your
keystore, signing keys, JWT secret, or API tokens. **Do not** delete
files under `~/.lantern/` by hand — your wallet keys live there, and a
stray `rm -rf` will destroy them. Always use `lantern reset` instead.

### Where your keys live

As of v1.7.19, all secrets are kept in a dedicated directory, physically
separated from rebuildable chain state:

```
~/.lantern/<network>/
  secrets/                     ← your keys live here
    keystore/                  signing keys (irreplaceable)
    jwt-secret                 RPC auth secret
    token, token-*             pre-minted scope tokens
  headerstore/                 chain state (rebuildable)
  bootstrap-anchor.json        chain state (rebuildable)
  backups/                     rolling tar backups of secrets/ (last 7)
```

Older installs are migrated automatically on first run of v1.7.19+ (the
loose `keystore/`, `jwt-secret`, and `token*` files move into
`secrets/`). The daemon writes a fresh backup of `secrets/` to
`backups/` on every start, so even an accidental data-dir wipe has a
same-machine recovery path until those backups are also removed.

To back up your node, copy `~/.lantern/<network>/secrets/` somewhere
safe. That single directory is everything irreplaceable.

---

## Capture a fresh F3 trust anchor

The embedded anchor is captured per release from a Forest/Lotus node we operate. Anyone can re-capture from their own node:

```sh
# Against a local Forest:
export FOREST_URL="http://127.0.0.1:2345/rpc/v1"
export FOREST_TOKEN="$(cat /opt/forest/data/admin-token)"

CGO_ENABLED=0 go build -o lantern-f3-anchor ./cmd/lantern-f3-anchor
./lantern-f3-anchor -network mainnet -out chain/f3/anchor/anchor_mainnet.json

# Anchor 100 instances behind latest by default; override:
./lantern-f3-anchor -instance 466353 -out anchor_mainnet.json
```

The anchor + a recent F3 certificate chain are how Lantern is cryptographically certain it's tracking the canonical Filecoin chain without holding the whole chain history.

---

## Advisors

Lantern's architecture and PDP-integration work has benefited from technical advisory from the [Curio](https://github.com/filecoin-project/curio) core team:

- **[LexLuthr](https://github.com/LexLuthr)** — Curio core team. Architecture review (Lantern [#10](https://github.com/Reiers/lantern/issues/10)).
- **[Andrew Jackson / @snadrus](https://github.com/snadrus)** — Curio core team. Bundle-architecture design and PDP-only carve-out review (Lantern [#11](https://github.com/Reiers/lantern/issues/11)).

Advisor roles are non-binding; views and code in this repository are the author's responsibility.

---

## License

Lantern is dual-licensed:

- **Apache License 2.0** ([LICENSE](LICENSE) or http://www.apache.org/licenses/LICENSE-2.0)
- **MIT License** ([LICENSE-LOTUS-MIT](LICENSE-LOTUS-MIT) or http://opensource.org/licenses/MIT)

Choose whichever fits your use case. Code lifted from Lotus is dual-licensed under the same terms ([LICENSE-LOTUS-APACHE](LICENSE-LOTUS-APACHE), [LICENSE-LOTUS-MIT](LICENSE-LOTUS-MIT)) with attribution headers on each file.

---

## Project name

Lantern fits Filecoin's natural-element family: **Lotus**, **Forest**, **Aurora**, **Spark**, **Saturn**, **Calibration**. A lantern is portable light - that's the whole product in one word.

---

<div align="center">
  <sub>Built by <a href="https://github.com/Reiers">TSE Reiersen</a> as part of the Filecoin ecosystem.</sub><br/>
  <sub><a href="https://golantern.io">golantern.io</a> · <a href="https://curiocore.io">curiocore.io</a></sub>
</div>
