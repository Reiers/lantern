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

Lantern is **alpha**. The cryptography works. The light-client primitives work. The Lotus-compatible RPC surface covers Curio's read path end-to-end. The VM is a **gas-accurate execution shell**, not a full FVM port — it dispatches every Curio call and accounts gas correctly, but only executes account-to-account `Send` end-to-end. Block production is gated behind an explicit operator opt-in flag pending the FVM bridge.

| Phase | Scope                                                                   | Status      |
|-------|-------------------------------------------------------------------------|-------------|
| 1     | Trusted root, header validation, F3 + DRAND verifier                    | ✅ Shipped  |
| 2     | HAMT/AMT walker, state accessor, gateway proxy                          | ✅ Shipped  |
| 3     | (folded into Phase 4)                                                   | —           |
| 4     | Lotus-compatible RPC server, wallet, signing                            | ✅ Shipped  |
| 5     | StateMiner/Market/VerifiedRegistry — SP-killer state APIs               | ✅ Shipped  |
| 6     | Persistent header store, libp2p, gossipsub mempool, StateWaitMsg        | ✅ Shipped  |
| 7     | Pure-Go VM shell, gas estimation, paych vouchers, block production scaffolding | ✅ Shipped  |
| 8     | Live Curio binding, FVM bridge, public release, docs site               | 🔄 In progress |

**64 of 71** methods in the Curio FULLNODE_API surface are implemented after Phase 7. Of the remaining 7:
- 6 are partial / approximation (the most material gaps: `MinerCreateBlock` and `StateCall` for EVM contracts, both pending the FVM bridge in Phase 8)
- 1 is a hard-gated stub (`SyncSubmitBlock` — never publishes without explicit `AllowBlockSubmit=true`)

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

Full FVM bridge is Phase 8. The shell is enough for: wallet sends, gas estimation, mempool participation, `StateWaitMsg`, and most of Curio's read-path operations. It is **not** enough for: block production, EVM contract execution, complex multisig operations.

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
