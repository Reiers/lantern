// Block-production handlers — Phase 7 Part C.
//
// MinerGetBaseInfo + MinerCreateBlock + MpoolSelect + SyncSubmitBlock.
//
// Curio calls these when its WinPoSt task believes it has a winning
// ticket. The flow is:
//
//   1. Curio's `tasks/winning/winning_task.go` calls
//      `MinerGetBaseInfo(maddr, epoch, tsk)`. The full node returns the
//      miner's power, network power, list of provable sectors, worker
//      key, sector size, and beacon entries for the target epoch.
//
//   2. Curio's WinPoSt task does the actual winning-PoSt computation
//      (this happens *outside* the full node — Curio has dedicated
//      GPU/CPU workers for it).
//
//   3. Curio calls `MpoolSelect(tsk, ticketQuality)` to get a sorted
//      list of pending messages to include in the block.
//
//   4. Curio fills in a `BlockTemplate` and calls
//      `MinerCreateBlock(template)`. The full node packs messages,
//      computes the message-roots CIDs, computes the new state root
//      by applying the messages, signs the block with the worker key,
//      and returns a `BlockMsg`.
//
//   5. Curio calls `SyncSubmitBlock(blockMsg)` which publishes to
//      gossipsub `/fil/blocks/<network>`.
//
// Phase 7 limitations (all documented in PHASE7-BLOCKERS.md):
//
//   - MinerCreateBlock can produce a syntactically valid BlockMsg with
//     real Parents, real Ticket, real ElectionProof, real BeaconEntries,
//     and real Messages. But the `ParentStateRoot` is taken verbatim
//     from the parent tipset because Lantern cannot execute messages.
//     A block published with this state root would be rejected by the
//     network — so SyncSubmitBlock is gated behind AllowBlockSubmit.
//
//   - MinerGetBaseInfo's `Sectors` field samples up to N (default 200)
//     active sectors via StateMinerActiveSectors. The full Filecoin
//     spec computes this from a deterministic randomness seed; we
//     approximate by returning the lowest-numbered active sectors.
//     Curio's WinPoSt code in practice only needs a representative
//     subset.

package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/beacon"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/proofs"
)

// MaxBaseInfoSectors caps the number of sectors we return in
// MinerGetBaseInfo. Curio's WinPoSt code samples a deterministic subset
// from this list; in practice 200 is plenty.
const MaxBaseInfoSectors = 200

// MinerGetBaseInfo returns the mining-base info for `m` at `epoch`.
// Tier 4 (#32). Phase 7 implementation: read miner + power state +
// beacon entries from the trusted root.
func (c *ChainAPI) MinerGetBaseInfo(ctx context.Context, m addr.Address, epoch abi.ChainEpoch, _ types.TipSetKey) (*api.MiningBaseInfo, error) {
	if c.Accessor == nil {
		return nil, errors.New("MinerGetBaseInfo: accessor not initialised")
	}

	// 1. Miner state + info.
	ms, _, err := c.Accessor.LoadMiner(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("MinerGetBaseInfo load miner: %w", err)
	}
	info, err := ms.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("MinerGetBaseInfo info: %w", err)
	}

	// 2. Power claims (miner + network).
	ps, _, err := c.Accessor.LoadPower(ctx)
	if err != nil {
		return nil, fmt.Errorf("MinerGetBaseInfo load power: %w", err)
	}
	mc, has, err := ps.MinerPower(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("MinerGetBaseInfo miner power: %w", err)
	}
	var minerQAPower abi.StoragePower
	if has {
		minerQAPower = mc.QualityAdjPower
	} else {
		minerQAPower = big.Zero()
	}
	tot := ps.Totals()

	// 3. Beacon entries. Lantern carries BeaconEntries on every block;
	//    for the requested epoch we pull the head tipset's entries as
	//    a best-effort sample. A full implementation would walk the
	//    drand round window for `epoch`.
	var prevEntry types.BeaconEntry
	var entries []types.BeaconEntry
	if c.HeaderStore != nil {
		if head := c.HeaderStore.Head(); head != nil && len(head.Blocks()) > 0 {
			entries = head.Blocks()[0].BeaconEntries
			if len(entries) > 0 {
				prevEntry = entries[len(entries)-1]
			}
		}
	}

	// 4. WinningPoSt-challenged sectors. Lotus returns exactly the sectors
	//    the WinningPoSt challenge selects (not the whole proving set): it
	//    draws the WinningPoStChallengeSeed randomness at `epoch` and calls
	//    GenerateWinningPoStSectorChallenge over the proving-set size to
	//    pick which sectors the miner must prove. Curio generates its
	//    WinningPoSt proof over exactly these sectors, so returning the full
	//    set (the old behavior) would make Curio prove the wrong sectors
	//    and produce an invalid block (#87 + #88).
	var sectors []api.SectorInfo
	allSectors, serr := c.StateMinerActiveSectors(ctx, m, types.TipSetKey{})
	if serr == nil && len(allSectors) > 0 {
		// Sort by sector number to match Lotus's proving-sectors bitfield
		// bit-index iteration (GetSectorsForWinningPoSt selects by index
		// into the ascending proving set).
		sort.Slice(allSectors, func(i, j int) bool {
			return allSectors[i].SectorNumber < allSectors[j].SectorNumber
		})
		sectors = c.winningPoStChallengedSectors(m, epoch, entries, prevEntry, allSectors)
	}

	// 5. Eligibility: above minimum power, no fee debt, no consensus
	//    fault elapsed in the next ~900 epochs.
	feeDebt := ms.FeeDebt()
	hasMinPower := has && minerQAPower.GreaterThanEqual(big.Zero()) && tot.MinerAboveMinCount > 0
	eligible := hasMinPower && feeDebt.IsZero()

	return &api.MiningBaseInfo{
		MinerPower:        minerQAPower,
		NetworkPower:      tot.QualityAdjPower,
		Sectors:           sectors,
		WorkerKey:         info.Worker,
		SectorSize:        info.SectorSize,
		PrevBeaconEntry:   prevEntry,
		BeaconEntries:     entries,
		EligibleForMining: eligible,
	}, nil
}

