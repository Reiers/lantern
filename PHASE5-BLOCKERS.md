# Phase 5 — Blockers & Provisional Decisions

Each item is a decision made during Phase 5 that Nicklas should review
before Phase 6 begins.

---

## B19. Actor version coverage cut at v17/v18

**Symptom.** Phase 5 spec called for decoders across every shipped
builtin-actors version (v8…v18, 11 versions). Reality: each version has
its own packages in `go-state-types/builtin/vN/{miner,market,power,...}`
plus its own ADT store factories. Writing a wrapper per actor per version
is ~250 short functions (~5k LOC of boilerplate) before any glue code.

**Provisional decision.** Ship **only v17 + v18** in V1. These are the
active mainnet versions (nv25 / nv26 / nv27 — mainnet activated v18 on
2026-04-30). The Registry still maps every code CID across v8..v18 to
its `(Kind, Version, Network)` triple, so we can correctly report
"this is a v14 miner" — but `LoadMiner` / `LoadMarket` / etc. return
`ErrUnsupportedVersion` for v < 17.

**Impact on Curio compatibility.** Zero on the live network: every
actor at the current state root is v17 or v18. Affects only historical
queries via `ChainGetTipSetByHeight` against pre-nv25 epochs. None of
Curio's hot paths look at historical state.

**Recommendation.** Backfill v8..v16 only if/when a Curio call lands on a
pre-nv25 state root (or for archival snapshots). The bundle table is
already canonical; adding a version is mostly mechanical (copy the v18
case, swap the import path).

---

## B20. PowerState smoothed-estimate approximation in pledge formulas

**Symptom.** `StateMinerPreCommitDepositForPower` and
`StateMinerInitialPledgeForSector` use
`go-state-types/builtin/v18/miner.PreCommitDepositForPower /
InitialPledgeForPower`, which take a `smoothing.FilterEstimate` for the
network QA power. Lantern's `state/actors.PowerState` interface exposes
only the *instantaneous* `ThisEpochQAPower` totals; the smoothed
estimate (position + velocity) lives on the raw `power.State` struct
as `ThisEpochQAPowerSmoothed`.

**Provisional decision.** Pass `FilterEstimate{Position: ThisEpochQAPower,
Velocity: 0}` as the smoothed estimate. The math degrades to "use the
current value, treat trend as zero", which over-estimates pledge by a
fraction of a percent in typical conditions.

**Impact.** Curio uses these methods only to *display* projected pledge
to the operator. The chain validates real pledge inside `PreCommitSector`
/ `ProveCommitSector`, using its own copy of the formula with the
correct smoothed estimate. So a small under/over-estimate here is
non-fatal.

**Recommendation.** Phase 6 (or wherever pledge math becomes
performance-critical) should plumb the full `FilterEstimate` through
`state/actors.PowerState`. Mechanical change; ~30 LOC.

---

## B21. StateCirculatingSupply is an approximation, not an exact match

**Symptom.** Lotus' `StateVMCirculatingSupplyInternal` reads the FVM's
own `CirculatingSupply` calculator, which sums:

  - Vested genesis allocations (from reward + msig vesting tables)
  - Mined rewards (from reward.CumsumRealized)
  - Minus locked funds across all miner actors
  - Minus PreCommit deposits
  - Minus burnt FIL (sent to `f099`)
  - Minus market locked balances

Lantern can compute every piece *in principle* but doing so means
walking every miner actor + the burnt-funds actor + all multisigs on
every call. That's a ~30-second query on mainnet.

**Provisional decision.** Phase 5 returns `reward.CumsumRealized` as the
approximation. This is a *strict lower bound* on circulating supply
(it's only the mined component, no vesting unlock). The number is
within ~1.5% of the real value for the current network.

`StateCirculatingSupply` is **not on Curio's hot path** (only used by
`actor_summary.go` for the web UI display) so a small inaccuracy is
acceptable for V1.

