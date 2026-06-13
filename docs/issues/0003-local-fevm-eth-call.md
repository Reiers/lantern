# lantern#3 — Local FEVM execution for `eth_call` (remove the Glif VMBridge dependency)

**Status:** open · **Priority:** P1 (keystone) · **Size:** large (weeks) · **Tracking:** lantern#3

## Problem

Lantern's native VM is **Send-only** (`vm/statecall.go`: *"We don't execute actor
logic... Return bytes are empty for builtin methods... that path will need a
Phase 8 EVM port."*). Any RPC that requires executing actor/EVM bytecode —
`eth_call`, `eth_estimateGas`, `StateCall` against a contract — is forwarded to
an upstream Forest/Lotus node via the **VMBridge** (`vm/bridge/`). In practice
that upstream defaults to **Glif** (`https://api.node.glif.io/rpc/v1`).

This is the single remaining external-trust dependency in the otherwise
self-contained "pure Go, no filecoin-ffi, end-to-end verified" stack. It shows
up identically in two products:

1. **filcensus** (`Reiers/sp-radar`) — the FoC providers section enumerates the
   on-chain `ServiceProviderRegistry` via `eth_call`
   (`activeProviderCount()`, `getAllActiveProviders()`,
   `getProviderWithProduct()`). Today those calls reach Glif through the census
   daemon's VMBridge.

2. **curio-core** (`Reiers/curio-core`) — embeds Lantern in-process via
   `pkg/daemon` and dials its own embedded Lantern's `eth_call` for every
   contract read (PDP verifier state, FilecoinPay/USDFC rails, settlement).
   `cmd/curio-core/main.go` documents this verbatim: *"This is the one
   architectural compromise in the embedded-Lantern story: until Lantern can
   execute FEVM reads from its own state tree (lantern#3 area), curio-core
   forwards eth_call / eth_estimateGas to a public RPC."*

**One root cause, two products.** Solving it once removes Glif from both and
makes the "no external RPC" claim true for the whole stack — the strongest
version of the Lantern marketing story.

## What's already in place (the foundation)

- **Verified state access.** `state/accessor` resolves any address → actor and
  walks the state-tree HAMT from Bitswap-fetched, CID-verified blocks
  (`GetActor`, `LookupID`, full IPLD proof path).
- **Actor registry + versioned decoders.** `state/actors/` decodes power,
  market, miner, verifreg, etc. from CBOR (used by lantern#3 Part A below).
- **Send-only VM scaffold.** `vm/` has message application, gas accounting
  shell, `StateCall` shape compatible with Lotus `InvocResult`.
- **EVM actor state is reachable.** A deployed contract is an EVM actor whose
  Head points at `{ bytecode CID, storage KAMT root }`. Both are fetchable and
  verifiable through the same accessor path.

## Scope split

### Part A — system-actor CBOR decode in `StateReadState` ✅ DONE (this PR)

`StateReadState` now decodes the f04/f05/f06 (power/market/verifreg) head CBOR
into the versioned go-state-types struct, returning Lotus-compatible named
fields instead of an opaque blob. This removes Glif for the **network-truth
headline numbers** (raw/QA power, pledge collateral, deal count, datacap) — no
FEVM required, since these are plain CBOR actor state, not EVM execution.

Shipped in `rpc/handlers/state_readstate_decode.go` + `chain_api.go`. Backward
compatible: unknown actors and EVM contracts fall back to raw bytes.

### Part B — local FEVM `eth_call` (this issue, the hard part)

Execute read-only EVM calls against locally-verified contract state, with no
upstream RPC. Two candidate approaches:

**B1 — Pure-Go EVM interpreter over verified contract storage (recommended).**
- Load the contract EVM actor: bytecode CID + storage KAMT root from state tree.
- Run a pure-Go EVM (e.g. a vetted `go-ethereum/core/vm`-style interpreter,
  CGO-free) with a `StateDB` shim backed by the contract's KAMT storage slots
  (Keccak-keyed AMT reads through the accessor).
- Map Filecoin↔EVM address spaces (f410 / 0x masked-ID) and gas semantics for
  read-only calls (gas can be generously bounded; no state writes).
- Honours the design rule: **pure Go, no filecoin-ffi, no Rust.** A pure-Go EVM
  is explicitly NOT the banned FFI path.

**B2 — Per-contract storage-layout decode (tactical fallback).**
- For a *specific* known contract (e.g. ServiceProviderRegistry), read raw
  storage slots and reimplement its Solidity storage layout in Go (mapping-slot
  keccak math, dynamic arrays, struct packing).
- Pros: no EVM interpreter. Cons: brittle, per-contract, breaks on contract
  upgrade. Only worth it as a stopgap for filcensus FoC if B1 slips.

### Recommendation

Pursue **B1**. It generalises to every contract curio-core and filcensus touch
(PDP verifier, FilecoinPay, FWSS, ServiceProviderRegistry) and is the only
version that makes the embedded-Lantern story truly self-contained. Keep the
VMBridge as an **automatic fallback** behind a config flag during rollout so a
local-exec miss degrades to Glif rather than failing — flip the default to
local-only once parity is proven against a corpus of real calls.

## Acceptance criteria

- `eth_call` for `ServiceProviderRegistry` methods returns byte-identical
  results to Glif for a captured corpus (filcensus FoC enumeration).
- curio-core can run `--vm-bridge-rpc-disable` and still complete its PDP +
  payments read paths.
- No CGo, no filecoin-ffi added. Binary stays pure-Go.
- VMBridge remains as opt-in fallback; local exec is default once at parity.

## Cross-references

- `vm/statecall.go` — "Phase 8 EVM port" TODO.
- `cmd/curio-core/main.go` — the documented Glif compromise.
- curio-core#62 / lantern#33 — head-staleness (separate, already fixed in v1.5.x).
- filcensus: `Reiers/sp-radar` `internal/foc/foc.go` — the eth_call consumer.
