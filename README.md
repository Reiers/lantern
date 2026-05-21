<div align="center">
  <img src="docs/assets/lantern-logo.png" alt="Lantern" width="280" />

  # Lantern

  **A Filecoin light node that boots in minutes with ~1 GB instead of 76 GB.**

  *Verified end-to-end. No CGo. No remote RPC trust.*

  [![License: Apache 2.0 OR MIT](https://img.shields.io/badge/license-Apache--2.0%20OR%20MIT-blue.svg)](#license)
  [![Status: alpha](https://img.shields.io/badge/status-alpha-orange.svg)](#status)
  [![Go: 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

</div>

---

## What is this?

Lantern is a Filecoin light client. It does what Lotus or Forest do — verify the chain, serve state, sign and submit messages — but with a fraction of the footprint:

- **~1 GB on disk** instead of 76 GB (and growing)
- **Minutes to boot**, not hours
- **Single static Go binary**, no CGo, no `filecoin-ffi`, no Rust toolchain
- **Cryptographically verifying** end-to-end. Headers, F3 finality certificates, DRAND beacons, and every IPLD block fetched on demand is verified against its CID. No RPC provider is trusted.

It's designed for three user classes:

| User           | What they want                                  | What Lantern gives them                                  |
|----------------|------------------------------------------------|----------------------------------------------------------|
| **Wallet user** | Send/receive FIL without trusting a custodial RPC | Local key, local sign, on-demand state, ~50 MB cache    |
| **Deal client** | Make deals against real SPs, query market data    | Verified market actor + SP catalog, prefetch on demand  |
| **Storage provider** | Run Curio without a 76 GB Lotus sidecar      | Lotus-compatible RPC server, the killer use case        |

---

## How it works

Filecoin's state is a content-addressed IPLD merkle tree (HAMTs and AMTs all the way down). If you have a verified state root, you don't need the whole tree — you need the *path* from the root to the actor you care about. That path is a handful of nodes, KBs not GBs, and every node is self-verifying against its parent by CID.

Lantern's bet: most users care about a small slice of state, and that slice can be fetched on demand from any honest peer while the cryptography stays airtight.

## Do I need to run a Filecoin node?

**No.** Lantern is the node, in the sense that matters.

Filecoin's state is content-addressed: every IPLD block has a cryptographic CID, and any block claiming to be at a given CID is provable by hashing it. That means Lantern can ask **anyone** — our gateway, a public IPFS peer, a stranger's Forest node, a friend's Lotus — for the bytes it needs, hash them, and verify them against the CID. The peer that served the bytes cannot lie. They can refuse to answer (denial of service), but they cannot give you wrong data, because content addressing makes lying detectable.

So running Lantern is closer to running your own full node — in security terms — than to using an RPC provider. The difference is that Lantern fetches the ~1 GB of headers, F3 certs, DRAND beacons, and actor code on first run, then pulls state on demand as you query it. No 76 GB snapshot, no hours of sync, no trust in any third party for the data you read.

| Approach                                  | Trust model                                                                                  |
|-------------------------------------------|----------------------------------------------------------------------------------------------|
| Glif RPC, Ankr, web wallets               | Trust the provider's node. They control your view of the chain.                              |
| Running your own Lotus or Forest          | Trust the math, hold all the state (76 GB+, hours to sync).                                  |
| **Lantern**                               | **Trust the math, hold ~1 GB. Get data from anyone, verify everything.**                     |

Lantern's gateway at `gateway.lantern.reiers.io` is a **convenience**, not a trust requirement. It exists to make first-run fast for users who don't yet have peers connected. Once Lantern is bootstrapped, it can pull blocks from any libp2p Bitswap peer that has them; the gateway is just one such peer. Every byte is verified locally regardless of source.

The long-term architecture is **swarm-native**: Lantern joins the same Filecoin libp2p network every Lotus, Forest, and Curio node is already on, and pulls state from any honest peer via Bitswap. Content-addressing means one honest peer is sufficient for correctness; multiple peers add availability and speed. The central HTTP gateway becomes a last-resort fallback as the swarm of state-serving "beacons" grows. See [`SWARM-ARCHITECTURE.md`](SWARM-ARCHITECTURE.md) for the full design and V1.2 roadmap.

### What are the soft trust points?

Two, named explicitly:

1. **The embedded genesis CID and F3 trust anchor** — baked into the binary at build time. If a malicious build is shipped, those could be wrong. Mitigations on the roadmap: reproducible builds, multiple independent maintainers re-pinning, community cross-validation of the anchor at install time.
2. **The optional FVM bridge** (off by default) — if you opt in, Lantern delegates the small subset of VM operations its pure-Go shell can't execute (EVM contracts, some complex builtin actor methods) to a node you configure. Documented as a soft-trust point in [`TRUST-MODEL.md`](TRUST-MODEL.md).

For everything else — wallet, balance queries, miner info, market deals, deal status, paych vouchers, state of any actor — zero third-party trust. Just BLS signatures, content-addressed blocks, and your local CPU verifying the math.

```
┌──────────────────────────────────────────────────────────────────┐
│ Lantern node (~1 GB boot, <100 MB steady state)                  │
│                                                                  │
│   Lotus-compat RPC ◄──── Curio / lotus CLI / your app            │
│         │                                                        │
│   State accessor (HAMT/AMT walker, proof-recording)              │
│         │                                                        │
│   Combined fetcher: local cache → Bitswap → HTTP gateway         │
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

## Quick start

```sh
# Build (Go 1.25+, no CGo, no filecoin-ffi)
git clone https://github.com/Reiers/lantern.git
cd lantern
CGO_ENABLED=0 go build -o lantern ./cmd/lantern

# First-run wizard
./lantern init

# Run the daemon (Lotus-compatible RPC on 127.0.0.1:1234)
./lantern daemon
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

Lantern is **alpha**. The cryptography works. The light-client primitives work. The Lotus-compatible RPC surface covers Curio's read path end-to-end. The VM is a **gas-accurate execution shell** that handles account-to-account `Send` natively; an optional **FVM bridge** delegates non-Send execution to a trusted Forest/Lotus node when operators want full VM coverage.

Validated against a real `lotus v1.36` CLI binding to a live Lantern daemon on mainnet — see [`docs/phase8-part-a-results.md`](docs/phase8-part-a-results.md). Every state read tested matched Glif byte-for-byte.

| Phase | Scope                                                                   | Status      |
|-------|-------------------------------------------------------------------------|-------------|
| 1     | Trusted root, header validation, F3 + DRAND verifier                    | ✅ Shipped  |
| 2     | HAMT/AMT walker, state accessor, gateway proxy                          | ✅ Shipped  |
| 3     | (folded into Phase 4)                                                   | —           |
| 4     | Lotus-compatible RPC server, wallet, signing                            | ✅ Shipped  |
| 5     | StateMiner/Market/VerifiedRegistry — SP-killer state APIs               | ✅ Shipped  |
| 6     | Persistent header store, libp2p, gossipsub mempool, StateWaitMsg        | ✅ Shipped  |
| 7     | Pure-Go VM shell, gas estimation, paych vouchers, block production scaffolding | ✅ Shipped  |
| 8     | Live Curio binding test, FVM bridge, block publisher, DHT, paych byte-exact, TRUST-MODEL.md | ✅ Shipped  |
| 9     | `ChainNotify` end-to-end + daemon header-store wiring + real Curio bind on lex — the V1.1 unlock | ✅ Shipped  |

**71 of 71** methods in the Curio FULLNODE_API surface are implemented after Phase 9. The V1.1 unlock landed: `ChainNotify` is wired through a head-change distributor (Lotus-style apply/revert events, bounded per-subscriber buffer with drop-slow semantics) and `StateAccountKey` decodes the account actor state. Live-validated against `lotus v1.36` CLI for 6 minutes and against a real Curio 1.28.1 binary on lex for 10 minutes — see [`docs/phase9-part-b-curio-bind.md`](docs/phase9-part-b-curio-bind.md). The 1 remaining hard-gated stub:
- `SyncSubmitBlock` is never lit without explicit `AllowBlockSubmit=true` plus a configured bridge. See [`docs/SAFETY-CHECKLIST.md`](docs/SAFETY-CHECKLIST.md) §1.

---

## Trust model

- **What you trust:** the cryptographic primitives (BLS, secp256k1, blake2b, SHA-256, DRAND), the embedded genesis CID, and the embedded F3 trust anchor (a recent finalized power table — re-pinned per release).
- **What you verify:** every header (BLS aggregate sig), every F3 certificate (BLS aggregate over ≥2/3 of committee power), every IPLD block fetched (CID hash match), every DRAND beacon, every actor state path you query.
- **What you do not trust:** any RPC provider, any peer serving state, any gateway. They can refuse to serve, but they cannot lie. Wrong bytes fail the hash check and get discarded.

This is strictly stronger than the "trust your RPC" model that most lightweight Filecoin tooling falls back to.

## VM honesty

The Phase 7 VM is a **gas-accurate execution shell**, not a complete FVM. It:

- **Dispatches** every message to the correct built-in actor method (v17/v18 method tables)
- **Accounts gas** using the canonical Filecoin gas schedule — numbers match Lotus byte-for-byte for builtin calls
- **Executes Send** (account-to-account FIL transfer) end-to-end against real state
- **Returns sensible defaults** (ExitCode=0, conservative GasUsed) for every other builtin call
- **Refuses to compute** state-mutating actor methods that would produce a state root the network doesn't agree with
- **Refuses to publish** blocks unless `AllowBlockSubmit=true` is explicitly set

The shell alone is enough for: wallet sends, gas estimation, mempool participation, `StateWaitMsg`, and most of Curio's read-path operations.

## FVM bridge (optional, opt-in)

For operators who need full VM coverage — EVM contract calls, complex multisig operations, or block production with a real state root — Lantern V1 supports an opt-in **side-channel bridge** to a trusted Forest or Lotus node. The bridge is bounded to two operations:

1. Non-`Send` `StateCall` execution
2. `ParentStateRoot` computation for block production

The bridge is **not** used for header verification, F3 finality, state reads (HAMT lookups), DRAND randomness, or any of Lantern's core trust-critical paths — those remain entirely self-verified.

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

## License

Lantern is dual-licensed:

- **Apache License 2.0** ([LICENSE](LICENSE) or http://www.apache.org/licenses/LICENSE-2.0)
- **MIT License** ([LICENSE-LOTUS-MIT](LICENSE-LOTUS-MIT) or http://opensource.org/licenses/MIT)

Choose whichever fits your use case. Code lifted from Lotus is dual-licensed under the same terms ([LICENSE-LOTUS-APACHE](LICENSE-LOTUS-APACHE), [LICENSE-LOTUS-MIT](LICENSE-LOTUS-MIT)) with attribution headers on each file.

---

## Project name

Lantern fits Filecoin's natural-element family: **Lotus**, **Forest**, **Aurora**, **Spark**, **Saturn**, **Calibration**. A lantern is portable light — that's the whole product in one word.

---

<div align="center">
  <sub>Built with care by <a href="https://github.com/Reiers">TSE Reiersen</a> as part of the Filecoin ecosystem.</sub>
</div>
