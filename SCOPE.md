# Lantern — Scope

A Filecoin light node that boots in minutes with ~1 GB instead of 76 GB,
verifying the chain cryptographically end-to-end with no remote RPC trust.

The name fits Filecoin's natural-element family (Lotus, Forest, Aurora, Spark).
A lantern is portable light — exactly what a light client is.

Targets three user classes: wallet users, deal clients, storage providers.

## The Insight

Filecoin state is content-addressed IPLD. You don't need the whole HAMT to
verify state about one actor. You need:
- The verified state root CID (from headers + F3)
- A proof path from root to that actor (~4-8 HAMT nodes, ~KB)
- The actor's state object

If you have a trusted state root, any peer can serve you a HAMT branch and you
verify it locally by hash. The peer cannot lie. This is exactly how Ethereum
light clients (Helios, LES) work, and Filecoin's IPLD substrate makes it
arguably cleaner.

## V1 Scope (the ~1 GB local node)

### Boot data (downloaded once, ~600 MB - 1 GB)

| Component             | Size      | Source                              | Verification              |
|-----------------------|-----------|-------------------------------------|---------------------------|
| Block headers genesis→head | ~380 MB   | libp2p header sync, or HTTPS bundle | BLS signatures, parent CIDs |
| F3 finality certificates  | ~80 MB    | F3 GPBFT certificate stream         | BLS aggregate sig + power table |
| DRAND beacon entries      | ~110 MB   | DRAND public network                | DRAND BLS verification    |
| Actor code (all network versions) | ~45 MB | Built-in actor manifest CIDs        | CID match against headers |
| Power tables at finality boundaries | ~50 MB | Derived from full node, verifiable | Reproducible from state    |

This gets you a fully-validated chain head with no state. ~2-5 min download
on a decent connection. ~2 min CPU to verify all BLS sigs in parallel.

### State on demand

When the user asks anything that touches actor state, the node:

1. Looks up the current state root from verified headers
2. Computes the HAMT path for the target actor address
3. Fetches HAMT nodes along that path from peers (Bitswap, or HTTP gateways)
4. Verifies each node's CID against the parent link
5. Reads the actor state, executes the query
6. Caches every fetched node locally

Cache grows with usage but stays small for typical workloads. A wallet user
touches ~5-10 actors over months. An SP touches more (their own miner actor,
their deal partners, market actor, power actor, reward actor) but still
bounded.

### Local cache budget

- Soft cap: 1 GB by default
- LRU eviction on HAMT nodes (re-fetchable)
- Pinned: wallet-owned actors, user-declared interests (their SP, their deals)

## User Classes

### Class A: Wallet user
- Use case: send / receive FIL, check balance, sign messages
- Touches: 1-3 actor accounts
- Cache size: <50 MB
- Boot data: 600 MB
- Network: occasional state fetches on demand, sub-second per query

### Class B: Deal client (filcrate users, app developers)
- Use case: make deals, query deal status, find SPs
- Touches: market actor, multiple miner actors, payment channels
- Cache size: 200-500 MB
- Boot data: 600 MB
- Network: more state fetches; can prefetch SP catalog in background

### Class C: Storage Provider
- Use case: run a Curio cluster or boost node WITHOUT a local 76 GB Lotus
- Touches: own miner actor, power actor, reward actor, market actor, FIL+ verifier
- Cache size: 500 MB - 2 GB
- Boot data: 1 GB (includes extra power table history)
- Network: high read load for active SP; needs aggressive prefetch
- **This is the killer use case.** SPs today need a full Lotus just for chain
  reads. If Hypersync can serve Curio's `ChainApiInfo` shape, an SP can run
  Hypersync + Curio + Yugabyte with no Lotus at all. That's a step-change in
  hardware cost and operational complexity for the SP ecosystem.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ Hypersync Node                                                    │
│                                                                   │
│  ┌─────────────────────┐    ┌────────────────────────────────┐   │
│  │ Lotus-compat RPC    │    │ CLI / wizard                   │   │
│  │ (subset)            │    │ (hypersync init/wallet/etc)    │   │
│  └──────────┬──────────┘    └──────────┬─────────────────────┘   │
│             │                          │                          │
│  ┌──────────▼──────────────────────────▼─────────────────────┐   │
│  │ Query layer (StateGet, ChainHead, MpoolPush, etc.)         │   │
│  └──────────┬─────────────────────────────────────────────────┘   │
│             │                                                     │
│  ┌──────────▼──────────────────────────────────────────────────┐  │
│  │ State accessor (HAMT walker + proof verifier)                │  │
│  │  - given (stateRoot, addr) → actorState                      │  │
│  │  - fetches missing nodes from network                        │  │
│  │  - verifies every fetched node by CID                        │  │
│  └──────┬─────────────────────────────┬───────────────────────┘  │
│         │                             │                            │
│  ┌──────▼──────────┐    ┌─────────────▼──────────────────┐        │
│  │ Local cache     │    │ State fetcher                  │        │
│  │ (BadgerDB CAR)  │◄──►│ (Bitswap + HTTP fallback)      │        │
│  └─────────────────┘    └────────────────────────────────┘        │
│                                       ▲                            │
│  ┌──────────────────────────────────────────┴─────────────────┐  │
│  │ Trusted root state                                          │  │
│  │  - Header chain validator                                   │  │
│  │  - F3 cert verifier                                         │  │
│  │  - DRAND beacon verifier                                    │  │
│  │  - Outputs: (epoch, tipsetCID, stateRoot) trusted tuple     │  │
│  └─────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
                                       ▲
                                       │ libp2p (Bitswap, gossipsub for new heads)
                                       │ HTTPS fallback for boot bundles
                                       ▼
                            ┌────────────────────────┐
                            │ Hypersync gateways     │
                            │ (TSE Reiersen infra)   │
                            │  - Full Forest nodes   │
                            │  - Aggressive Bitswap  │
                            │  - HTTP state proxy    │
                            │  - Boot bundle CDN     │
                            └────────────────────────┘