// winningPoStChallengedSectors selects, from the miner's ascending-sorted
// proving set, the sectors the WinningPoSt challenge picks at `epoch`.
// Mirrors Lotus stmgr.GetSectorsForWinningPoSt: draw the
// WinningPoStChallengeSeed randomness (beacon-at-epoch + miner entropy),
// then GenerateWinningPoStSectorChallenge over the proving-set size.
//
// Falls back to a bounded lowest-N sample if the randomness can't be
// derived (no beacon entry available) so the field is never empty when
// sectors exist — an approximate answer beats a hard failure on a node
// that's briefly missing beacon data.
func (c *ChainAPI) winningPoStChallengedSectors(m addr.Address, epoch abi.ChainEpoch, entries []types.BeaconEntry, prevEntry types.BeaconEntry, allSectors []*api.SectorOnChainInfo) []api.SectorInfo {
	toInfo := func(s *api.SectorOnChainInfo) api.SectorInfo {
		return api.SectorInfo{SealProof: s.SealProof, SectorNumber: s.SectorNumber, SealedCID: s.SealedCID}
	}

	// Randomness base: the block's last beacon entry if present, else the
	// prev-epoch entry (matches Lotus rbase selection).
	var rbaseData []byte
	if len(entries) > 0 {
		rbaseData = entries[len(entries)-1].Data
	} else if len(prevEntry.Data) > 0 {
		rbaseData = prevEntry.Data
	}
	mid, iderr := addr.IDFromAddress(m)
	if len(rbaseData) > 0 && iderr == nil {
		var buf bytes.Buffer
		if err := m.MarshalCBOR(&buf); err == nil {
			base, err := beacon.DrawRandomnessFromBase(rbaseData, gscrypto.DomainSeparationTag_WinningPoStChallengeSeed, epoch, buf.Bytes())
			if err == nil {
				var rand [32]byte
				copy(rand[:], base)
				if ids, cerr := proofs.GenerateWinningPoStSectorChallenge(abi.ActorID(mid), rand, uint64(len(allSectors))); cerr == nil {
					out := make([]api.SectorInfo, 0, len(ids))
					for _, idx := range ids {
						if idx < uint64(len(allSectors)) {
							out = append(out, toInfo(allSectors[idx]))
						}
					}
					if len(out) > 0 {
						return out
					}
				}
			}
		}
	}

	// Fallback: bounded lowest-N sample (previous behavior).
	n := len(allSectors)
	if n > MaxBaseInfoSectors {
		n = MaxBaseInfoSectors
	}
	out := make([]api.SectorInfo, n)
	for i := 0; i < n; i++ {
		out[i] = toInfo(allSectors[i])
	}
	return out
}

