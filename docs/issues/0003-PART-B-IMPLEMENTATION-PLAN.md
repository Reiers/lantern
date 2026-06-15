# lantern#43 Part B — Local FEVM `eth_call` — Implementation Plan

_Sprint plan, written 2026-06-15. Companion to `0003-local-fevm-eth-call.md`.
Approach **B1**: pure-Go EVM over locally-verified contract storage. Pure Go,
CGO_ENABLED=0, no filecoin-ffi, no Rust._

## The shape of the problem

A read-only `eth_call(to, data)` against a deployed contract =

1. Resolve `to` (0x address) → Filecoin EVM actor via the state tree.
2. Load that actor's `evm.State`: `Bytecode cid.Cid` + `ContractState cid.Cid`
   (a `KAMT<U256, U256>` storage dictionary).
3. Fetch + verify the bytecode block (IPLD, from Bitswap/blockstore).
4. Run a pure-Go EVM interpreter with a `StateDB` shim whose `SLOAD(slot)`
   reads `ContractState[keccak-of-slot] -> U256` through the accessor.
5. Return the EVM return bytes (or revert data with the right error shape).

**Lotus cribs nothing here** — Lotus executes FEVM via filecoin-ffi (FVM), the
banned path (confirmed: `chain/vm/execution.go` is all FFI). We build the
interpreter independently. That's the cost and the moat.

## Foundation already in place (reuse, do not rebuild)

- `state/accessor` — address → actor, state-tree HAMT walk, CID-verified blocks.
- `state/hamt`, `state/amt` — IPLD structures + stores.
- `state/actors/` — versioned CBOR actor decoders.
- `rpc/handlers/ethshape.go` — `EthAddressFromFilecoinIDActor` (0xff||0(11)||be64(id)),
  `EthHashFromCid`. Address mapping foundation.
- `go-state-types/builtin/v{8..18}/evm` — the `evm.State` struct (Bytecode,
  BytecodeHash, ContractState, Nonce, Tombstone). Pure-Go CBOR.
- `vm/` — apply/gas/StateCall scaffold + `vm/bridge` (the VMBridge to keep as fallback).

## Missing pieces (what to build)

### Stage 0 — survey + design lock ✅ (this doc)

### Stage 1 — EVM actor state loader  [foundation, no EVM yet]
- `state/evm/actor.go`: given an `address.Address` or actor, load + CBOR-decode
  the versioned `evm.State`. Return bytecode CID + ContractState (storage root) CID.
- Reuse `state/actors` version dispatch (actor code CID → network version → evm v).
- **Verify gate:** for ServiceProviderRegistry on calibration, load its evm.State
  and confirm BytecodeHash matches keccak256(bytecode block bytes).

### Stage 2 — KAMT storage reader  [the tricky IPLD bit]
- FEVM storage is `KAMT<U256, U256>` — a Keccak-keyed AMT, NOT the standard HAMT.
  Key = U256 storage slot, hashed; value = U256 (RLP/CBOR encoded per FVM).
- `state/kamt/kamt.go`: read-only KAMT lookup over the accessor's blockstore.
  Mirror the ref-fvm KAMT layout (bitwidth, hash function, node CBOR shape).
- **Verify gate:** read a known non-zero storage slot of a calibration contract
  and match `eth_getStorageAt` from Glif byte-for-byte.

### Stage 3 — pure-Go EVM interpreter  [the engine]
- Add a CGO-free EVM. Options, in order of preference:
  a. Vendor `go-ethereum/core/vm` interpreter (it is pure-Go; the CGO bits are
     secp256k1/blst, which we avoid by not enabling those precompiles, or by
     using the pure-Go fallbacks go-ethereum already ships).
  b. A minimal hand-rolled interpreter (only if go-ethereum's dep weight is
     unacceptable for the <90MB-ish footprint story).
- `vm/evm/interp.go`: wire a `vm.StateDB` shim → SLOAD via Stage 2 KAMT, account
  existence + code via Stage 1, BALANCE/EXTCODESIZE via accessor. Read-only:
  SSTORE/CREATE/SELFDESTRUCT are unreachable on a `call` (revert if hit).
