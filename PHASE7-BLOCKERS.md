# Phase 7 — Blockers, Decisions, and Known Limitations

Phase 7 delivered the SP-compatibility surface: a pure-Go VM shell,
StateCall, the GasEstimate* family, MinerGetBaseInfo, MinerCreateBlock
(dry-run), MpoolSelect, SyncSubmitBlock (gated), Paych voucher methods,
and the cmd/lantern-phase7 end-to-end demo.

Below are the rough edges and follow-ups Phase 7 deliberately rolled
forward.

---

## B1. VM shell does not execute actor logic

**What:** Lantern's `vm/` package dispatches messages to the built-in
actor method tables in `go-state-types/builtin/v{17,18}` and accounts
gas using the canonical Filecoin v15 price list. It does **not**
execute the method bodies. For Curio's flow:

- `Send` (method 0) is end-to-end: sender resolution, balance check,
  gas accounting. Real fidelity.
- Builtin actor methods: gas-accounting only. ExitCode=0,
  Return=nil, GasUsed = OnChainMessage + OnInvoke + OnIpldGet.
- User-deployed actors (EVM contracts, FVM v3+ user actors):
  ExitCode=SysErrInvalidReceiver.

**Impact for Curio:**

- `GasEstimateMessageGas` is order-of-magnitude correct but not
  byte-exact. Curio's `tasks/message/sender.go` sets MaximizeFeeCap=true
  so feeCap is recomputed at submit time anyway. GasPremium is the
  60th percentile of Lantern's local mempool (or 100k fallback).
- `StateCall` does not detect application-level reverts. For
  `eth_call` against an EVM contract, this means Lantern returns
  ExitCode=SysErrInvalidReceiver. **Curio uses StateCall for the
  storage-market PSD verification path** — that path will fail loudly
  with Lantern until the Phase 8 EVM port lands.
- Any Curio task that depends on a specific return-value byte payload
  (e.g. `Multisig.Approve` returning the resulting state hash) will
  see an empty `Return` field. Curio currently does not appear to
  depend on these in its write-path; spot-check before swap-in.

**Fix in Phase 8 / V2:** port the minimal Account+Multisig+Paych+Init
method bodies into Lantern, or wire a side-channel call-out to a full
node for StateCall only.

## B2. MinerCreateBlock produces an invalid state root

**What:** `MinerCreateBlock` returns a syntactically valid `*types.BlockMsg`
with real Parents, Ticket, ElectionProof, BeaconEntries, signed header.
But the header's `ParentStateRoot` is taken verbatim from the parent
tipset — it does **not** include the post-execution state root after
applying the selected messages.

**Why:** computing the correct state root requires applying every
message through a full FVM. Lantern's VM shell cannot do that.

**Consequence:** a block built by Lantern's `MinerCreateBlock` would be
**rejected by the network** as soon as it propagates: the state root
won't match what other nodes compute. Hence Phase 7's hard rule:

```go
ChainAPI.AllowBlockSubmit = false // default
```

When false, `SyncSubmitBlock` returns `ErrNotImpl` rather than
publishing. Operators have to flip this manually if they ever want
Lantern to publish blocks, and the docs must warn them.

**Fix in Phase 8 / V2:** same as B1 — full FVM port, or call out to a
full node for "compute new state root over this message list" once
per won-block.

## B3. Paych voucher signing bytes are not Lotus-exact

**What:** `paychVoucherSigningBytes` uses a Lantern-internal
canonical-form placeholder (`voucher:<addr>:<lane>:<nonce>:<amount>...`)
instead of the cbor-gen serialised SignedVoucher with Signature=nil that
Lotus / go-state-types produce.

**Impact:** vouchers signed by Lantern's `PaychVoucherCreate` will
round-trip through Lantern's own `PaychVoucherCheckValid` (the demo
proves this). They will **not** verify under Lotus's voucher checker
and vice versa.

**Fix:** lift `paych.SignedVoucher.SigningBytes` verbatim into a
helper, vendor the relevant cbor-gen output, and switch the canonical
form. ~30 LOC of cbor encoding plumbing.

## B4. MinerGetBaseInfo BeaconEntries are head-of-chain, not target-epoch

**What:** the `BeaconEntries` and `PrevBeaconEntry` fields are taken
from the head tipset's first block, not walked specifically for the
target epoch passed to `MinerGetBaseInfo(miner, epoch, tsk)`.

**Why:** Lantern doesn't carry a per-epoch beacon-entry lookup; only the
header store does, and the demo runs without one wired.

**Consequence:** when Curio queries `MinerGetBaseInfo(maddr, epochN)`
where epochN < headEpoch, the returned beacon entries are stale. For
the SP block-production path Curio almost always asks for the most
recent epoch, so this is usually fine; historical queries (e.g. dispute
prep) will be off.

**Fix in Phase 8:** add `HeaderStore.BeaconEntriesAt(epoch) []BeaconEntry`,
walk backward from head to find the canonical entries for the target
epoch.

## B5. MinerGetBaseInfo `Sectors` field is a sample, not the spec-mandated subset

**What:** Filecoin's MinerGetBaseInfo returns a deterministically sampled
subset of provable sectors (typically 10 per partition for winning
PoSt). Lantern returns the lowest-numbered N=200 active sectors,
sorted by sector number.

**Impact:** Curio's WinPoSt task uses this list to schedule winning
PoSt proofs against the provable sectors it has on disk. As long as
the returned sectors are actually live + active, the WinPoSt math
works; the determinism mismatch matters only if Curio cross-checks the
list against another full node.

