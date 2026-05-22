# ❌ StateMinerInfo timing out at 30s even with healthy peer count

**Status:** Open. Reproduces on live mainnet daemon (f03678816 / 192.168.2.32:11234) on `v1.2.1` (commit `665a136`).
**Severity:** Medium. Curio sees `Filecoin.Version` and `Filecoin.ChainHead` fine but cannot ask Lantern anything about state. Effectively blocks Lantern from being a useful Curio secondary for anything beyond chain-head probing.
**Discovered:** 2026-05-21 (afternoon, post-V1.2.0-rc.1 deploy).
**Confirmed not peer-count related:** 2026-05-22 13:00 CPH, after the DHT protocol prefix fix lifted peer count from 4 → 80. State calls still hang.

---

## Symptom

```
$ curl -sS -X POST http://127.0.0.1:11234/rpc/v1 \
    -H 'Content-Type: application/json' \
    --data '{"jsonrpc":"2.0","method":"Filecoin.StateMinerInfo","params":["f03678816",null],"id":1}' \
    --max-time 30

curl: (28) Operation timed out after 30005 milliseconds with 0 bytes received
```

Same hang on `Filecoin.StateMinerPower`. `Filecoin.ChainHead`, `Filecoin.Version`, `Filecoin.NetPeers`, `Filecoin.StateNetworkVersion`, all return in <1s.

The shape of the failure is "hang, never respond" — the RPC handler doesn't return an error, it just doesn't return.

---

## What we've ruled out

1. **Not peer count.** Original hypothesis was that state walks over Bitswap with only 4 peers were too slow. After the DHT-protocol-prefix fix (`665a136`), the daemon sits at 46-80 peers. Calls still hang. Peer count is no longer the bottleneck.

2. **Not the binary tag.** Same hang on `v1.2.0-rc.1-b11_01`, `v1.2.1`, and `665a136`. This regression predates B-11-01.

3. **Not the bootstrap quorum.** Quorum bootstraps cleanly. Chain head is current within 1-12 epochs of Lotus.

4. **Not the gateway.** Lantern's HTTP backfill gateway (`https://gateway.lantern.reiers.io`) is up and serving. Earlier comparator runs (2026-05-21, yesterday) showed StateMinerInfo working at boot when the local Bitswap cache was warm. The deploy that introduced cold-state symptoms is somewhere between yesterday's `61feb6a` and today's later commits.

5. **Not a known F3 / cert-exchange issue.** F3 subscriber is healthy.

---

## What's actually happening (best hypothesis)

The state-read path is `accessor.GetActor(addr, tipset) → state.LoadStateTree(stateRoot) → hamt.Lookup(addr) → IPLD block fetch`. Each `Lookup` traverses the state HAMT, which is a Merkle tree of CBOR blocks. Every internal node is a separate block fetched from the IPLD blockstore. The blockstore is the combined fetcher (cache → bitswap → gateway → glif).