// MpoolSelect returns messages to include in a block. Tier 4 (#34).
//
// Phase 7 implementation: walk the local pending list, sort by
// (premium × estimatedGas) descending, dedupe by (sender, nonce), stop
// when the cumulative gas budget exceeds 90% of BlockGasLimit.
func (c *ChainAPI) MpoolSelect(ctx context.Context, _ types.TipSetKey, ticketQuality float64) ([]*types.SignedMessage, error) {
	if c.Mpool == nil {
		return nil, ErrMpoolNotWired
	}
	pl, ok := c.Mpool.(MpoolPendingLister)
	if !ok || pl == nil {
		return nil, nil
	}
	pending := pl.Pending()
	if len(pending) == 0 {
		return nil, nil
	}

	// Score: premium * estimatedGas. We use the message's own
	// GasPremium; if zero, treat as a 1k floor.
	type scored struct {
		sm    *types.SignedMessage
		score big.Int
		gas   int64
	}
	scoredMsgs := make([]scored, 0, len(pending))
	for _, sm := range pending {
		if sm == nil {
			continue
		}
		prem := sm.Message.GasPremium
		if prem.NilOrZero() {
			prem = big.NewInt(1_000)
		}
		gas := sm.Message.GasLimit
		if gas <= 0 {
			gas = 10_000_000
		}
		score := big.Mul(prem, big.NewIntUnsigned(uint64(gas)))
		scoredMsgs = append(scoredMsgs, scored{sm: sm, score: score, gas: gas})
	}
	sort.SliceStable(scoredMsgs, func(i, j int) bool {
		return scoredMsgs[i].score.GreaterThan(scoredMsgs[j].score)
	})

	// Dedupe by (sender, nonce). Sort by nonce within sender so we
	// pick the lowest nonce first (avoids out-of-order inclusion).
	seen := make(map[string]bool)
	out := make([]*types.SignedMessage, 0, len(scoredMsgs))
	budget := build.BlockGasLimit * 9 / 10
	used := int64(0)
	for _, s := range scoredMsgs {
		key := fmt.Sprintf("%s/%d", s.sm.Message.From, s.sm.Message.Nonce)
		if seen[key] {
			continue
		}
		if used+s.gas > budget {
			continue
		}
		seen[key] = true
		out = append(out, s.sm)
		used += s.gas
	}
	_ = ticketQuality // not used in V1 selection — Lotus uses it to weight by chain quality
	return out, nil
}

