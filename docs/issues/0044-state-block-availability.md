# lantern#44 — Embedded state-block availability for local eth_call

**Goal:** Reach a true `--vm-bridge-rpc-disable` in embedded curio-core (and
filcensus). Every `eth_call` served from local FEVM execution, zero Glif.

**Engine status (recap):** lantern#43 Stages 1-5 done, v1.6.0→v1.6.4 live on
cc-smoke. PDPVerifier reads serve locally. The only remaining bridge
fallbacks are `block not found` on KAMT storage nodes, i.e. the embedded
Bitswap blockstore hasn't fetched a storage-trie node at the live head yet.

This is a **block-availability** problem, not an EVM / KAMT problem.

## Design

Two complementary mechanisms, both behind config knobs, both safe under
embedded mode where libp2p may be off:

### A. Synchronous fetch-on-miss retry (cheap, immediate robustness)

Today the eth_call backend sees a single `BlockGetter.Get` failure and
falls back to bridge. The combined fetcher's race tier has a 1.5s fast
deadline + 5s full Bitswap deadline; a cold storage-trie block under load
can miss that first window. Add a small retry/extend layer specifically
for KAMT-node fetches during eth_call.

- Retry config: `LocalFEVMFetchRetries int` (default 2),
  `LocalFEVMFetchTimeout time.Duration` (default 8s total budget per CID).
- Implementation: a thin `retryingBlockGetter` wrapper used **only** in
  the eth_call backend (vm/evm Backend), not in normal accessor walks
  (so we don't inflate latency on hot-path proof loop reads).
- Semantics: when underlying Get returns an error, retry up to N times
  with a fresh context bounded by the total timeout. Re-check the cache
  between retries (a concurrent prefetch may have filled it).

### B. Targeted state-block prefetch on head advance

Hook `chain/header/store.Store.OnHeadChange` and, for a configured set
of contract addresses, walk:

1. address → Filecoin ID/delegated addr (already in evmexec.go)
2. accessor.GetActor at the new head → actor.Head (EVM state CID)
3. LoadEVM → bytecode CID + StorageRoot (KAMT root)
4. BFS the KAMT a bounded number of nodes into the local cache

This pulls the cold storage-trie nodes into the cache **before** the
first eth_call needs them, so the EVM backend sees the cache, not a
Bitswap RTT.

- Prefetch config:
  - `LocalFEVMPrefetchAddrs []string` — eth addresses to prefetch.
    Defaults to the known PDPVerifier proxy + FWSS proxy +
    ServiceProviderRegistry proxy + USDFC for the active network when
    `EmbeddedMode=true`.
  - `LocalFEVMPrefetchMaxBlocksPerAddr int` (default 256) — BFS bound.
  - `LocalFEVMPrefetchTimeout time.Duration` (default 20s per head
    advance, per address).
  - `LocalFEVMPrefetchMinInterval time.Duration` (default 2 epochs ≈
    60s) — coalesce rapid head advances so a long catch-up doesn't
    fan out one walk per epoch.
- Cost bound: each contract prefetch is capped to N storage-trie nodes
  + bytecode + actor head. With ~256 nodes/address and 4 default
  addresses, that's ~1024 IPLD block fetches per ~60s window worst-case
  cold; once cached, subsequent head advances re-walk the SAME CIDs
  cheap (cache hit).
- Failure mode: prefetch errors are logged at debug and dropped. They
  must never affect head advance. Eth_call retains the bridge fallback
  for as long as one is configured.

### C. Observability

- `evmexec.go` already logs local-exec faults at debug. Add a counter:
  `local_eth_call_total`, `local_eth_call_local_served`,
  `local_eth_call_bridge_fallback`, `local_eth_call_kamt_miss`. Expose
  via the existing handler stats path (mirror Combined.Fetcher.Stats
  shape).
- Prefetcher exposes counters: `prefetch_runs`,
  `prefetch_blocks_walked`, `prefetch_errors`.

## Out of scope (this issue)

- Read-head-N-back (option 3 in the issue). This is a behavior change
  that's harder to reason about for paying-callers like FilecoinPay
  views; defer until we see real evidence A+B aren't enough.
- Eth_estimateGas local exec.
- Precompiles beyond what the Stage-3 interpreter already covers.

## Acceptance

1. cc-smoke runs curio-core with `--vm-bridge-rpc-disable` and PDP +
   payments read paths complete with **no bridge fallbacks** over a
   sustained run (≥ 1h, ≥ 1 full proving window).
2. Proof loop unaffected; head-advance latency unchanged within noise.
3. No CGo / no filecoin-ffi.
4. Counters expose the fallback-rate so regressions are visible.

## Validation plan

1. Unit tests for `retryingBlockGetter` (succeeds on Nth retry,
   respects total timeout, surfaces underlying error after N).
2. Unit tests for prefetcher (walks bounded depth, coalesces rapid
   head advances, head-advance does not block prefetch).
3. Integration test against live calibration Glif as the network
   source: a fresh in-memory cache, run prefetch for PDPVerifier
   proxy, then issue 5 read calls (`FIL_CLEANUP_DEPOSIT`,
   `dataSetLive`, `getNextChallengeEpoch`, etc.) and assert
   `bridge_fallback == 0`.
4. cc-smoke soak: deploy, flip `--vm-bridge-rpc-disable` after the
   prefetch warms, watch counters for 1h+ + 1 proving window.

## Sequencing

1. Counters + retry wrapper (smallest, immediate value, low risk).
2. Prefetcher + OnHeadChange hook + config plumbing.
3. Defaults wiring for embedded mode (curio-core path).
4. cc-smoke soak with bridge disabled.
</content>
</invoke>