<div align="center">
  <img src="docs/assets/lantern-mark-256.png" alt="Lantern" width="96" />

  # Lantern

  **The Filecoin node that fits on your laptop.**

  *Boots in minutes. Verifies everything. No 76 GB snapshot. No third-party trust.*

  [![CI](https://github.com/Reiers/lantern/actions/workflows/ci.yml/badge.svg)](https://github.com/Reiers/lantern/actions/workflows/ci.yml)
  [![License: Apache 2.0 OR MIT](https://img.shields.io/badge/license-Apache--2.0%20OR%20MIT-blue.svg)](#license)
  [![Release: v1.5.3](https://img.shields.io/badge/release-v1.5.3-0090ff.svg)](https://github.com/Reiers/lantern/releases)
  [![Go: 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

  **One line to install:**

  ```sh
  curl -fsSL https://get.golantern.io | bash
  ```

  Website: **[golantern.io](https://golantern.io)** · Source: **[github.com/Reiers/lantern](https://github.com/Reiers/lantern)**

</div>

---

## What is Lantern?

Lantern is a small Filecoin node that runs on your computer.

Most people who use Filecoin today rely on a remote provider (Glif, Ankr, web wallets) to tell them what's on the chain. That works, but it's trust-by-handshake — the provider sees every query, knows every balance, and could in theory show you the wrong data.

The alternative used to be running a full node like Lotus or Forest. That works too, but it means a 76 GB snapshot, hours of sync, ongoing disk growth, and a Rust toolchain. For most users that's overkill.

Lantern is the middle path: **a real node with real cryptographic verification, in a 40 MB binary that holds about 150 MB of working state.** It joins the actual Filecoin peer-to-peer network, downloads only the blocks you ask about, and proves to itself that every byte is genuine before showing it to you.

### Who is this for?

Lantern is built for three audiences, and the dashboard ([opens at install time](#dashboard)) has a mode for each:

- **🌱 Client (the default).** You want to hold your own keys, send and receive FIL, look up balances and miners, without trusting Glif or any custodial RPC. You want your wallet to be **your wallet**.
- **🪪 SP backup.** You run a Filecoin Storage Provider and you want a redundant chain node next to your primary Lotus or Forest. If the primary goes down during a WdPost window, Curio fails over to Lantern and your sectors stay current.
- **🔧 Dev.** You're building on Filecoin and want a real Lotus-compatible RPC running on your laptop, not a hosted endpoint with rate limits. Lantern implements 71 of 71 Curio FULLNODE_API methods.

You pick the mode in the upper-right of the dashboard. The first time you open it, it shows you the **Client** view: chain status in plain English, three friendly numbers, an explanation of what's actually happening underneath.

---

## Quick start

**One-line install** (macOS or Linux):

```sh
curl -fsSL https://get.golantern.io | bash
```

This downloads the latest signed release binary, walks you through a multi-source trust quorum to anchor the node, sets up the wallet, and offers to install Lantern as a background service. Three minutes start to finish.

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
2. **The optional FVM bridge** (off by default) - if you opt in, Lantern delegates the small subset of VM operations its pure-Go shell can't execute (EVM contracts, some complex builtin actor methods) to a node you configure. Documented as a soft-trust point in [`TRUST-MODEL.md`](TRUST-MODEL.md).

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
lantern service install
lantern service status
lantern service stop
```

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

**Current release: v1.2.1.** Lantern is alpha but production-deployed on at least one mainnet Storage Provider (f03678816, sp.reiers.io) as a secondary chain node.

What works today:

- **Sync stays current with Lotus.** Lag = 0 epochs most of the time, lag = 1 epoch transiently during epoch transitions. Gossipsub mesh tuned to Filecoin's exact wire format.
- **Cold state queries in single seconds.** `StateMinerInfo` against a previously-unseen miner returns in 0.1–1.8s, down from 30s+ in the previous release.
- **Lotus-compatible JSON-RPC.** 71 of 71 Curio FULLNODE_API methods implemented. Real Curio binaries bind transparently.
- **VM bridge for block production.** When configured, Lantern can produce blocks for an SP by delegating the post-execution state-root computation to an operator's own sibling Forest or Lotus node.
- **Embedded operator dashboard.** Three-mode UI, prefers-color-scheme, served on the same listener as `/metrics`.

Validated against a real `lotus v1.36` CLI binding to a live Lantern daemon on mainnet — see [`docs/phase8-part-a-results.md`](docs/phase8-part-a-results.md). Every state read tested matched Glif byte-for-byte.

| Phase | Scope                                                                   | Status      |
|-------|-------------------------------------------------------------------------|-------------|
| 1     | Trusted root, header validation, F3 + DRAND verifier                    | ✅ Shipped  |
| 2     | HAMT/AMT walker, state accessor, gateway proxy                          | ✅ Shipped  |
| 3     | (folded into Phase 4)                                                   | -           |
| 4     | Lotus-compatible RPC server, wallet, signing                            | ✅ Shipped  |
| 5     | StateMiner/Market/VerifiedRegistry - SP-killer state APIs               | ✅ Shipped  |
| 6     | Persistent header store, libp2p, gossipsub mempool, StateWaitMsg        | ✅ Shipped  |
| 7     | Pure-Go VM shell, gas estimation, paych vouchers, block production scaffolding | ✅ Shipped  |
| 8     | Live Curio binding test, FVM bridge, block publisher, DHT, paych byte-exact, TRUST-MODEL.md | ✅ Shipped  |
| 9     | `ChainNotify` end-to-end + daemon header-store wiring + real Curio bind on lex — the V1.1 unlock | ✅ Shipped  |
| 10    | Live libp2p stats wired to Curio webui (Net*), Bitswap as primary fetch path, `lantern beacon` subcommand — V1.2 swarm-native delivery | ✅ Shipped  |
| 11    | One-line installer + multi-source bootstrap quorum + `lantern doctor` / `repair` / `service` + native Mac menu-bar app + tag-triggered release pipeline — V1.2 GA delivery | ✅ Shipped  |

**71 of 71** methods in the Curio FULLNODE_API surface are implemented after Phase 9; Phase 10 turned the Net*/Eth probe stubs (`NetPeers`, `NetBandwidthStats`, `NetAutoNatStatus`, etc.) into **live data** sourced from the running libp2p host. The V1.1 unlock landed in Phase 9: `ChainNotify` is wired through a head-change distributor (Lotus-style apply/revert events, bounded per-subscriber buffer with drop-slow semantics) and `StateAccountKey` decodes the account actor state. Live-validated against `lotus v1.36` CLI for 6 minutes and against a real Curio 1.28.1 binary on lex for 10 minutes — see [`docs/phase9-part-b-curio-bind.md`](docs/phase9-part-b-curio-bind.md) and [`docs/phase10-part-d-live-bind.md`](docs/phase10-part-d-live-bind.md). The 1 remaining hard-gated stub:
- `SyncSubmitBlock` is never lit without explicit `AllowBlockSubmit=true` plus a configured bridge. See [`docs/SAFETY-CHECKLIST.md`](docs/SAFETY-CHECKLIST.md) §1.

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

## FVM bridge (optional, opt-in)

For operators who need full VM coverage - EVM contract calls, complex multisig operations, or block production with a real state root - Lantern V1 supports an opt-in **side-channel bridge** to a trusted Forest or Lotus node. The bridge is bounded to two operations:

1. Non-`Send` `StateCall` execution
2. `ParentStateRoot` computation for block production

The bridge is **not** used for header verification, F3 finality, state reads (HAMT lookups), DRAND randomness, or any of Lantern's core trust-critical paths - those remain entirely self-verified.

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
  lantern/             user-facing CLI
  lantern-gateway/     HTTP gateway server (deployed at gateway.lantern.reiers.io)
  lantern-f3-anchor/   tool that captures a fresh F3 trust anchor from a Forest/Lotus node
  lantern-phase{1..7}/ end-to-end demos per phase

SCOPE.md              full project scope
MODULES.md            module layout + lift-vs-reimplement decisions
CURIO-RPC-SURFACE.md  the 71-method Curio compatibility target with call-site map
TRUSTED-ROOT.md       trusted-root data structure spec
PHASE*-BLOCKERS.md    per-phase deferred decisions and known limitations
```

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