```

## Trust model

- **Trust roots:** genesis CID (hardcoded), F3 power tables (initially from
  genesis power, follows F3 cert chain forward).
- **Verifiable:** every header, every F3 cert, every DRAND beacon, every state
  node. No trust in serving peers.
- **Liveness assumption:** at least one honest peer is serving the state CIDs
  you ask for. Worst case is unavailability, never wrong data.

This is strictly better than the current "trust your RPC provider" model that
most lightweight Filecoin tooling falls back to today.

## Open Questions to Resolve Early

1. **Header bundle distribution.** Do we serve a single HTTPS-fetched header
   archive at boot (fastest), or only libp2p-stream from peers (purist)? V1: both, prefer HTTPS for speed.

2. **State serving from full nodes today.** Lotus and Forest expose Bitswap;
   serving full historical state via Bitswap is slow. We may need to operate
   one or two "Hypersync gateway" nodes that aggressively cache hot state.
   This is the "Pi-hole / public-DNS for Filecoin state" pattern.

3. **Write path.** Sending messages: trivial, MpoolPush is just gossipsub.
   Local signing: lifted from Lotus wallet code. No state needed.

4. **SP-specific:** Curio expects a `FULLNODE_API_INFO`. We expose a Lotus-
   compatible RPC subset. Need to confirm exact method coverage Curio uses;
   we already know it queries chain head, state at tipset, etc. Probably
   ~30-50 RPC methods to implement for full Curio compatibility.

5. **Re-org handling.** If the chain reorgs past our latest header, we need
   to detect, rewind, refetch. F3 finality bounds the re-org window to ~30s
   of recent epochs.

## Roadmap

| Phase    | Weeks | Goal                                                    |
|----------|-------|---------------------------------------------------------|
| Phase 0  | 1     | Repo + research dive, lift code paths from Lotus/Forest |
| Phase 1  | 2     | Header sync + F3 verifier → produce trusted state root  |
| Phase 2  | 2     | HAMT walker + state fetcher + verifier                  |
| Phase 3  | 1     | Local wallet + sign + send (read-modify-write loop)     |
| Phase 4  | 1     | Lotus-compat RPC subset (read methods first)            |
| Phase 5  | 1     | CLI wizard + installer UX                               |
| Phase 6  | 1     | Hypersync gateway infrastructure on Hetzner             |
| Phase 7  | 1     | Curio compatibility pass + SP testing                   |
| Phase 8  | 2     | Polish, public release, docs, hypersync.io              |

**Total: ~12 weeks for V1.** With slack and parallel work, realistically 8-10
focused weeks.

## Risks

- **State availability:** if no honest peer serves a state CID we need, we're
  stuck. Mitigation: run our own Hypersync gateways.
- **F3 light client maturity:** F3 is recent; the cert-chain verification path
  is less battle-tested than full Lotus. Mitigation: lift from go-f3 reference
  implementation, write extensive tests against mainnet.
- **Curio compatibility surface:** if Curio relies on a Lotus RPC method we
  haven't implemented, SPs can't switch over. Mitigation: instrument a real
  Curio cluster (sp.reiers.io) to log every RPC call, then implement that
  list.
- **Brand confusion:** Old working name "Hypersync" collided with Envio's
  HyperSync (Ethereum multichain indexer). Renamed to Lantern, clean check
  against Filecoin/IPFS/major crypto namespaces.

## Deliverables for V1

- Single static Go binary `lantern` (no CGo, runs anywhere)
- `lantern init` interactive wizard
- `lantern wallet [new|import|list|balance|send]`
- `lantern rpc` (Lotus-compat RPC server on 127.0.0.1:1234)
- `lantern gateway` mode (run-your-own-gateway for SPs / power users)
- One curl-able boot bundle at `https://bootstrap.lantern.reiers.io/mainnet/latest.bundle`
- Docs at `https://lantern.reiers.io`
- Curio integration tested on calibration first, then mainnet