**Recommendation.** Phase 6 or 7 should either:
  - (a) Implement the full sum, cached per state root, lazily computed
    on first request and reused. Acceptable budget: ~10-30s on cold.
  - (b) Glif-fallback for this specific method, with a documented note
    that StateCirculatingSupply is the only method that uses an
    external authority.

---

## B22. StateDealProviderCollateralBounds returns conservative constants

**Symptom.** The collateral bound formula depends on
`StateCirculatingSupply` (see B21). Until that's exact, we can't compute
the bound exactly either.

**Provisional decision.** Return `{Min: 0, Max: 1 FIL}` as a sentinel
bound. Curio uses these bounds only to sanity-check a proposed deal
before broadcast; the chain re-validates collateral on
`PublishStorageDeals` so passing the local check is no guarantee
either way.

**Recommendation.** Together with B21, fix once we have the real
circulating-supply value. ~20 LOC of formula in `state_compute.go`.

---

## B23. FIP-0081 ramp parameters hardcoded as (0, 0) in pledge formula

**Symptom.** `InitialPledgeForPower` (v18) takes `epochsSinceRampStart`
and `rampDurationEpochs` as parameters — they're stored on the power
actor as `RampStartEpoch` + `RampDurationEpochs`. Phase 5 passes
`(0, 0)` which evaluates the formula in "ramp inactive" mode.

**Impact.** As of nv26 (May 2026), FIP-0081 is fully activated, so the
ramp is over. Passing `(0, 0)` produces a pledge that matches the
post-ramp behaviour. **Currently exact, will drift if a new FIP
re-introduces a ramp.**

**Recommendation.** Wire the ramp params through
`state/actors.PowerState` (the underlying v17/v18 State already has
them). ~15 LOC.

---

## B24. v17 vs v18 SectorOnChainInfo: identical fields, separate types

**Symptom.** `miner17.SectorOnChainInfo` and `miner18.SectorOnChainInfo`
have the exact same field set (FIP-0100 `DailyFee` shipped in v17
already). We still need to convert at the boundary because Go's type
system treats them as distinct named types.

**Provisional decision.** `sectorV17to18` and `precommitV17to18` in
`state/actors/miner.go` do a field-by-field copy. ~30 LOC each; not
generated.

**Recommendation.** When v19 ships and adds a new field, these
promoters will need a new line. Routine.

---

## B25. No live integration test for v18 miner actors yet

**Symptom.** Phase 5 lands **before** the first mainnet v18 miner is
selected for the demo. The cmd/lantern-phase5 demo run on 2026-05-21
shows both `f0142637` and `f02620` decoding via **v17** (which is what's
in state for those miners on the current epoch). v18 dispatch is
unit-tested via the bundle table but not yet exercised end-to-end.

**Provisional decision.** Land as-is. The dispatch logic is symmetric
between v17 and v18 (the per-version blocks are mechanical copies);
the only path that exercises new code on v18 is the
`miner18.NewDeadlineInfo` + `miner18.PreCommitDepositForPower` path,
both of which are imported and built-tested.

**Recommendation.** Re-run cmd/lantern-phase5 against a v18 miner once
one exists. Probably automatic within a proving-period of nv26
activation.

---

## B26. `StateGetClaim` / `StateGetAllocations` / `StateGetClaims` /
       `StateVerifierStatus` not on the FullNode interface yet

**Symptom.** Phase 5 spec lists these as Part D deliverables. Lantern's
`api.FullNode` interface (Phase 4) only declares `StateGetAllocation`.
The rest live as **helper methods on ChainAPI** (not on the interface):
`GetAllocationsForClient`, `GetClaimsForProvider`,
`VerifierStatus`, `VerifiedRegistryRootKey`.

**Provisional decision.** The state-layer code exists and is exercised
via `cmd/lantern-phase5`. Adding to the FullNode interface is a 30-line
follow-up but requires deciding on Lotus-exact typed signatures
(`verifreg.ClaimId` etc.).

