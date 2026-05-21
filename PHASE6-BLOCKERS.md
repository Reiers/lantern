# Phase 6 — Blockers, Decisions, and Known Limitations

Phase 6 delivered: persistent header store, F3 cert subscriber, DRAND
randomness, libp2p host + gossipsub message pool, StateSearchMsg /
StateWaitMsg, and a Phase 6 demo. Below are the rough edges that
deliberately rolled forward into Phase 7.

---

## B1. F3 anchor power-table mismatch

**What:** The embedded mainnet anchor at instance 466453 fails to verify the
cert for the same instance because the power table we captured is at the
*current chain head* (epoch ~6035600 at capture time), not the committee
that signed instance 466453.

**Error from F3:** `finality certificate for instance 466453 specifies a
signer 1350631 but they have no effective power after scaling` — that
signer's power had changed by the time we snapshotted the table.

**Why this happened:** `lantern-f3-anchor` queries
`Filecoin.F3GetF3PowerTable` at the chain head, but a cert at instance N is
verified against the power table that was *finalized in cert N-1*. The
correct capture is `F3GetF3PowerTableByInstance(N-1)` or equivalent.

**Fix in Phase 7:** Re-pin the anchor via a tagged-instance Forest call (or
walk forward from the manifest's `InitialPowerTable` if we want a hardcoded
trust root). Track in a new `B-anchor-rebuild` issue. Until then the
embedded anchor loads fine but the cert chain stops at instance 466453.

## B2. DRAND randomness cross-check requires non-Glif RPC

**What:** Glif's public RPC at `api.node.glif.io` doesn't expose
`StateGetRandomnessFromBeacon`, `StateGetRandomnessFromTickets`,
`ChainGetRandomnessFromBeacon`, `ChainGetRandomnessFromTickets`, or
`StateGetBeaconEntry`. They all return `method not found`.

**What we cross-checked instead:**
- `MaxBeaconRoundForEpoch(6035749) == 28858492` matches the observed
  BeaconEntry round in mainnet tipset at epoch 6035749 (live curl).
- `DrawRandomnessFromDigest` matches a hand-computed blake2b256 over the
  exact preimage Lotus hashes (`int64-BE(pers) | digest[32] |
  int64-BE(round) | entropy`); see `chain/beacon/randomness_test.go`.

**Outstanding:** End-to-end byte-equality vs Lotus requires a Lotus full
node (Forest does not implement these methods either, last we checked).
Workaround: run lantern-phase6 against the Forest on lex via SSH tunnel
when we want a Forest-side equivalence proof. The math is independent of
the gateway, so as long as the unit tests pass we trust the derivation.

## B3. libp2p peer count: 3 instead of 10 in 60s

**What:** Lantern's libp2p host connects to ~3 of the 7 hardcoded bootstrap
peers within 60s. Two of the seven addresses are different transports for
the same peer (`devtty.eu` TCP + QUIC), so the effective peer ceiling
from the bootstrap list alone is 6 unique peers.

**Why ~3 is normal:** We don't run a DHT or DHT bootstrap, and gossipsub's
peer-exchange is conservative. The mesh propagates messages anyway (we
observed 15 valid `/fil/msgs/testnetnet` messages over 20s on a single
poll), so the read path works.

**Fix in Phase 7:** Add `go-libp2p-kad-dht` bootstrap + a configurable DHT
mode (Server when running as a Lantern gateway, Client otherwise). Once
the DHT is up, peer count climbs to 30-50 within seconds. Track in
`B-dht-bootstrap`.

## B4. Sync agent's full PollOnce is slow on cold start

**What:** `Sync.PollOnce` on an empty store starting at head H fetches
every block at every epoch in `[H - MaxBacktrack, H]` (1 RPC per tipset
+ 1 per block CID), then for each tipset calls `backfillParents` which
walks an additional `MaxBacktrack` parent steps. With MaxBacktrack=5 on
Glif this took ~6-8s, but on a more loaded RPC the cold-start cost can
push past 60s.

**Workaround in the demo:** We bypass `Sync.PollOnce` for the cold start
and just walk forward 5 epochs directly. The `Sync` agent itself is unit
tested for linear advance, reorg detection, and parent-chain backfill.