**Fix in Phase 8:** import the canonical sampling math from
go-state-types/builtin/v18/miner/policy.go or the proofs-actor
WinningPoStProofIndex helper. ~40 LOC.

## B6. PaychGet cannot create new channels

**What:** `PaychGet(from, to, amt)` returns ErrNotImpl. Channel
creation requires constructing + signing + publishing an
`InitActor.Exec(paych.ConstructorParams)` message and waiting for
the resulting actor address. Lantern's VM shell doesn't carry the
init actor's "predict next actor address" logic.

**Impact:** Curio's retrieval flow expects to look up an existing
channel by (from, to) pair when it has previously created one. Without
that mapping, Curio fails open and falls back to its own channel store
(which is the actual source of truth — Curio persists channel CIDs in
Yugabyte).

**Fix in Phase 8:** add a (from, to) → channel index either in the
gateway or via a side-call to a full node. Lantern itself never needs
to mint channels.

## B7. SyncSubmitBlock needs a BlockPublisher implementation

**What:** SyncSubmitBlock is wired through a `BlockPublisher` interface
that the mpool would have to implement (publish to gossipsub
`/fil/blocks/<network>`). The mpool package currently only publishes
to `/fil/msgs/<network>`. Phase 7 declared the interface but did NOT
add the implementation, because:

  (a) AllowBlockSubmit defaults false anyway, so the path is dead
      code at runtime.
  (b) Combined with B2 (invalid state root), publishing a block from
      Lantern would be harmful.

**Fix in Phase 8 / V2:** when the FVM port lands and B2 is closed,
add `(*mpool.Publisher).PublishBlock(blk)` that subscribes the
gossipsub topic and publishes.

## B8. Anchor / phase 6 follow-ups still open

Carry-over from PHASE6-BLOCKERS.md that Phase 7 did **not** address:

- **B1-anchor-rebuild (Phase 6)**: addressed in commit 3e3883b.
- **B3 DHT bootstrap**: still pending. Lantern's libp2p host connects
  to ~3 bootstrap peers; without DHT it cannot discover more. Track
  as Phase 8.
- **B5 StateSearchMsg cross-test fixture**: still pending.
- **B7 F3 cert subscriber persistence**: still pending.

---

## Curio compatibility checklist after Phase 7

| Tier | Method                                  | Phase  | Status |
|------|-----------------------------------------|--------|--------|
| 1    | StateWaitMsg                            | 6      | ✅ |
| 1    | StateSearchMsg                          | 6      | ✅ |
| 1    | ChainGetTipSetByHeight                  | 6      | ✅ |
| 2    | StateGetRandomnessFromBeacon            | 6      | ✅ |
| 2    | StateGetRandomnessFromTickets           | 6      | ✅ |
| 2    | StateGetBeaconEntry                     | 6      | ✅ |
| 1    | MpoolPush                               | 6      | ✅ |
| 2    | MpoolGetNonce                           | 6      | ✅ |
| 2    | MpoolPending                            | 6      | ✅ |
| 5    | StateCall                               | **7**  | ✅ (gas-only fidelity, no real exec) |
| 5    | GasEstimateMessageGas                   | **7**  | ✅ (heuristic) |
| 5    | GasEstimateFeeCap                       | **7**  | ✅ |
| 5    | GasEstimateGasPremium                   | **7**  | ✅ (mempool sample) |
| 4    | MinerGetBaseInfo                        | **7**  | ✅ (real state read, sampled sectors) |
| 4    | MinerCreateBlock                        | **7**  | ⚠ (returns valid BlockMsg but wrong state root; AllowBlockSubmit=false) |
| 4    | MpoolSelect                             | **7**  | ✅ |
| 4    | SyncSubmitBlock                         | **7**  | ⚠ (gated; needs B7 mpool block publisher) |
| 3    | PaychAvailableFunds                     | **7**  | ✅ |
| 3    | PaychVoucherCreate                      | **7**  | ✅ (signs; bytes differ from Lotus — B3) |
| 3    | PaychVoucherCheckValid                  | **7**  | ✅ (within Lantern; cross-Lotus needs B3) |
| 3    | PaychVoucherCheckSpendable              | **7**  | ✅ |
| 3    | PaychVoucherList                        | **7**  | ⚠ (returns nil — no persistence) |
| 3    | PaychGet                                | **7**  | ❌ (channel creation not in scope; see B6) |

**Phase 7 unblocks** Curio's read-only gas estimation path, miner-base-info
queries, and the dry-run block-template assembly. It does NOT yet
support live block production against mainnet (B2 + B7 + B1).

---

## Phase 8 priority order

1. **B2 + B1 FVM bridge.** Either (a) port Account, Multisig, Init
   method bodies into Lantern (~3-5k LOC), or (b) wire a side-channel
   to a full node for `Filecoin.StateCompute` over the per-block
   message list. Option (b) is faster but reintroduces a trust point
   for SPs producing blocks.

2. **B3 Paych signing bytes**: lift `paych.SignedVoucher.SigningBytes`
   verbatim. ~30 LOC. Unlocks Curio voucher interop.

3. **B7 BlockPublisher**: add `/fil/blocks/<network>` topic to the
   mpool gossipsub host. Pre-req for live SyncSubmitBlock once B2 is
   closed.

4. **DHT bootstrap** (from Phase 6 B3): peer count climbs above the
   bootstrap-list ceiling.

5. **Curio integration smoke test on a real Linux box.** Document the
   Forest -> Lantern swap procedure for sp.reiers.io and any test SP.
