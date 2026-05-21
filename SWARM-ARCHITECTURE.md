# Swarm architecture

How Lantern scales without depending on any central infrastructure, including ours.

## The thesis

Filecoin is already a peer-to-peer network. Every Lotus, Forest, and Curio node runs libp2p, speaks Bitswap, and participates in the same swarm. Lantern's correct architectural place is **inside that swarm**, not in front of it via a central HTTP gateway.

Content-addressing makes this work safely. Every IPLD block has a CID; any peer can serve bytes claiming to be at that CID; Lantern hashes what arrives and accepts only what verifies. **Lying is detectable. One honest peer is sufficient.**

This document specs what V1.2 implements to make Lantern a first-class swarm participant.

## What changes

V1.1 (current alpha):
```
Lantern → HTTP gateway (gateway.lantern.reiers.io) → Glif proxy → real data
```

V1.2 (this document):
```
                    ┌─ libp2p Bitswap peers ──┐
                    │   (any honest node)     │
Lantern ────────────┤                         ├─── real data
                    │   - SPs running Curio   │
                    │   - Lantern beacons     │
                    │   - Forest / Lotus ops  │
                    │   - third-party gateways│
                    └─────────────────────────┘
                              │
                    HTTP gateway (last-resort fallback)
```

The HTTP gateway never goes away — it stays as a cold-start convenience and a denial-of-service fallback. But it stops being the primary path.

## Why this is safe

Content-addressed data has a property that token-based consensus does not: **correctness does not require quorum**.

For polled consensus (e.g., "ask 5 RPC providers, take the majority"):
- You need ≥ majority of honest sources
- Adversary needs to control majority to lie
- Lying changes the response you receive

For content-addressed Lantern state queries:
- Lantern computes the target CID from BLS-verified chain headers
- Any peer claiming to serve that CID must produce bytes whose hash matches
- Only the correct bytes hash correctly
- An adversary can refuse to serve (denial of service), but cannot serve a lie
- **One honest source is sufficient for correctness**

Multiple peers improve **availability** and **speed** (parallel requests, geographic distribution, load shedding) but they are not needed for **correctness**. This is the same property that lets Bitcoin SPV clients trust headers from any peer that produces a valid PoW chain.

## What we ship in V1.2 (Phase 10 — DELIVERED)

### 1. Activate Bitswap properly — ✅ DELIVERED

Phase 2's `net/bitswap` stub has a 100ms timeout because mainnet bootstrap peers don't reliably serve historical state CIDs. Phase 10 work:

- Increase default Bitswap timeout to 3–5 seconds with progressive deadlines (try fast peers first, escalate)
- Add a Bitswap session reuse layer so successive lookups in the same query (HAMT walk = 4-8 sequential blocks) share a session and warm-pool of peers
- Tune Bitswap WANT-HAVE / WANT-BLOCK negotiation for HAMT walks (small, sequential, deterministic CIDs)

Phase 10 lifted `github.com/ipfs/boxo/bitswap` as Lantern's primary fetch path. The daemon now runs a real Bitswap client wired against the live libp2p host (`net/libp2p`), with progressive deadlines: 1.5s for the preferred-peer fast stage, 5s for the full-swarm broadcast. Sessions reuse warm peer pools for HAMT-walk patterns.

The combined fetcher order in `cmd/lantern daemon` is now:

1. local in-memory cache (`state/hamt.MemBlockStore`)
2. **Bitswap from preferred peers** (`--bitswap-peers` multiaddrs, 1.5s fast deadline)
3. **Bitswap from full swarm** (5s deadline, broadcast WANT-HAVE)
4. HTTP gateway (last-resort fallback, 5s)
5. Glif RPC (deep fallback, 20s)

Each layer's success short-circuits the rest. Operators can disable Bitswap with `--bitswap=false` (e.g. for fully offline cache-warming tests). The `--metrics` Prometheus endpoint exposes per-layer hit counts (`lantern_fetch_total{source=...}`) plus Bitswap-specific block/byte counters.

### 2. State-serving "beacon" nodes — ✅ DELIVERED (purpose-built variant)