- FEVM specifics: precompiles at 0x0e..(filecoin precompiles) can be stubbed/
  unsupported initially (most contract *reads* don't hit them); map f4-namespace
  addresses; chain id 314/314159; generous read-only gas cap.
- **Verify gate:** execute `ServiceProviderRegistry.activeProviderCount()` and
  match Glif return bytes.

### Stage 4 — `eth_call` integration + fallback
- `rpc/server/eth_api.go EthCall`: try local exec first; on any miss/error,
  fall back to VMBridge (config flag `--fevm-local` default on once proven).
- Decode the `{to, data, from?, value?}` call object; resolve block param
  (latest/by-number) → tipset → state root.
- Revert handling: return error code 3 + revert data (lotus #13467/#12553 shape).
- **Verify gate:** filcensus FoC enumeration (activeProviderCount,
  getAllActiveProviders, getProviderWithProduct) byte-identical to Glif.

### Stage 5 — curio-core cutover
- curio-core runs `--vm-bridge-rpc-disable` and completes PDP + payments reads
  against local eth_call only. Validate on cc-smoke.
- Keep VMBridge as opt-in fallback.

## Verification corpus (capture once, replay forever)

Capture real Glif responses for a fixed set of calls on calibration:
- ServiceProviderRegistry: activeProviderCount, getAllActiveProviders, getProviderWithProduct
- PDPVerifier: FIL_CLEANUP_DEPOSIT, getDataSetLastProvenEpoch, dataSetLive, getNextChallengeEpoch
- FilecoinPay / USDFC: balanceOf, allowance, rail views
Store as golden files; each stage's verify gate replays against them.

## Sequencing note

Stages 1→2→3 are a hard dependency chain. Stage 1 is small and unblocks the
verify harness. Stage 2 (KAMT) is the riskiest IPLD work. Stage 3 (interpreter)
is the largest. Ship each behind its verify gate; do not integrate (Stage 4)
until Stage 3 matches Glif on the corpus.

## Non-goals (this sprint)
- eth_estimateGas local exec (forward to bridge; gas for read calls is bounded).
- eth_sendRawTransaction (stays on bridge; it's a write).
- Full precompile coverage (add as real calls demand).
- State *writes* of any kind.

## Status 2026-06-15 — Stages 1-5 landed, local eth_call live on cc-smoke

All five stages implemented and released as Lantern **v1.6.0 → v1.6.4**:

- **Stage 1** (`state/actors/evm.go`): EVM actor loader. ✅ Glif parity.
- **Stage 2** (`state/kamt/`): pure-Go KAMT reader. ✅ Glif parity (registry + deep-tree contract).
- **Stage 3** (`vm/evm/`): pure-Go read-only EVM interpreter. ✅ Glif parity (3 registry view fns).
- **Stage 4** (`rpc/handlers/evmexec.go`): `ChainAPI.EthCall` local-first + VMBridge fallback,
  live-head anchoring, revert→code-3. ✅ integration parity with `Bridge=nil`.
- **Stage 5**: curio-core pinned to v1.6.4, deployed to cc-smoke. PDPVerifier contract
  reads now **served locally** (pure-Go EVM, no Glif); proof loop intact; daemon stable.

Two KAMT correctness bugs were found and fixed *because of* the live cutover (they only
showed on deep contract trees, not the shallow registry):
- extension matching (ref-fvm `match_extension`: partial match ⇒ absent), v1.6.3.
- final hash-bits window clamp (`min(bit_width, remaining)`; 256-bit key leaves a 1-bit
  bottom window), v1.6.4.

### Remaining before a true `--vm-bridge-rpc-disable`

The only fallbacks left in the live run are `fetch kamt node <cid>: block not found` —
the embedded Bitswap blockstore hasn't fetched every storage-trie node at the live head
yet (lazy fetch). This is a **block-availability** concern (same class as the head-store
work in #33/#62), *not* an eth_call engine limitation. The bridge safely covers these
today. Full bridge-disable needs embedded state-block prefetch/availability; tracked as
follow-up. Opcode coverage is expanded as new contract calls demand it.