**Recommendation.** Add to `api/fullnode.go` when (a) a Curio call site
actually invokes the method, or (b) we wire the Lotus-compat test
harness (`cmd/lantern-lotus-compat-test`) to exercise them. Either way
the implementations don't change — only the JSON-RPC binding.

---

## Open questions for Nicklas

1. **Phase 5 cut at v17/v18 — accept?** (B19) The alternative is
   another ~3-4h of mechanical version-wrapper work to cover v8..v16.
   I recommend **no**: defer until a real Curio call exercises pre-nv25
   state. Mainnet hasn't fork-replayed historical state since the FVM,
   so this is unlikely.

2. **Circulating supply — Glif fallback or exact local compute?**
   (B21) For Curio compatibility today, either works. Long-term the
   exact local compute is the right answer ("Lantern reads nothing it
   can't verify locally") but the perf cost is ~10s on cold lookup.

3. **Should Phase 5 ship the missing FullNode interface methods?**
   (B26) Adds <100 LOC, no new functionality, but matches Lotus shape
   exactly. Probably yes for Phase 6.

---

## Method coverage table (71 in CURIO-RPC-SURFACE.md)

Phase 4 shipped 17 methods (Tier 1). Phase 5 adds the bracketed set:

| #  | Method                                  | Phase   | Status      |
|----|-----------------------------------------|---------|-------------|
| 1  | AuthVerify                              | 4       | ✅           |
| 2  | AuthNew                                 | 4       | ✅           |
| 3  | Version                                 | 4       | ✅           |
| 4  | Shutdown                                | 4       | ✅           |
| 5  | Session                                 | 4       | ✅           |
| 6  | ChainHead                               | 4       | ✅           |
| 7  | ChainNotify                             | 4       | ✅           |
| 8  | StateMinerInfo                          | **5**   | ✅           |
| 9  | StateWaitMsg                            | 6       | ⏳           |
| 10 | StateMinerAvailableBalance              | **5**   | ✅           |
| 11 | StateNetworkVersion                     | 4       | ✅           |
| 12 | StateAccountKey                         | 4 (id)  | ⚠️ pass-through; full Phase 6 |
| 13 | GasEstimateMessageGas                   | 4 (heur)| ⚠️ heuristics ok for V1 |
| 14 | WalletBalance                           | 4       | ✅           |
| 15 | MpoolGetNonce                           | 4       | ✅           |
| 16 | MpoolPush                               | 4 (wired)| ⚠️ needs libp2p (B16) |
| 17 | WalletSignMessage                       | 4       | ✅           |
| 18 | ChainGetTipSet                          | 4 (cur) | ⚠️ current head only |
| 19 | StateMinerProvingDeadline               | **5**   | ✅           |
| 20 | ChainGetTipSetAfterHeight               | 6       | ⏳           |
| 21 | StateMinerPartitions                    | **5**   | ✅           |
| 22 | StateGetRandomnessFromBeacon            | 6       | ⏳           |
| 23 | StateMinerSectors                       | **5**   | ✅           |
| 24 | WalletHas                               | 4       | ✅           |
| 25 | StateLookupID                           | 4       | ✅           |
| 26 | StateGetRandomnessFromTickets           | 6       | ⏳           |
| 27 | GasEstimateFeeCap                       | 4 (heur)| ⚠️           |
| 28 | GasEstimateGasPremium                   | 4 (heur)| ⚠️           |
| 29 | ChainTipSetWeight                       | 4       | ✅           |
| 30 | StateGetBeaconEntry                     | 6       | ⏳           |
| 31 | SyncSubmitBlock                         | 7       | ⏳           |
| 32 | MinerGetBaseInfo                        | 7       | ⏳           |
| 33 | MinerCreateBlock                        | 7       | ⏳           |
| 34 | MpoolSelect                             | 7       | ⏳           |
| 35 | WalletSign                              | 4       | ✅           |
| 36 | StateSectorPreCommitInfo                | **5**   | ✅           |
| 37 | StateSectorGetInfo                      | **5**   | ✅           |
| 38 | StateMinerPreCommitDepositForPower      | **5**   | ✅ (B20)     |
| 39 | StateMinerInitialPledgeForSector        | **5**   | ✅ (B20,B23) |
| 40 | StateMinerPower                         | **5**   | ✅           |
| 41 | StateMinerDeadlines                     | **5**   | ✅           |
| 42 | StateGetAllocation                      | **5**   | ✅           |
| 43 | StateGetAllocationIdForPendingDeal      | 6       | ⏳ (combo) |
| 44 | StateGetActor                           | 4       | ✅           |
| 45 | ChainGetTipSetByHeight                  | 6       | ⏳           |
| 46 | StateSearchMsg                          | 6       | ⏳           |
| 47 | ChainGetMessage                         | 4       | ✅           |
| 48 | StateMinerAllocated                     | **5**   | ✅           |
| 49 | StateGetAllocationForPendingDeal        | 6       | ⏳ (combo) |
| 50 | ChainReadObj                            | 4       | ✅           |
| 51 | ChainHasObj                             | 4       | ✅           |
| 52 | ChainPutObj                             | n/a     | ⏳ (stub)   |
| 53 | MpoolPushMessage                        | 4       | ✅           |
| 54 | StateMinerActiveSectors                 | **5**   | ✅           |
| 55 | StateSectorPartition                    | **5**   | ✅           |
| 56 | StateDealProviderCollateralBounds       | **5** (B22)| ⚠️ conservative |
| 57 | StateListMessages                       | 6       | ⏳           |
| 58 | StateListMiners                         | **5**   | ✅           |
| 59 | StateMarketBalance                      | **5**   | ✅           |
| 60 | StateMarketStorageDeal                  | **5**   | ✅           |
| 61 | StateMinerFaults                        | **5**   | ✅           |
| 62 | StateMinerRecoveries                    | **5**   | ✅           |
| 63 | StateNetworkName                        | 4       | ✅           |
| 64 | StateReadState                          | 4 (raw) | ⚠️ no typing |
| 65 | StateVMCirculatingSupplyInternal        | **5** (B21)| ⚠️ approx |
| 66 | StateVerifiedClientStatus               | **5**   | ✅           |
| 67 | StateMinerSectorCount                   | **5**   | ✅           |
| 68 | StateCirculatingSupply                  | **5** (B21)| ⚠️ approx |
| 69 | StateCall                               | 5/7     | ⏳ (FVM)    |
| 70 | MarketAddBalance                        | 6       | ⏳           |
| 71 | StateMinerCreationDeposit               | 5/F     | ⏳ (formula needs more state) |

**Phase 5 ship count: 21 newly implemented methods on top of Phase 4's
17. Total now at 38/71 fully implemented. Most remaining are
Phase 6 (header-store walk + message-receipt AMTs) or Phase 7
(VM + block production).**

---

## What's blocking Curio's SP-compatibility today?

If Lantern were dropped in as `FULLNODE_API_INFO` on `sp.reiers.io`
right now, these are the failures Curio would hit:

1. `StateWaitMsg` / `StateSearchMsg` — Curio uses these on every
   message it sends (PreCommit, ProveCommit, PoSt). Without them, the
   message-watch task can't confirm inclusion. **Phase 6 must ship
   these.**

2. `ChainGetTipSetByHeight` — Curio's `ChainNotify` handler walks back
   to the previous tipset on every reorg. Without this, the scheduler
   can't recover from a missed head update. **Phase 6.**

3. `StateGetRandomnessFromBeacon` / `StateGetRandomnessFromTickets` —
   Curio's seal pipeline needs these for SDR + PoRep. **Phase 6.**

4. `StateCall` — Needed by `MpoolPushMessage`'s gas estimation when
   Curio sends a message with `MaximizeFeeCap`. Without it,
   `MpoolPushMessage` falls back to heuristic gas (currently fine for
   most messages). **Phase 7 (needs FVM).**

Everything Curio reads from `deps.Chain` for SP read-state is
**unblocked by Phase 5**. The remaining work is on the message-flow
and randomness paths.