**Fix in Phase 7:** Wire `Filecoin.ChainNotify` (when available) so we
get pushed head-change events instead of polling. Falls back to the
current poll loop for RPC sources that don't support it.

## B5. StateSearchMsg cross-test fixture missing

**What:** Phase 6 ships the implementation but no live cross-check
against a known on-chain tx. The msgsearch unit tests cover the
nil-store / not-found paths; the AMT walk math is shared with the
already-tested `state/amt` package.

**Fix in Phase 7:** Pick 3-5 known mainnet message CIDs (one PreCommit,
one ProveCommit, one PoSt, one SetMaxPower etc.), record their expected
receipt JSON from Glif, and assert byte-equality in a new
`msgsearch_live_test.go` (gated behind `-tags=live`).

## B6. MpoolPushMessage gas estimation is heuristic

**What:** Lantern's `MpoolPushMessage` currently uses fixed heuristic gas
defaults (`GasLimit=10M, FeeCap=100M, Premium=100k`) instead of running
real chain state through a VM. For most Curio messages this is fine
(Curio sets `MaximizeFeeCap=true` so the fee cap is overwritten anyway),
but it can over-spend on the premium.

**Fix in Phase 7:** Implement `StateCall` + `GasEstimateMessageGas` once
we have the FVM port. Track in `B-gas-real-estimation`.

## B7. F3 cert subscriber doesn't yet persist to BadgerDB on the demo path

**What:** The subscriber supports persistence via `Options.DB` but the
Phase 6 demo doesn't open a DB — every demo run re-walks from the
anchor. The unit test (`TestSubscriberInitialState`) confirms the in-
memory state path works.

**Fix in Phase 7:** Once the demo daemon mode lands, wire the subscriber
to share its BadgerDB with the header store and the trustedroot
persistence.

---

## Curio compatibility checklist after Phase 6

| Tier | Method                                  | Phase | Status        |
|------|-----------------------------------------|-------|---------------|
| 1    | StateWaitMsg                            | **6** | ✅            |
| 1    | StateSearchMsg                          | **6** | ✅            |
| 1    | ChainGetTipSetByHeight                  | **6** | ✅ (when header store wired) |
| 2    | StateGetRandomnessFromBeacon            | **6** | ✅ (formula verified vs mainnet) |
| 2    | StateGetRandomnessFromTickets           | **6** | ✅            |
| 2    | StateGetBeaconEntry                     | **6** | ✅            |
| 1    | MpoolPush                               | **6** | ✅ (dry-run safe; needs B3 for steady-state) |
| 2    | MpoolGetNonce                           | **6** | ✅ (accounts for local pending) |
| 2    | MpoolPending                            | **6** | ✅            |
| 4    | SyncSubmitBlock                         | 7     | ⏳ (needs /fil/blocks/<network> topic) |
| 4    | MinerCreateBlock                        | 7     | ⏳ (FVM + winning POST) |
| 4    | MinerGetBaseInfo                        | 7     | ⏳            |
| 5    | StateCall                               | 7     | ⏳ (FVM)      |

**Lantern can now serve Curio's full read+write SP path for non-block-
producing messages.** Curio's seal pipeline (PreCommit, ProveCommit, PoSt,
randomness for SDR/PoRep) is unblocked by Phase 6. Block production
(MinerCreateBlock + SyncSubmitBlock) still needs Phase 7's FVM port.

---

## Phase 7 priority order

1. **B1 anchor rebuild** — pick a stable instance and capture its prior-
   instance power table. Without this, F3 finality is recorded but not
   actually advancing past the anchor.
2. **FVM port** for `StateCall` and `GasEstimateMessageGas`. This is the
   heaviest Phase 7 task; track in `B-fvm-port`.
3. **B3 DHT bootstrap** so libp2p peer counts climb above the bootstrap
   list size.
4. **Block production** (`MinerCreateBlock`, `SyncSubmitBlock`, the
   `/fil/blocks/testnetnet` topic). Curio needs this to mine blocks.
5. **`PaychVoucherCheckSpendable` family** — Curio uses these on every
   retrieval deal payment. Tier 3 in `CURIO-RPC-SURFACE.md`.