When state is **warm** (yesterday's comparator), the cache layer hits and everything is local. When state is **cold** (most production usage), Lantern walks the HAMT block-by-block over Bitswap or HTTP gateway, with `fast=1.5s` and `full=5s` deadlines per block.

A `StateMinerInfo` query for a miner actor walks ~6-10 HAMT internal blocks + the actor's own state CBOR + a few address-lookup hops. Conservatively 10-15 blocks per query.

At 5s `full` deadline per block, a worst-case cold-cache query is **50-75 seconds**. Curio's RPC client times out at 30s. **The math doesn't fit.**

Two likely contributing factors:

1. **Bitswap broadcast is the wrong primary for sequential cold-block walks.** Each block requires a Bitswap roundtrip to whichever peer has it. With 80 peers and broadcast WANTs, that's a lot of network overhead per block. The gateway fallback at 5s is the actual happy path for cold reads, not Bitswap.
2. **No prefetch / pipeline.** The HAMT walk fetches blocks one at a time, awaiting each before issuing the next. Even at gateway speeds (50-200ms per block), 10-15 sequential fetches = 0.5-3s, which fits in 30s. But there's likely additional latency.

---

## Three concrete next steps to chase, in priority order

### 1. Add timing instrumentation to the state-read path

Wrap `accessor.GetActor` (and its descendants — `LoadStateTree`, `hamt.Lookup`, the underlying `Blockstore.Get`) with `defer time.Since`-style timing. Log per-call:

- which CIDs were fetched
- how long each fetch took
- which layer of the combined fetcher served it (cache / bitswap / gateway / glif)
- total wall time for the StateMinerInfo handler

Sample CLI invocation that doesn't impact normal sync:

```bash
LANTERN_STATE_TRACE=1 /home/reiers/lantern daemon ...
```

This is the cheapest way to know whether we're CPU-bound, network-bound, or stuck on a single slow block.

### 2. Re-order the combined fetcher: gateway first for state-tree blocks

The current order is cache → Bitswap (fast=1.5s) → gateway (5s) → glif. Bitswap-first makes sense for blocks that lots of peers have (recent chain blocks, hot state). It's *worst-case* slow for cold state-tree internal nodes that aren't necessarily widely replicated.

For state-tree HAMT internals, a tiered tag-based ordering would help:

- Tag blocks we know are state-tree internals (we know this because we just walked into them from `StateRoot`) and route them gateway-first
- Tag blocks that are likely message/block payloads (recent epochs) and keep Bitswap-first

Or simpler: when the cache misses on state-tree depth ≥ 2, skip Bitswap and go straight to gateway.

### 3. Prefetch / pipeline the HAMT walk

The HAMT block layout makes prefetching tractable: when we resolve a HAMT branch, we know the next ~8 children's CIDs. We can fire off concurrent `Blockstore.Get` calls for all of them and only block on the one we actually need.

Implementation lives in `state/hamt/` — make `Lookup` start a background prefetch when it discovers a branch.

### 4. (Stretch) Persistent state cache

The Bitswap cache is in-memory. Every daemon restart cold-starts the state read path. A small persistent state cache (BadgerDB, ~100MB) would make repeated `StateMinerInfo` queries against the same miner return in milliseconds. Worth doing for any operator that polls Curio's "Chain Node Network" panel.

---

## Acceptance test for "this is fixed"

```bash
# Cold-cache run (fresh restart of the daemon)
systemctl restart lantern-daemon  # or kill + tmux new-session

# Wait for boot to complete
sleep 30

# Probe StateMinerInfo, must complete < 10s
time curl -sS -X POST http://127.0.0.1:11234/rpc/v1 \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"Filecoin.StateMinerInfo","params":["f03678816",null],"id":1}' \
  --max-time 15

# Probe StateMinerPower, must complete < 10s
time curl -sS -X POST http://127.0.0.1:11234/rpc/v1 \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"Filecoin.StateMinerPower","params":["f03678816",null],"id":1}' \
  --max-time 15
```

When both complete in <10s on a cold cache, this issue closes.

---

## Related files

- `state/accessor/` — top-level state read API
- `state/hamt/` — HAMT walk implementation
- `net/bitswap/` — Bitswap client wrapper, `fast`/`full` deadlines
- `net/combined/` — the multi-tier blockstore fetcher
- `rpc/handlers/state_api.go` (or similar) — StateMinerInfo RPC handler

---

## History

- **2026-05-21:** First noticed during the post-deploy comparator. StateMinerInfo worked at boot when state was warm. After ~14h of uptime, all state calls were timing out. Hypothesized: cold-Bitswap-fetch problem made worse by low peer count.
- **2026-05-22 morning:** Sub-agent shipped DHT walks + connmgr lift to address peer count. Walks fired correctly but routing table stayed at 0 — discovered downstream that DHT protocol prefix was wrong.
- **2026-05-22 afternoon:** Fixed DHT protocol prefix (`665a136`). Peer count jumped 4 → 80. **StateMinerInfo still timed out at 30s.** Confirmed peer count was not the bottleneck. Filed this issue.