A **Lantern beacon** is a libp2p node that aggressively serves recent state CIDs to the swarm. It is not Lantern-specific: it's a Forest or Lotus configured to participate as a Bitswap provider for state CIDs.

Two flavors:

**Heavyweight beacon** (Forest/Lotus operator):
- Configure existing Forest or Lotus to announce state CIDs aggressively to Bitswap
- One config flag: `BitswapAdvertise=true` in Forest, or `ProvideEnabled=true` in Lotus
- Pre-existing infrastructure, near-zero marginal cost for the operator
- Targets: ChainSafe, Glif, Protocol Labs Storacha, large SPs running Curio

**Lightweight beacon** (purpose-built) — ✅ SHIPPED:
- `lantern beacon` subcommand: read-only Lantern node that aggressively caches state CIDs and serves them via Bitswap
- ~5 GB disk for hot state cache (Badger v4, pure Go), ~2 GB RAM
- Single-binary, no Forest or Lotus required
- Run on a Hetzner VM for €5/mo
- Target: community operators, third-party RPC providers, security researchers

Usage:

```sh
lantern beacon \
    --cache-dir /var/lib/lantern-beacon \
    --cache-size 5GiB \
    --listen /ip4/0.0.0.0/tcp/4001,/ip4/0.0.0.0/udp/4001/quic-v1 \
    --dht-announce \
    --metrics 127.0.0.1:4711
```

The beacon joins the swarm, announces `lantern/beacon/v1` in the DHT, persistently caches blocks via Badger, and serves Bitswap from cache. Cache-miss backfill from an upstream gateway is scaffolded (`--gateway`) pending upstream Bitswap wantlist subscriber API.

We will run 2-3 of these on lex / Hetzner once Phase 10 lands in production so the network has always-available beacons. Third parties operate their own.

### 3. DHT-based beacon discovery — ✅ DELIVERED

Phase 6 already shipped Kademlia DHT (`net/libp2p/dht.go`). V1.2 uses it for beacon discovery:

- Each beacon advertises a well-known service rendezvous: `lantern/beacon/v1` (the constant `BeaconRendezvous` in `cmd/lantern/beacon.go`)
- Beacons re-advertise every TTL/2 (or hourly fallback) so stale entries don't pile up
- Lantern clients can query the DHT for that rendezvous and use the returned `AddrInfo` list as their `--bitswap-peers` preferred set. (Client-side automatic discovery loop is a V1.3 polish.)
- No central registry. No coordination needed. Beacons come and go organically.

### 4. Fallback hierarchy

Per-block fetch path, in order:

1. **Local Badger cache** (instant, ~80% hit rate after warmup)
2. **Bitswap from preferred beacons** (3-5 fastest known beacons, parallel WANT-BLOCK)
3. **Bitswap from full swarm** (broadcast WANT, anyone with the block can serve)
4. **HTTP gateway** (`gateway.lantern.reiers.io` and any configured alternates, last resort)

Each layer's success short-circuits the rest. Cold-start cost is dominated by layer 4 the first time (1-2 sec to gateway round trip) and by layer 2-3 thereafter (<500ms when beacons are warm).

### 5. Operator onboarding

If you run a Forest, Lotus, or Curio cluster, becoming a Lantern peer is one config flag:

```toml
# Forest:
[network]
bitswap_advertise = true

# Lotus:
[Libp2p]
ProvideEnabled = true
```

Both already do this when configured. We document it as "supporting the Lantern light-client network" in the operator guide. Costs operators nothing — they already have the bytes, they're already on libp2p — they just toggle whether they answer Bitswap requests for state CIDs.

For purpose-built beacons:

```sh
./lantern beacon \
  --cache-dir /var/lib/lantern-beacon \
  --cache-size 5GiB \
  --listen /ip4/0.0.0.0/tcp/4001 \
  --dht-announce
```

That's the whole config. Beacon joins the swarm, announces itself via DHT, starts caching and serving state CIDs. No central registration. No API key. No coordination with us.

## How this scales

**100 users:** existing HTTP gateway handles it fine. V1.2 changes nothing visible to users.

