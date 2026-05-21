# Lantern — Module Map (Phase 0)

Concrete Go layout for V1. Every package is tagged with its provenance:

- **L** = lift from Lotus (`github.com/filecoin-project/lotus`) at the indicated path.
- **L+sub** = lift skeleton from Lotus, substitute one or more transitive deps
  to drop CGo / filecoin-ffi.
- **F3** = lift from `github.com/filecoin-project/go-f3` (already pure-Go BLS via gnark).
- **GST** = import `github.com/filecoin-project/go-state-types` (no fork needed,
  already pure-Go).
- **GIP** = import `github.com/ipld/go-ipld-prime`, `github.com/ipfs/boxo`, or
  `github.com/filecoin-project/go-hamt-ipld/v3` (all pure-Go).
- **LP2P** = import `github.com/libp2p/go-libp2p` and friends.
- **DRAND** = import `github.com/drand/go-clients` (pure-Go BLS via
  `kyber-bls12381`).
- **NEW** = reimplement, with rough LOC estimate.

Versions pin against the Curio v1.36 / Lotus v1.36 / go-f3 v0.8.13 ecosystem (the
exact set already in use on sp.reiers.io).

The hard rule: **no CGo, no filecoin-ffi, no `lib/sigs/bls` import.** Anything
that pulls those (directly or transitively) gets substituted.

---

## Top-level layout

```
github.com/Reiers/lantern/
├── cmd/lantern/                     # main + cobra/urfave CLI
├── api/                             # Lotus-compat RPC types + interface
├── node/                            # wiring; the "Lantern node" struct
├── chain/
│   ├── types/                       # BlockHeader, TipSet, Message, ...
│   ├── header/                      # header chain validator + store
│   ├── beacon/                      # DRAND verifier
│   ├── f3/                          # F3 cert verifier + power table follower
│   ├── trustedroot/                 # the (epoch, tipsetCID, stateRoot) producer
│   └── actors/                      # actor codec registry (read-only)
├── state/
│   ├── accessor/                    # (root, addr) → ActorState facade
│   ├── hamt/                        # path-fetching HAMT walker
│   ├── amt/                         # path-fetching AMT walker
│   └── cache/                       # local IPLD block cache (Badger)
├── net/
│   ├── libp2p/                      # host, gossipsub, peer discovery
│   ├── bitswap/                     # state block fetcher
│   ├── hsync/                       # HTTPS gateway client (boot bundle + state proxy)
│   └── mpool/                       # MpoolPush over gossipsub
├── wallet/                          # key store + sign (BLS/secp256k1/delegated)
├── crypto/
│   └── bls/                         # pure-Go BLS12-381 (verify + sign)
├── rpc/
│   ├── server/                      # JSON-RPC server (Lotus-compatible)
│   └── handlers/                    # one file per RPC method
├── build/                           # build params, genesis CID, drand chain config
└── internal/
    ├── cbor/                        # cbor-gen integration
    └── log/                         # logging setup
```

Target binary size: <80 MB stripped, single static ELF / Mach-O / Windows EXE.

---

## Package detail

### `chain/types`  — **L** (verbatim)

Path: `github.com/filecoin-project/lotus/chain/types`

Lift verbatim. Used types: `BlockHeader` (`blockheader.go`, 198 LOC),
`TipSet`/`TipSetKey` (`tipset.go`, 258 LOC), `Message`, `SignedMessage`,
`BlockMsg`, `BeaconEntry`, `ElectionProof`, `Ticket`, `BigInt`, `FIL`,
`EthAddress`, `EthHash`. Copy under `lantern/chain/types`, keep CBOR
codegen, attribute at top of each file. LOC pulled: ~1.8k.

### `chain/header`  — **L+sub**

Lifted from Lotus `chain/sync.go` (the `ValidateBlock` body, ~600 LOC of the
1.4k file) and `chain/sync_manager.go`. Drop the parts that compute state and
keep only:

- Parent-pointer linkage (header → parent CIDs)
- Block signature verification (BLS over header bytes)
- BLS aggregate verification over message-meta
- DRAND beacon entry referencing (defers to `chain/beacon`)
- Election proof signature check (BLS over ticket)
- Weight calculation (`chain/store/basefee.go`)

**The substitution:** Lotus uses `lib/sigs/bls` → `filecoin-ffi`. Lantern
replaces every `sigs.Verify(sig, addr, msg)` call with a dispatcher in
`crypto/bls` (BLS) + `go-crypto` (secp) + `go-keccak` + `go-crypto` (delegated).
See `crypto/bls`.

Storage: a small append-only Badger KV `(epoch, blockCID) → header`. Headers
genesis → head are validated in order on first boot, then incrementally on
gossipsub.

LOC: ~1.5k lifted, ~300 new (storage + boot-bundle ingest).

### `chain/beacon`  — **L+sub**

Lifted from Lotus `chain/beacon/` (~500 LOC). Key types: `RandomBeacon`
interface, `BeaconEntry`, `drand.DrandBeacon`.

Substitution: Lotus' `chain/beacon/drand/drand.go` already uses
`github.com/drand/go-clients` for BLS verification (pure-Go via
`kyber-bls12381`). **No CGo here today.** Lift as-is.

Genesis network drand configuration ships in `build/drand.go` (chain hash,
public key, period, genesis epoch). Lifted verbatim from
`build/drand.go` in Lotus.

LOC: ~600 lifted.

### `chain/f3`  — **F3** (thin wrapper)

Path: `github.com/filecoin-project/go-f3` v0.8.13.

Direct imports (no fork):

- `go-f3/certs`:
  - `certs.FinalityCertificate` — the on-wire cert.
  - `certs.ValidateFinalityCertificates(verifier, network, prevPowerTable,
    nextInstance, base, certs...) (next instance, next power table, chain,
    err)` (`certs/certs.go:91`) — the entire forward-walking verifier we
    need, including BLS aggregate sig check and power-table-diff
    application.
  - `certs.ApplyPowerTableDiffs`, `certs.MakePowerTableCID`.
- `go-f3/blssig`:
  - `blssig.VerifierWithKeyOnG1()` — pure-Go via
    `internal/gnark` (Consensys gnark-crypto) + `go.dedis.ch/kyber/v4`.
    Passes the `gpbft.Verifier` interface.
- `go-f3/certexchange`:
  - libp2p protocol `/f3/certexchange/0.0.1` for pulling cert ranges from
    peers. Used at boot to slurp certs genesis → head.
- `go-f3/manifest`:
  - mainnet/calibration manifests with initial power table, network name,
    bootstrap epoch.

Lantern code in `chain/f3/` is a thin (~300 LOC) wrapper:

- `Follower` struct: holds `currentInstance`, `currentPowerTable`, persists
  `(instance → cert)` to Badger, exposes `LatestFinalizedTipSet() *TipSet`.
- Boot: load mainnet manifest → cert exchange → verify all certs sequentially.
- Steady state: subscribe to `/f3/granite/certificates/<network>` gossipsub
  topic; verify each new cert against current power table; advance.

LOC: ~300 new, all import code.

### `chain/trustedroot`  — **NEW**

The output module of Phase 1. ~400 LOC.

Inputs:

- Latest header chain (from `chain/header`).
- Latest F3 finality cert (from `chain/f3`).

Output: `TrustedRoot { Epoch, TipSetKey, TipSetCID, StateRoot,
ParentMessageReceipts, ParentWeight, F3Instance, F3Cert }`.

Re-org logic: on a new header that doesn't extend our current head, walk back
to common ancestor, replay header validation, expose `OnReorg(oldHead,
newHead)` to invalidate cached state nodes that depend on the abandoned root.

See `TRUSTED-ROOT.md` for the full spec.

### `chain/actors`  — **L+sub**

Lift only the **read-only ABI**:

- `chain/actors/builtin/miner` — miner state accessors. Lotus has these
  versioned by network version (v0…v14). We only need the latest 3-4 to read
  current state; older versions only when answering historical queries.
- `chain/actors/builtin/market` — deal state, balance, etc.
- `chain/actors/builtin/power` — claim/total power.
- `chain/actors/builtin/verifreg` — allocations, claims.
- `chain/actors/builtin/account` — pubkey lookup.
- `chain/actors/builtin/init` — addr → ID map.

These packages are pure-Go in Lotus *except* they import `lib/blockstore`
(uses go-ds-badger; fine) and indirectly pull in `lib/sigs` for some message
helpers (we don't need those helpers).

Plan: copy the `builtin/` tree under `chain/actors/builtin/`, strip dead
imports, run `go vet`. LOC pulled: ~12k, ~80% will be untouched.

Alternative: depend on go-state-types directly, which exposes the same
ABI structures but without Lotus' `cbor.IpldStore` wrappers. Decision deferred
to Phase 2 — start by importing GST and only fork to a local
`chain/actors` if we need the helper methods (`miner.LoadState`, etc.) that
live in Lotus.

LOC: ~2k of glue, the rest is registry.

### `state/hamt`  — **GIP** + **NEW**

Imports:

- `github.com/filecoin-project/go-hamt-ipld/v3` (`hamt.Node`, `Load`,
  `Find`). 1.4k LOC pure-Go. Filecoin actors use HAMT with bit-width 5,
  KECCAK_256 hash (actually `sha256` truncated for v3+).
- `github.com/ipfs/go-cid`, `github.com/ipld/go-car/v2`.

NEW code: a `PathFetcher` that, given a root CID and an address (after
`StateLookupID`), walks the HAMT one node at a time. On every miss in the
local cache, it calls `state.fetcher.Get(cid)` → `bitswap` or `hsync`
HTTP fallback → verifies the returned bytes hash to the requested CID,
inserts into cache, continues.

Critical invariant: **no parsing of state bytes happens before the CID is
verified.** This is what gives us the "no trust in peers" property.

LOC new: ~600.

### `state/amt`  — **GIP**

Imports: `github.com/filecoin-project/go-amt-ipld/v4`. AMTs are used for
miner sectors, message receipts, partition expiries. Same path-fetching
wrapper pattern as HAMT. ~300 LOC new.

### `state/accessor`  — **NEW**

The public surface used by every RPC handler. ~800 LOC.

```go
type Accessor interface {
    GetActor(ctx, root cid.Cid, addr address.Address) (*types.Actor, error)
    LoadMiner(ctx, root, maddr) (miner.State, error)
    LoadMarket(ctx, root) (market.State, error)
    LoadPower(ctx, root) (power.State, error)
    // ... one per builtin actor
}
```

Behind: HAMT walker → actor head CID → actor-specific state loader. All
state loaders are lifted from Lotus' `chain/actors/builtin/*/state.v*.go`.

### `state/cache`  — **NEW**

Local IPLD block store. ~400 LOC.

- Badger v4 KV: `cid.Cid → []byte` plus a `(addr, lastAccess)` index for LRU.
- Soft cap (default 1 GB, configurable). When cap is hit, evict by LRU,
  honoring a pin set (`wallet`, `owner_actor`, `user_declared`).
- Boot-bundle import: a `.car` file from `bootstrap.lantern.reiers.io/...`
  decompressed straight into the cache with hash verification.
- `Has(cid)`, `Get(cid)`, `Put(cid, []byte)`, `Pin(cid)`, `Unpin(cid)`.

Uses `github.com/ipld/go-car/v2` for CAR I/O and `github.com/dgraph-io/badger/v4`
for the KV.

### `net/libp2p`  — **LP2P** + **NEW**

Imports:

- `github.com/libp2p/go-libp2p` v0.47
- `github.com/libp2p/go-libp2p-pubsub` v0.15 (gossipsub)
- `github.com/libp2p/go-libp2p-kad-dht` v0.38 (peer discovery)

NEW (~500 LOC): bootstrap peers list (lift `build/bootstrap/mainnet.pi`
from Lotus), gossipsub topic subscriptions (`/fil/blocks/<network>`,
`/fil/msgs/<network>`, F3 cert topic), connection manager tuning for
~50 peers max.

### `net/bitswap`  — **GIP** + **NEW**

Imports:

- `github.com/ipfs/boxo/bitswap/client`
- `github.com/ipfs/boxo/bitswap/network/bsnet`

Lotus uses `bsnet.NewFromIpfsHost(host, bsnet.Prefix("/chain"))` to namespace
bitswap on the `/chain/` prefix (see `node/modules/chain.go:38`). We do the
same. ~150 LOC new (mostly wiring + a `Fetcher` adapter that satisfies the
`state/hamt.BlockGetter` interface).

### `net/hsync`  — **NEW**

HTTPS fallback for state fetches. ~400 LOC.

- Client: `GET https://gateway.lantern.reiers.io/state/<cid>` returns the raw
  IPLD block. We hash-verify locally before insertion.
- Boot bundle: `GET https://bootstrap.lantern.reiers.io/mainnet/<epoch>/bundle.car.zst`
  with manifest pinning (manifest itself is signed by a Lantern release key).
- Configurable allow-list of gateways (default: ours; users can self-host).

This is the "Pi-hole for Filecoin state" mentioned in `SCOPE.md` open
question 2.

### `net/mpool`  — **NEW**

~200 LOC. Wraps a gossipsub publisher on `/fil/msgs/<network>`. No local
mpool — we trust the network's full nodes to actually include our messages.

`MpoolPushMessage` flow: sign locally → encode → publish → return CID. The
sender then polls `StateSearchMsg` (which itself walks message-receipts AMT
under the verified state root) until inclusion.

### `wallet`  — **L+sub**

Lift Lotus' `chain/wallet/` and `chain/wallet/key/`. ~500 LOC.

- `wallet.LocalWallet` with a `keystore` interface.
- Key store options: encrypted JSON file (default), OS keychain (later).
- Sign dispatch → `lib/sigs` (replaced).

Substitution: replace `lib/sigs/bls` import path with `lantern/crypto/bls`.
`lib/sigs/secp` and `lib/sigs/delegated` carry over verbatim — no CGo there.

### `crypto/bls`  — **F3** (transitive) / **NEW** (~150 LOC of glue)

Pure-Go BLS12-381 signing + verification.

Backend options ranked by preference:

1. **`github.com/filecoin-project/go-f3/internal/gnark` + `go.dedis.ch/kyber/v4`**
   — exactly what go-f3 uses. But `internal/gnark` is, well, internal. We'd
   have to fork or vendor a copy. Acceptable but ugly.
2. **`github.com/consensys/gnark-crypto/ecc/bls12-381`** — Consensys'
   reference pure-Go BLS12-381. Used by go-f3 under the hood. Implement the
   Filecoin DST manually:
   `BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_`. ~150 LOC for sign + verify +
   aggregate-verify, plus tests against Lotus' filecoin-ffi reference vectors.
3. **`github.com/drand/kyber-bls12381`** — also pure-Go, used by drand client.
   Mature.

Decision: ship option 2 (gnark-crypto directly) for full control of DST and
aggregate-sig shape. Option 1 / option 3 are fallbacks if any edge case bites.

This package satisfies the `sigs.SigShim` interface from `lib/sigs/sigs.go`
so the wallet drop-in works.

LOC: ~150 new.

### `api`  — **L** (verbatim subset)

Path: `github.com/filecoin-project/lotus/api`.

Lift `api_full.go`, `api_common.go` and `api_errors.go`. Strip every method
**outside** the Curio surface (see `CURIO-RPC-SURFACE.md`). What remains is
~70 methods plus shared types (`MinerInfo`, `MsgLookup`, `MinerPower`,
`Partition`, `Deadline`, `MarketBalance`, ...).

Most of those types already live in `chain/types` or `chain/actors/builtin`.
The `api` package becomes ~500 LOC of method signatures + a few thin
result-shape structs.

### `rpc/server`  — **L** (skeleton)

Lift `github.com/filecoin-project/lotus/api/v1api` + `cli/util/api.go` JSON-RPC
plumbing. Uses `github.com/filecoin-project/go-jsonrpc` (pure-Go, already
imported by Curio).

Bind: `127.0.0.1:1234` by default, JWT auth lifted from Lotus
`node/impl/common/api.go` (`AuthNew`/`AuthVerify`). ~400 LOC lifted, ~100 LOC
new.

### `rpc/handlers`  — **NEW** (with **L** inspiration)

One Go file per RPC method, each ~30-80 LOC. Each handler:

1. Resolves the tipset key against the trusted root layer.
2. Calls `state/accessor` for any state read.
3. Returns the Lotus-shape response.

For `MpoolPush`, `MpoolPushMessage`, `WalletSign*`, `SyncSubmitBlock`,
`MinerCreateBlock`: route to `wallet` + `net/mpool` instead.

For methods that are pure computations on already-fetched state
(`GasEstimateMessageGas`, `StateCall`): port Lotus' `node/impl/full/gas.go`
and `chain/stmgr/call.go` (this is the heaviest port; see Phase 4 risk note
below).

LOC: ~3-4k new across ~70 files.

### `build`  — **L** (subset)

Lift only the relevant constants:

- `build/genesis.go`: mainnet genesis CID, calibration genesis CID.
- `build/params_*.go`: epoch params (block delay, network version schedule).
- `build/drand.go`: drand chain configuration.
- `build/bootstrap/mainnet.pi`: libp2p bootstrap peer list.
- `build/buildconstants/upgrade_schedule.go`: network upgrade epochs.

~1k LOC lifted, mostly data.

---

## What we explicitly do NOT lift

- `cmd/lotus*/` — replaced by `cmd/lantern`.
- `chain/stmgr/` `Compute*` — Lantern does not execute messages. Exception:
  `Call` (read-only VM eval) needs a subset of `call.go`; deferred to Phase 4.
- `chain/vm/` — no message execution in V1.
- `chain/store/` — replaced by `chain/header` (header-only).
- `chain/exchange/`, `chain/messagepool/`, `chain/gen/`, `markets/`,
  `paychmgr/`, `node/modules/lp2p/`, `metrics/` — all replaced by lighter
  Lantern-native equivalents or out of V1 scope.
- `extern/filecoin-ffi/`, `lib/sigs/bls/` — replaced (CGo-free constraint).

---

## Dependency budget

Direct deps (Lantern → external):

| Module                                                       | Use                                  | CGo? |
|--------------------------------------------------------------|--------------------------------------|------|
| `github.com/filecoin-project/lotus`                          | type lifts only (verbatim)           | indirect via ffi — must `replace` or strip |
| `github.com/filecoin-project/go-state-types`                 | actor ABI                            | no   |
| `github.com/filecoin-project/go-f3`                          | F3 certs + BLS                       | no   |
| `github.com/filecoin-project/go-hamt-ipld/v3`                | HAMT walker                          | no   |
| `github.com/filecoin-project/go-amt-ipld/v4`                 | AMT walker                           | no   |
| `github.com/filecoin-project/go-address`                     | address codec                        | no   |
| `github.com/filecoin-project/go-jsonrpc`                     | JSON-RPC server + client             | no   |
| `github.com/filecoin-project/go-crypto`                      | secp256k1                            | no   |
| `github.com/filecoin-project/go-keccak`                      | delegated addrs                      | no   |
| `github.com/consensys/gnark-crypto`                          | BLS12-381 verify/sign                | no   |
| `github.com/drand/go-clients`                                | drand beacon verify                  | no   |
| `github.com/ipfs/boxo`                                       | bitswap client                       | no   |
| `github.com/ipld/go-car/v2`                                  | boot bundle CAR I/O                  | no   |
| `github.com/ipld/go-ipld-prime`                              | block decoding                       | no   |
| `github.com/libp2p/go-libp2p` + pubsub + kad-dht             | networking                           | no   |
| `github.com/dgraph-io/badger/v4`                             | local cache + header store           | no   |
| `github.com/urfave/cli/v2`                                   | CLI                                  | no   |
| `github.com/ethereum/go-ethereum/common/hexutil` (maybe)     | eth shim if we expose `eth_*` later  | no   |

**Trap:** importing `github.com/filecoin-project/lotus` pulls
`filecoin-ffi` transitively. We have three options:

1. `go.mod` `replace github.com/filecoin-project/filecoin-ffi =>
   ./internal/ffi-shim` — a tiny stub package that satisfies the import
   graph but is never called. Lotus uses ffi via interfaces in `lib/sigs/bls`
   only, so as long as we never import that package, the shim is fine.
2. Don't import Lotus at all — copy the small set of files we need into
   `chain/types/`. Cleaner long-term, but ~3k more LOC under Lantern
   maintenance.
3. Hybrid: do (2) for the hot paths (`types`, `lib/sigs/sigs.go`), keep (1)
   as escape valve for actor-builtin packages where Lotus has years of
   careful version handling we'd rather not duplicate.

Decision: start with hybrid (3). Revisit if the shim leaks.

---

## LOC summary

|Pkg              | Lifted | New   | Notes                                          |
|-----------------|--------|-------|------------------------------------------------|
| chain/types     | 1800   | 50    | verbatim                                       |
| chain/header    | 1500   | 300   | strip state-exec from sync.go                  |
| chain/beacon    | 600    | 50    | drand                                          |
| chain/f3        | 0      | 300   | thin wrapper over go-f3                        |
| chain/trustedroot| 0     | 400   | new                                            |
| chain/actors    | 12000  | 200   | mostly registry                                |
| state/hamt      | 0      | 600   | over go-hamt-ipld                              |
| state/amt       | 0      | 300   | over go-amt-ipld                               |
| state/accessor  | 0      | 800   | facade                                         |
| state/cache     | 0      | 400   | badger + LRU + pins                            |
| net/libp2p      | 200    | 500   | host wiring                                    |
| net/bitswap     | 0      | 150   | adapter                                        |
| net/hsync       | 0      | 400   | HTTPS gateway client                           |
| net/mpool       | 0      | 200   | gossipsub publish                              |
| wallet          | 500    | 100   | sub BLS backend                                |
| crypto/bls      | 0      | 150   | gnark + Filecoin DST                           |
| api             | 500    | 100   | subset                                         |
| rpc/server      | 400    | 100   | jsonrpc + JWT                                  |
| rpc/handlers    | 200    | 3500  | ~70 methods × ~50 LOC                          |
| build           | 1000   | 100   | constants                                      |
| cmd/lantern     | 0      | 800   | CLI: init, wallet, rpc, gateway                |
| **Totals**      |**~18.7k**|**~8.9k**| ~28k Go LOC total                           |

Order of magnitude smaller than Lotus (~250k), comparable to Forest's
chain crate. Tractable for one engineer.

## Phase mapping

| Phase | Packages built |
|-------|----------------|
| 0 | this doc |
| 1 | crypto/bls, chain/types, chain/header, chain/beacon, chain/f3, chain/trustedroot |
| 2 | state/{hamt,amt,cache,accessor}, net/{libp2p,bitswap,hsync}, chain/actors |
| 3 | wallet, net/mpool |
| 4 | api, rpc/server, rpc/handlers (read first, then write) |
| 5 | cmd/lantern (CLI wizard) |
| 6 | gateway server (`cmd/lantern-gateway`, shares core) |
| 7 | Curio compatibility hardening + missing handlers |
| 8 | docs + release |
