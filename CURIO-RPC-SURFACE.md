# Curio RPC Surface

The complete list of Filecoin JSON-RPC methods Curio invokes on its
`FULLNODE_API_INFO` endpoint. This is the V1 RPC compatibility target for
Lantern.

## Source of truth

Curio defines its dependency interface in
[`api/api_chain.go`](https://github.com/filecoin-project/curio/blob/main/api/api_chain.go)
as `CurioChainRPC` (active under `-tags forest`). With no build tag,
`Chain = api.FullNode` from Lotus, but `CurioChainRPC` is the explicit
minimal subset Curio's authors guarantee works against Forest. Lantern
targets exactly this set.

Build-tag selector (Curio):
- `api/api_chain_all.go` (no tag): `type Chain = api.FullNode`
- `api/api_chain_limited.go` (`-tags forest`): `type Chain = CurioChainRPC`

That second file is what we need to satisfy. The version listed below
matches `filecoin-project/curio@main` as of 2026-05-21 (commit on the
`v1.36`-era release line; verified by cloning fresh:
`git clone --depth 1 https://github.com/filecoin-project/curio.git`).

## Methodology

For each method in `CurioChainRPC`:
1. Grep every Go source file under Curio (excluding `_test.go`, `itests/`,
   `api/proxy_gen.go`, `api/api_chain.go`, `api/docgen-openrpc/`,
   `web/api/webrpc/` listed separately) for `\.<Method>(`.
2. Record total call count and up to three representative call sites
   (file:line).
3. Categorize:
   - **R** = read-only state (must be served by trusted-root + HAMT walker).
   - **G** = gossip / network publish (no state read).
   - **W** = local wallet operation (no remote dependency).
   - **C** = compute on state (read + ephemeral VM call).
   - **N** = node admin (auth, version, shutdown — trivial).
   - **S** = subscription / stream.
   - **SP** = SP block production (Phase 7 only, needs winning POST infra).

## The table

| # | Method | Kind | Calls | Where (representative) | Notes |
|---|--------|------|-------|------------------------|-------|
| 1 | `AuthVerify(token)` | N | 0 | declared only | JWT verify against shared secret. |
| 2 | `AuthNew(perms)` | N | 3 | `cmd/curio/guidedsetup/guidedsetup.go:291`, `cmd/curio/config_new.go:52` | Issue scoped JWT for Curio's own UI. |
| 3 | `Version()` | N | 6 | `cmd/curio/cli.go:196`, `market/denylist/denylist.go:271` | Return `APIVersion{Version, APIVersion, BlockDelay}`. |
| 4 | `Shutdown()` | N | 6 | `cuhttp/server.go:248`, `cmd/curio/stop.go:24` | Curio calls on the **Curio** daemon, not the full node. Implement as no-op on Lantern (returns nil). |
| 5 | `Session()` | N | 0 | declared only | Returns UUID; trivial. |
| 6 | `ChainHead()` | R | **70** | every poller, every task. The hottest call. | Returns current best tipset. Lantern: `trustedroot.LatestTipSet()`. |
| 7 | `ChainNotify()` | S | 1 | `lib/chainsched/chain_sched.go:117` | Stream of `[]api.HeadChange{Type: "current"\|"apply"\|"revert", Val: TipSet}`. Curio uses this as its master scheduler tick. **Critical for liveness.** |
| 8 | `StateMinerInfo(maddr, tsk)` | R | 31 | `cmd/curio/seal.go:118`, `cmd/sptool/toolbox_curio_stats.go:55` | Reads `MinerInfo{Owner, Worker, Beneficiary, PeerId, SectorSize, ...}` from miner actor state. |
| 9 | `StateWaitMsg(cid, conf, limit, allowReplaced)` | R | 9 | `cmd/curio/market.go:347`, `tasks/message/watch.go:151` | Wait for message inclusion at given confidence. Polling implementation acceptable. |
| 10 | `StateMinerAvailableBalance(maddr, tsk)` | R | 4 | `tasks/seal/task_submit_precommit.go:297`, `tasks/seal/task_submit_commit.go:154` | Computed: `miner.Balance - PreCommitDeposits - LockedFunds - InitialPledge - FeeDebt`. |
| 11 | `StateNetworkVersion(tsk)` | R | 19 | `cmd/curio/seal.go:123`, `cmd/curio/rpc/rpc.go:295` | Derived from `tsk.Height()` and the network upgrade schedule (no state read needed; pure constant lookup). |
| 12 | `StateAccountKey(addr, tsk)` | R | 9 | `tasks/message/sender.go:323`, `cmd/sptool/actor.go:98` | Resolve actor address → BLS/secp pubkey via account actor state. |
| 13 | `GasEstimateMessageGas(msg, spec, tsk)` | C | 9 | `cmd/sptool/sector.go:1220`, `cmd/sptool/toolbox_ipni.go:204` | Combines: nonce assignment + `GasEstimateGasLimit` (StateCall) + `GasEstimateFeeCap` (basefee read) + `GasEstimateGasPremium` (mempool stats). Heavy — Phase 4. |
| 14 | `WalletBalance(addr)` | R | 8 | `tasks/balancemgr/handlers_wallet.go:22`, `cmd/sptool/toolbox_deal_client.go:727` | Read `actor.Balance` at head. |
| 15 | `MpoolGetNonce(addr)` | R | 1 | `tasks/message/sender.go:161` | Returns `max(actor.Nonce, pendingNonceInMpool) + 1`. Without a local mpool, Lantern returns actor nonce — caller (Curio's `tasks/message/sender.go`) tracks pending itself. |
| 16 | `MpoolPush(signedMsg)` | G | 2 | `tasks/message/sender.go:225`, `cmd/sptool/toolbox_deal_tools.go:405` | Publish to `/fil/msgs/<network>` gossipsub topic, return msg CID. |
| 17 | `WalletSignMessage(addr, msg)` | W | 1 | `tasks/message/sender.go:181` | Compute message CID, sign with local key, return SignedMessage. |
| 18 | `ChainGetTipSet(tsk)` | R | 2 | `tasks/message/watch.go:77`, `tasks/winning/winning_task.go:170` | Load tipset by key from header store. |
| 19 | `StateMinerProvingDeadline(maddr, tsk)` | R | 5 | `tasks/window/recover_task.go:261`, `tasks/window/compute_task.go:499` | Compute from `miner.ProvingPeriodStart` + epoch math; mostly stateless. |
| 20 | `ChainGetTipSetAfterHeight(h, tsk)` | R | 1 | `tasks/window/compute_task.go:193` | Walk tipset chain forward to first tipset at or after height. |
| 21 | `StateMinerPartitions(maddr, dlIdx, tsk)` | R | 8 | `tasks/window/recover_task.go:126`, `web/api/webrpc/actor_summary.go:389` | Read miner deadline → partitions array (AMT) → bitfields. |
| 22 | `StateGetRandomnessFromBeacon(dst, epoch, entropy, tsk)` | R | 6 | `tasks/seal/task_porep.go:111`, `cmd/curio/rpc/rpc.go:317` | Fetch beacon entry at given epoch, derive randomness via blake2b(dst‖entropy). |
| 23 | `StateMinerSectors(maddr, filter, tsk)` | R | 5 | `cmd/sptool/sector.go:462`, `tasks/window/compute_do.go:259` | AMT walk of miner sectors, filtered by bitfield. **Can be expensive** for an SP with 100k sectors. |
| 24 | `WalletHas(addr)` | W | 1 | `alertmanager/alerts.go:128` | Local keystore lookup. |
| 25 | `StateLookupID(addr, tsk)` | R | **33** | `cmd/curio/market.go:419`, `tasks/storage-market/mk20.go:*` | Init actor HAMT lookup: `addr → ID address`. Cacheable forever (resolutions don't change). |
| 26 | `StateGetRandomnessFromTickets(dst, epoch, entropy, tsk)` | R | 2 | `tasks/seal/task_sdr.go:169`, `tasks/window/submit_task.go:122` | Walk tipset back ~900 epochs grabbing tickets, derive randomness. |
| 27 | `GasEstimateFeeCap(msg, nblocks, tsk)` | R | 1 | `tasks/window/submit_task.go:304` | basefee × (1 + premium percentile). Pure compute on header basefee. |
| 28 | `GasEstimateGasPremium(nblocks, sender, gaslimit, tsk)` | C | 2 | `tasks/message/sender.go:342`, `tasks/window/submit_task.go:298` | Statistical over recent message inclusions. Approximate from header data; conservative default OK if exact stats unavailable. |
| 29 | `ChainTipSetWeight(tsk)` | R | 4 | `tasks/winning/winning_task.go:352`, `:357`, `:701` | Trivial: `tipset.ParentWeight` or computed weight at tipset. |
| 30 | `StateGetBeaconEntry(epoch)` | R | 1 | `tasks/winning/winning_task.go:199` | Returns drand beacon at epoch from header beacon-entry list. |
| 31 | `SyncSubmitBlock(blockMsg)` | G+SP | 1 | `tasks/winning/winning_task.go:475` | Publish new block to gossipsub. SP-only. |
| 32 | `MinerGetBaseInfo(maddr, epoch, tsk)` | R+SP | 1 | `tasks/winning/winning_task.go:205` | Returns `MiningBaseInfo{MinerPower, NetworkPower, Sectors, WorkerKey, SectorSize, PrevBeaconEntry, BeaconEntries, EligibleForMining}`. SP-only, Phase 7. |
| 33 | `MinerCreateBlock(template)` | C+SP | 1 | `tasks/winning/winning_task.go:415` | Pack messages, set parents, sign. SP-only, Phase 7. **Hardest method** because it needs message-pool selection and partial state execution. |
| 34 | `MpoolSelect(tsk, ticketQuality)` | C+SP | 2 | `tasks/winning/winning_task.go:331`, `:398` | Returns messages to include in a block. Without local mpool: subscribe to `/fil/msgs/<network>`, deduplicate, sort by quality. Phase 7. |
| 35 | `WalletSign(addr, data)` | W | 11 | `cmd/curio/debug-proofsvc.go:92`, `cmd/curio/debug-snsvc.go:732` | Sign arbitrary bytes with local key. |
| 36 | `StateSectorPreCommitInfo(maddr, sectorNum, tsk)` | R | 3 | `tasks/seal/poller_commit_msg.go:43`, `task_submit_commit.go:214` | HAMT lookup on miner.PreCommittedSectors. |
| 37 | `StateSectorGetInfo(maddr, sectorNum, tsk)` | R | 9 | `cmd/curio/rpc/rpc.go:285`, `tasks/seal/poller_commit_msg.go:186` | AMT lookup on miner.Sectors. |
| 38 | `StateMinerPreCommitDepositForPower(maddr, info, tsk)` | C | 1 | `tasks/seal/task_submit_precommit.go:255` | Pure formula from sector size + verified power. |
| 39 | `StateMinerInitialPledgeForSector(dur, size, verifiedSize, tsk)` | C | 2 | `tasks/seal/task_submit_commit.go:313`, `tasks/snap/task_submit.go:362` | Reads reward + power actor, applies pledge formula. |
| 40 | `StateMinerPower(maddr, tsk)` | R | 3 | `cmd/sptool/toolbox_curio_stats.go:137`, `deps/stats/wallet_exporter.go:245` | Read miner claim from power actor HAMT. |
| 41 | `StateMinerDeadlines(maddr, tsk)` | R | 0 (declared); used via webrpc `actor_summary.go:381` | Returns the 48 deadline state objects. |
| 42 | `StateGetAllocation(client, allocId, tsk)` | R | 8 | `cmd/curio/market.go:442`, `tasks/storage-market/mk20.go:1032` | HAMT lookup on verifreg actor. |
| 43 | `StateGetAllocationIdForPendingDeal(dealId, tsk)` | R | 2 | `tasks/seal/task_submit_commit.go:236`, `market/storageingest/deal_ingest_snap.go:232` | HAMT lookup on market actor. |
| 44 | `StateGetActor(addr, tsk)` | R | **25** | `cmd/curio/market.go:324`, `web/api/webrpc/actor_summary.go:329` | The fundamental HAMT lookup: state-root HAMT → actor head + balance + nonce + code. Every other state read funnels here. |
| 45 | `ChainGetTipSetByHeight(h, tsk)` | R | 5 | `tasks/message/watch.go:83`, `tasks/gc/storage_gc_mark.go:366` | Walk backward to tipset at height. |
| 46 | `StateSearchMsg(from, msgCid, limit, allowReplaced)` | R | 2 | `tasks/storage-market/task_find_deal.go:154`, `tasks/message/watch.go:151` | Scan `limit` epochs of messages-receipts AMTs looking for the cid. |
| 47 | `ChainGetMessage(msgCid)` | R | 2 | `tasks/storage-market/task_find_deal.go:179`, `tasks/message/watch.go:167` | IPLD block lookup by CID via Bitswap. |
| 48 | `StateMinerAllocated(maddr, tsk)` | R | 1 | `tasks/seal/sector_num_alloc.go:23` | Bitfield of allocated sector numbers, from miner state. |
| 49 | `StateGetAllocationForPendingDeal(dealId, tsk)` | R | 1 | `market/storageingest/deal_ingest_seal.go:276` | Combo: look up allocation id via market, then allocation via verifreg. |
| 50 | `ChainReadObj(cid)` | R | 0 (declared) | also called via `lotus-shed` style debugging | Raw block fetch. Trivial via Bitswap. |
| 51 | `ChainHasObj(cid)` | R | 0 (declared) | | Local-cache presence check. |
| 52 | `ChainPutObj(block)` | G | 0 (declared) | | Insert block into store. Lantern: insert into cache only. |
| 53 | `MpoolPushMessage(msg, spec)` | C+W+G | 9 | `cmd/curio/market.go:340`, `cmd/sptool/sector.go:1232` | Compose: nonce + gas estimate + sign + push. The high-level convenience wrapper. |
| 54 | `StateMinerActiveSectors(maddr, tsk)` | R | 3 | `cmd/sptool/sector.go:608`, `web/api/sector/sector.go:391` | Filter `Sectors` by allocated bitfield minus terminated, minus faults. |
| 55 | `StateSectorPartition(maddr, sectorNum, tsk)` | R | 5 | `cmd/curio/rpc/rpc.go:290`, `tasks/snap/task_submit.go:259` | Reverse lookup sector → (deadline, partition). Scan partitions until found. |
| 56 | `StateDealProviderCollateralBounds(size, verified, tsk)` | C | 2 | `cmd/sptool/toolbox_deal_client.go:481`, `market/mk12/mk12.go:340` | Read power + reward actors, apply formula. |
| 57 | `StateListMessages(match, tsk, fromEpoch)` | R | 0 (declared) | | Scan messages from `fromEpoch` matching predicate. Heavy. Defer until used. |
| 58 | `StateListMiners(tsk)` | R | 1 | `cmd/sptool/toolbox_curio_stats.go:38` | List all miner IDs from power actor's `Claims` HAMT keys. Heavy. Used only by an SP "stats" tool — fine to be slow. |
| 59 | `StateMarketBalance(addr, tsk)` | R | 8 | `cmd/curio/market.go:305`, `cmd/sptool/toolbox_deal_client.go:734` | Read `Escrow` + `Locked` HAMT entries in market actor. |
| 60 | `StateMarketStorageDeal(dealId, tsk)` | R | 1 | `tasks/storage-market/task_find_deal.go:231` | AMT lookup on market actor proposals + states. |
| 61 | `StateMinerFaults(maddr, tsk)` | R | 0 (declared) | | Aggregate fault bitfields across deadlines. |
| 62 | `StateMinerRecoveries(maddr, tsk)` | R | 0 (declared) | | Aggregate recovery bitfields. |
| 63 | `StateNetworkName()` | R | 1 | `deps/apiinfo.go:71` | Static once known: `"mainnet"` or `"calibrationnet"`. Pulled from init actor on first call, then cached. |
| 64 | `StateReadState(addr, tsk)` | R | 0 (declared) | | Generic JSON dump of actor state. Used by `lotus state read-state`. |
| 65 | `StateVMCirculatingSupplyInternal(tsk)` | R | 0 (declared) | | Heavy: reads several actors + applies vesting math. Defer. |
| 66 | `StateVerifiedClientStatus(addr, tsk)` | R | 4 | `cmd/sptool/toolbox_deal_tools.go:908`, `cmd/sptool/evm.go:200` | HAMT lookup on verifreg actor's `VerifiedClients` map. |
| 67 | `StateMinerSectorCount(maddr, tsk)` | R | 0 (declared) | | Active + faulty + live counts. Derive from `StateMinerSectors` if needed. |
| 68 | `StateCirculatingSupply(tsk)` | R | 0 (declared) | | Same heavy compute as method 65, public shape. |
| 69 | `StateCall(msg, tsk)` | C | 11 | `cmd/sptool/evm.go:133`, `cmd/sptool/toolbox_ipni.go:118`, `lib/proofsvc/common/l1ops.go:580` | **Read-only VM eval.** Executes a message against a tipset's state without persisting, returns trace + result. Required for `eth_call` and gas estimation. The hardest port; deferred to Phase 4. |
| 70 | `MarketAddBalance(wallet, addr, amt)` | C+W+G | 0 (declared) | also called by webrpc | Convenience: compose+sign+push a market deposit message. Equivalent to `MpoolPushMessage` with the message body filled in. |
| 71 | `StateMinerCreationDeposit(tsk)` | R | 1 | `lib/createminer/create_miner.go:108` | Read reward actor + power actor, apply formula. |

## Extra calls from the Curio web UI (`web/api/webrpc/`)

These reach the same `deps.Chain` interface, so the surface above already
covers them. For completeness, the web layer additionally exercises:
`ChainHead`, `MpoolPushMessage`, `StateAccountKey`, `StateGetActor`,
`StateGetRandomnessFromBeacon`, `StateLookupID`, `StateMarketBalance`,
`StateMinerDeadlines`, `StateMinerInfo`, `StateMinerPartitions`,
`StateMinerPower`, `StateMinerProvingDeadline`, `StateNetworkVersion`,
`StateSectorGetInfo`, `StateSectorPartition`, `WalletBalance` — all
already on the table above. So our V1 list is complete.

## Counts by category

| Category | Methods | Implementation phase |
|----------|---------|----------------------|
| **N** node admin | 5 | trivial wrappers, ship in Phase 4 |
| **W** wallet local | 3 (#17, #24, #35) | Phase 3 |
| **R** read state | 47 | Phase 2 (HAMT/AMT) + Phase 4 (RPC shim) |
| **C** compute on state | 10 (#13, #28, #38, #39, #53, #56, #69, #70, plus parts of #11 if approached as formula) | Phase 4, with VM-call hardest |
| **G** gossip / publish | 4 (#16, #31, #52, plus the G half of #53/#70) | Phase 3 |
| **S** streaming | 1 (#7 `ChainNotify`) | Phase 4. Implementation = local follower; emits "apply" on each header gossip, "revert"+"apply" on reorg. |
| **SP** block production | 4 (#31, #32, #33, #34) | **Phase 7** — needed only for SPs producing blocks themselves; an SP without winning-POST infra would never call these. |

## Surface size and validation strategy

71 declared methods, but only ~52 see real call sites in Curio source today.
The 19 with zero call sites are either (a) inherited from Lotus interface
parity, (b) used only inside Curio's tests or itests, or (c) latent for
future tasks. **All 71 still need to compile** so the interface lines up at
build time, even if some return `ErrNotImplemented` in V1.

Validation plan, ordered by risk:

1. Stand up Lantern with **only** `ChainHead` + `ChainNotify` + `StateGetActor`
   + `StateLookupID` working. Point `sptool actor` at it. Confirms the
   plumbing end-to-end.
2. Add `StateMinerInfo`, `StateMinerSectors`, `StateMinerPower`,
   `StateMinerProvingDeadline`, `StateMinerPartitions`,
   `StateGetRandomnessFromBeacon`, `StateGetRandomnessFromTickets`. Replay
   real WindowPoSt deadlines from a calibration miner using Curio.
   ⇒ Phase 7 milestone.
3. Add `WalletSign`, `MpoolPush`, `MpoolPushMessage`, `MpoolGetNonce`,
   `GasEstimate*`. Submit an actual message on calibration. ⇒ message flow
   is working.
4. Add `StateCall`. Now Curio's storage-market PSD verification works.
5. Add `StateWaitMsg`, `StateSearchMsg`, `ChainGetMessage`. Curio's deal
   pipeline now retains correctness.
6. Add `MinerGetBaseInfo`, `MinerCreateBlock`, `MpoolSelect`, `SyncSubmitBlock`.
   ⇒ Curio can mine. Phase 7 close-out.

After step 6 we can swap the FULLNODE_API_INFO on sp.reiers.io live and let
the miner run for a full proving period to validate.
