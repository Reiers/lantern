# #45 — Local FEVM write path (the zero-Glif keystone)

Status: **SCOPED, not started** · Depends on: #43, #44 (both done) · Blocks: full `--vm-bridge-rpc-disable`

## Why
After #44 (adaptive warming + deep-trie walk), the **read** path is zero-Glif:
contract-state `eth_call` serves 100% locally on cc-smoke. The remaining
bridge dependency is entirely the **write** path. A writing SP (the proof
loop: InitPP / NextPP / Prove / dataset create / delete) still needs:

| Method | Used for | Today |
|---|---|---|
| `eth_estimateGas` | gas sizing every tx | bridge-only |
| `eth_getTransactionCount` | nonce | bridge-only |
| `eth_sendRawTransaction` | broadcast | bridge-only |
| `eth_getTransactionReceipt` | confirm (the #81 watcher polls this) | bridge-only |
| `eth_feeHistory` | EIP-1559 premium suggestion | bridge-only |
| `eth_getLogs` | event scan | bridge-only |

Disabling the bridge today breaks all writes + confirmation. This issue
makes them local.

## Key finding: the primitives ALREADY EXIST in Lantern
This is **wiring + an ETH↔Filecoin translation layer**, not new infra.

1. **Nonce** — `accessor.GetActor(addr).Nonce` is read locally already
   (`chain_api.go:1164 onChain = act.Nonce`, plus a pending-mpool adjust
   loop at 1169-1170). The stale comment on `EthGetTransactionCount`
   claiming "state tree doesn't include f4 nonces" is **wrong**; the
   field is right there via the accessor.
2. **Gas estimate** — `vm.GasEstimator.EstimateMessageGas` exists and is
   wired through `ChainAPI.VMShell` (`chain_api.go:1004`). Pure-Go.
3. **Send** — `ChainAPI.MpoolPush` publishes a `SignedMessage` to
   gossipsub `/fil/msgs/<network>` locally (`chain_api.go:1109`).
4. **Receipt / confirm** — `StateSearchMsg` + the persistent header store
   do local message lookup (`chain_api.go:823`).

The **one missing piece** is the ETH-tx codec: decode the RLP signed-eth
tx (EIP-1559 / delegated 0x04) into Filecoin `Message` + signature, and
reconstruct an ETH-shaped receipt from a Filecoin `MsgLookup`. Lotus's
`ethtypes` is the reference recipe (we port the shape, not the FFI).

## Implementation plan (ordered, each independently shippable)

### Stage 1 — `eth_getTransactionCount` local (smallest, unblocks nonce)
- Resolve addr → f4/EAM filaddr (same recipe as `EthGetBalance`,
  evmexec.go:112).
- `act.Nonce` from the accessor at the requested block tag.
- For `"pending"`, add the count of this sender's queued mpool messages
  (the 1169-1170 loop already exists; factor it out).
- Bridge fallback retained behind a flag for safety during rollout.
- **Verify:** nonce matches bridge for the SP wallet across 100 blocks.

### Stage 2 — `eth_estimateGas` local
- Decode `callObj` → Filecoin `Message` (reuse the localEthCall arg
  marshalling from #43).
- `VMShell.EstimateGasLimit` → gas; map to ETH gas units.
- Add a safety margin matching upstream's `GasEstimateGasLimit` overestimate.
- **Verify:** estimate within tolerance of bridge for real PDP txs; the
  proof loop must not under-estimate (would fail on-chain).

### Stage 3 — ETH-tx codec (`internal/ethtx` or `chain/ethtx`)
- `DecodeSignedEthTx(rlpHex) -> (msg types.Message, sig crypto.Signature)`.
  Handle EIP-1559 + the Filecoin delegated (0x04 / EAM) shape.
- `EthReceiptFromMsgLookup(lookup, msg) -> ethReceipt`.
- Pure-Go; port shapes from Lotus `ethtypes` (NO filecoin-ffi).
- Heavily unit-tested against known calibration txs (decode → re-encode
  round-trip + hash match).

### Stage 4 — `eth_sendRawTransaction` local
- `DecodeSignedEthTx` → `SignedMessage` → `MpoolPush`.
- Return the eth tx hash (computed from the wire bytes, not the bridge).
- **Verify:** SP broadcasts a real PDP tx with bridge disabled; tx lands
  on-chain.

### Stage 5 — `eth_getTransactionReceipt` + `eth_getTransactionByHash` local
- eth tx hash → Filecoin msg CID (the EAM/delegated mapping) →
  `StateSearchMsg` → `EthReceiptFromMsgLookup`.
- Return nil-until-found (the go-ethereum poll loop expects this; the #81
  watcher already polls this shape).
- **Verify:** the #81 watcher confirms a real tx with bridge disabled.

### Stage 6 — `eth_feeHistory` + `eth_getLogs`
- feeHistory: synthesize from header store base fees + `GasEstimator`
  premium percentiles.
- getLogs: scan the header store's receipt/event index for the requested
  range. (Larger; may stay bridge-backed longest.)

### Stage 7 — flip `--vm-bridge-rpc-disable` on cc-smoke
- Only after Stages 1-5 verified. Run a full proving window + a
  dataset-create + a delete with the bridge OFF. Watch the #81 watcher
  confirm every tx locally. Calibration before any mainnet claim.

## Risk notes
- **Under-estimating gas is the dangerous failure** — an SP tx that
  under-estimates fails on-chain and stalls the proof loop. Stage 2 must
  err high, matching upstream's conservative ceiling.
- **Nonce races** — the pending-mpool adjust must be correct or two txs
  collide on a nonce. Reuse the existing loop, don't reinvent.
- **Receipt shape drift** — go-ethereum clients are strict about receipt
  fields; round-trip tests against real bridge receipts are mandatory.
- Keep every stage behind a per-method bridge fallback during rollout so
  a translation bug degrades to Glif rather than breaking the SP.

## Done = 
`--vm-bridge-rpc-disable` runs a full proof loop (sign → estimate → send →
confirm) on cc-smoke with zero Glif traffic, verified by the #81 watcher
confirming every tx locally over a full proving window.