**1,000 users:** Bitswap layer starts paying back. ~80% of queries hit local cache, ~15% hit Bitswap from a beacon, ~5% fall through to HTTP gateway. Gateway load drops by 95% relative to V1.1 architecture.

**10,000 users:** Gateway is essentially idle except for cold-start. Beacons handle the bulk via Bitswap. If we run 5 beacons and ChainSafe runs 5 and a few SPs run their own, the network has 10-15 well-connected beacons globally, plenty for the load.

**Filecoin-ecosystem-scale (every SP, every dApp, every wallet using Lantern):** Network self-organizes. Beacons emerge from the SP population because SPs already have the data on disk. DHT scales horizontally. No single org is the bottleneck.

**Adversarial scale:** If someone tries to DDoS Lantern by spamming our gateway, the gateway falls over, and *the network keeps working* because Lantern clients fall through to Bitswap automatically. There is no single point of failure to attack.

## Trust properties

Same as V1.1, but worth restating in the swarm context:

- **Headers and F3 finality:** verified locally from BLS signatures. Source of headers is irrelevant.
- **State CIDs:** verified locally by hash. Source of bytes is irrelevant.
- **DRAND beacons:** verified locally against DRAND public keys. Source is irrelevant.
- **The embedded F3 anchor:** the one soft trust point. V1.2 adds optional install-time cross-validation against multiple beacons (refuse to start if 3+ beacons disagree on the anchor's correctness). This is *additional* defense in depth, not a replacement for reproducible builds.

The swarm architecture makes Lantern strictly more robust without weakening any trust property. If anything, it removes the gateway as a soft attack surface: today, an attacker compromising `gateway.lantern.reiers.io` could censor users (refuse to serve their queries), forcing them into errors or stale data. With swarm fallback, those users seamlessly route around the broken gateway and never notice.

## What this is not

- **Not a new consensus mechanism.** We don't poll multiple peers and take majority. We rely on the existing Filecoin consensus that produced the headers and state we're verifying.
- **Not a new IPFS network.** We participate in the existing Filecoin libp2p swarm, same as every other Filecoin node.
- **Not a paid network.** Beacons are volunteer infrastructure. They run because operators already have the data and serving it costs them little.
- **Not a bandwidth subsidy.** A heavy Lantern user pulls maybe 500 MB / session of unique state. At 10k users that's 5 TB / session total — distributed across all the beacons in the swarm, that's negligible per-beacon.

## Roadmap

**V1.1 (Phase 9 work) — SHIPPED:**
- ChainNotify + daemon header-store wiring
- Real Curio binary binding test
- 71/71 RPC method coverage

**V1.2 (Phase 10) — SHIPPED:**
- Bitswap fully activated as primary fetch path (Part B)
- `lantern beacon` subcommand (Part C)
- DHT beacon rendezvous (`lantern/beacon/v1`) (Part C)
- Live libp2p stats exposed via Curio webui (Part A, Part D)

**V1.3 (Phase 11) — next:**
- Client-side DHT beacon discovery (auto-populate `--bitswap-peers` from the DHT rendezvous)
- Beacon backfill: hook into upstream Bitswap wantlist to pull missing CIDs from the gateway proactively
- Install-time anchor cross-validation against 3+ beacons
- Multi-gateway fallback in HTTP layer (operator-configurable alternates)
- Metrics dashboard showing swarm health, beacon count, hit rates

**V2 (Phase 12+):**
- Optional encrypted-private state caching (for users who want forward secrecy on their query patterns)
- Federated gateway registry via signed DNS or DHT
- Bandwidth incentivisation experiment (FIL micropayments for serving state — only if community wants it; the swarm works fine as volunteer infrastructure)

## Bottom line

V1.1 ships with a central gateway because it's the fastest path to "alpha that works." V1.2 makes that gateway optional. By V2, "the Lantern network" is a property that emerges from the existing Filecoin swarm with no central coordinator. Our infrastructure is one beacon among many, indistinguishable to clients from any other honest peer.

This is the right shape for the project. We're not trying to be the next Infura. We're trying to make sure Filecoin doesn't need one.