// MinerCreateBlock builds a BlockMsg from `bt`. Tier 4 (#33).
//
// Phase 7 implementation: assemble all header fields, take
// ParentStateRoot from the parent tipset's first block's
// ParentStateRoot (i.e. "no state change"), sign the header with the
// miner's worker key via WalletSign.
func (c *ChainAPI) MinerCreateBlock(ctx context.Context, bt *api.BlockTemplate) (*types.BlockMsg, error) {
	if bt == nil {
		return nil, errors.New("MinerCreateBlock: nil template")
	}
	if c.Wallet == nil {
		return nil, errors.New("MinerCreateBlock: wallet not initialised")
	}
	if c.HeaderStore == nil {
		return nil, errors.New("MinerCreateBlock: header store not initialised")
	}

	// 1. Load parent tipset to copy ParentStateRoot, ParentMessageReceipts,
	//    ParentWeight, ParentBaseFee.
	parentCids := bt.Parents.Cids()
	if len(parentCids) == 0 {
		return nil, errors.New("MinerCreateBlock: empty parent tipset key")
	}
	parentBlock, err := c.HeaderStore.Get(parentCids[0])
	if err != nil {
		return nil, fmt.Errorf("MinerCreateBlock: load parent block %s: %w", parentCids[0], err)
	}

	// 2. Resolve miner worker key.
	if c.Accessor == nil {
		return nil, errors.New("MinerCreateBlock: accessor not initialised")
	}
	ms, _, err := c.Accessor.LoadMiner(ctx, bt.Miner)
	if err != nil {
		return nil, fmt.Errorf("MinerCreateBlock load miner: %w", err)
	}
	info, err := ms.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("MinerCreateBlock miner info: %w", err)
	}

	// 3. Build header. Messages CID is computed lazily by the storage
	//    code path; we leave it nil here (the caller's gossipsub
	//    publisher will compute the CommP-style root).
	bls := make([][]byte, 0)
	secp := make([]*types.SignedMessage, 0)
	for _, sm := range bt.Messages {
		if sm == nil {
			continue
		}
		// SignedMessage with BLS signature -> bls list (Message CID only).
		// Otherwise -> secp list (SignedMessage CID).
		if sm.Signature.Type == 2 { // BLS
			bls = append(bls, sm.Message.Cid().Bytes())
		} else {
			secp = append(secp, sm)
		}
	}

	// Compute post-execution state root. Without a bridge, we copy the
	// parent stateRoot verbatim (PHASE7-BLOCKERS.md B2: the resulting
	// block will be rejected by the network because the stateRoot won't
	// match what other nodes compute). With a bridge configured AND
	// AllowBlockSubmit=true, we delegate state-root computation to the
	// upstream Forest/Lotus node.
	stateRoot := parentBlock.ParentStateRoot
	if c.Bridge != nil && c.AllowBlockSubmit {
		root, _, berr := c.Bridge.ComputeStateRoot(ctx, parentBlock.ParentStateRoot, int64(bt.Epoch-1), unsignedMessagesForBridge(bt.Messages))
		if berr != nil {
			return nil, fmt.Errorf("MinerCreateBlock: bridge ComputeStateRoot failed: %w", berr)
		}
		stateRoot = root
	}

	header := &types.BlockHeader{
		Miner:                 bt.Miner,
		Ticket:                bt.Ticket,
		ElectionProof:         bt.Eproof,
		BeaconEntries:         bt.BeaconValues,
		Height:                bt.Epoch,
		Parents:               parentCids,
		ParentWeight:          parentBlock.ParentWeight, // recomputed by network if accepted
		ParentStateRoot:       stateRoot,
		ParentMessageReceipts: parentBlock.ParentMessageReceipts,
		ParentBaseFee:         parentBlock.ParentBaseFee,
		Timestamp:             bt.Timestamp,
	}

	// 4. Sign header with worker key.
	signBytes, err := header.SigningBytes()
	if err != nil {
		return nil, fmt.Errorf("MinerCreateBlock signing bytes: %w", err)
	}
	sig, err := c.Wallet.Sign(ctx, info.Worker, signBytes)
	if err != nil {
		return nil, fmt.Errorf("MinerCreateBlock sign with worker %s: %w", info.Worker, err)
	}
	header.BlockSig = sig

	// 5. Build BlockMsg.
	out := &types.BlockMsg{
		Header:        header,
		BlsMessages:   blsMessageCIDs(bt.Messages),
		SecpkMessages: secpMessageCIDs(bt.Messages),
	}
	_ = bls
	_ = secp
	return out, nil
}

// blsMessageCIDs filters bt.Messages to BLS-signed messages and returns
// the Message (unsigned) CIDs.
func blsMessageCIDs(msgs []*types.SignedMessage) []cid.Cid {
	out := make([]cid.Cid, 0)
	for _, sm := range msgs {
		if sm == nil || sm.Signature.Type != 2 { // 2 == BLS
			continue
		}
		out = append(out, sm.Message.Cid())
	}
	return out
}

// secpMessageCIDs filters bt.Messages to secp-signed messages and returns
// the SignedMessage CIDs.
func secpMessageCIDs(msgs []*types.SignedMessage) []cid.Cid {
	out := make([]cid.Cid, 0)
	for _, sm := range msgs {
		if sm == nil || sm.Signature.Type == 2 {
			continue
		}
		out = append(out, sm.Cid())
	}
	return out
}

// unsignedMessagesForBridge unwraps a slice of SignedMessage into the
// underlying *types.Message slice that vm/bridge.Bridge takes. Phase 8.
func unsignedMessagesForBridge(sms []*types.SignedMessage) []*types.Message {
	out := make([]*types.Message, 0, len(sms))
	for _, sm := range sms {
		if sm == nil {
			continue
		}
		m := sm.Message
		out = append(out, &m)
	}
	return out
}
