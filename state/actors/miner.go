// Versioned miner-actor loader.
//
// Goal: given the actor's Head CID + Code CID, fetch the state block, pick
// the right go-state-types/builtin/vN/miner package, decode, and expose a
// normalized MinerState handle.
//
// Phase 5 scope: actors v17 and v18 are the active mainnet versions
// (nv25 / nv26). Earlier versions (v8..v16) are recognized by the Registry
// for code-CID identification, but loading their state returns
// ErrUnsupportedVersion; that's a Phase 5/6 follow-up.
//
// All decoders are exactly the cbor-gen routines shipped by go-state-types;
// we never touch raw CBOR bytes except to dispatch on actor version.

package actors

import (
	"bytes"
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/dline"
	blockformat "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"

	miner17 "github.com/filecoin-project/go-state-types/builtin/v17/miner"
	adt17 "github.com/filecoin-project/go-state-types/builtin/v17/util/adt"
	miner18 "github.com/filecoin-project/go-state-types/builtin/v18/miner"
	adt18 "github.com/filecoin-project/go-state-types/builtin/v18/util/adt"

	"github.com/Reiers/lantern/state/hamt"
)

// decodePeerID converts the on-chain miner Info.PeerId bytes (the raw
// libp2p peer.ID encoding) into the base58-multihash string that Lotus
// returns over JSON-RPC. The on-chain value is what `peer.ID.MarshalBinary`
// produces; Lotus's `peer.ID.String()` is its base58-encoded multihash.
//
// Returns nil if the bytes are empty or not a valid peer.ID, so callers
// can fall back to a nil PeerId field consistent with Lotus's behaviour
// for miners that have never declared one.
func decodePeerID(b []byte) *string {
	if len(b) == 0 {
		return nil
	}
	pid, err := libp2ppeer.IDFromBytes(b)
	if err != nil {
		return nil
	}
	s := pid.String()
	return &s
}

// MinerInfo is a network-version-agnostic view of MinerInfo. It mirrors the
// shape of api.MinerInfo (Lotus-compatible).
type MinerInfo struct {
	Owner                      address.Address
	Worker                     address.Address
	NewWorker                  address.Address // f00 if no pending change
	ControlAddresses           []address.Address
	WorkerChangeEpoch          abi.ChainEpoch
	PeerId                     *string // base58
	Multiaddrs                 [][]byte
	WindowPoStProofType        abi.RegisteredPoStProof
	SectorSize                 abi.SectorSize
	WindowPoStPartitionSectors uint64
	ConsensusFaultElapsed      abi.ChainEpoch
	Beneficiary                address.Address
	BeneficiaryQuota           abi.TokenAmount
	BeneficiaryUsedQuota       abi.TokenAmount
	BeneficiaryExpiration      abi.ChainEpoch
	PendingBeneficiary         *PendingBeneficiaryChange
}

// PendingBeneficiaryChange is the version-agnostic version of
// miner.PendingBeneficiaryChange.
type PendingBeneficiaryChange struct {
	NewBeneficiary        address.Address
	NewQuota              abi.TokenAmount
	NewExpiration         abi.ChainEpoch
	ApprovedByBeneficiary bool
	ApprovedByNominee     bool
}

// MinerState is the network-version-agnostic interface for working with a
// miner actor's state.
type MinerState interface {
	Version() int
	Info(ctx context.Context) (*MinerInfo, error)
	LockedFunds() (vesting, initialPledge, preCommitDeposits abi.TokenAmount)
	FeeDebt() abi.TokenAmount
	AvailableBalance(actorBalance abi.TokenAmount) (abi.TokenAmount, error)
	ProvingPeriodStart() abi.ChainEpoch
	CurrentDeadline() uint64
	SectorsAMTRoot() cid.Cid
	PreCommitsHAMTRoot() cid.Cid
	DeadlinesRoot() cid.Cid
	AllocatedSectorsRoot() cid.Cid
	// GetSector loads a SectorOnChainInfo from the Sectors AMT. The
	// returned shape is the *latest* (v18) struct; pre-v17 sectors are
	// upgraded by zeroing the new fields.
	GetSector(ctx context.Context, sectorNo abi.SectorNumber) (*miner18.SectorOnChainInfo, bool, error)
	// GetPrecommit loads a SectorPreCommitOnChainInfo from the precommits HAMT.
	GetPrecommit(ctx context.Context, sectorNo abi.SectorNumber) (*miner18.SectorPreCommitOnChainInfo, bool, error)
	// FindSector returns the (deadline, partition) location of a sector.
	FindSector(ctx context.Context, sno abi.SectorNumber) (uint64, uint64, error)
	DeadlineCount() uint64
	Deadline(ctx context.Context, dlIdx uint64) (MinerDeadline, error)
	Partitions(ctx context.Context, dlIdx uint64) ([]MinerPartition, error)
	Faults(ctx context.Context) (bitfield.BitField, error)
	Recoveries(ctx context.Context) (bitfield.BitField, error)
	ProvingDeadlineInfo(currEpoch abi.ChainEpoch) *dline.Info
	// AllSectors iterates the Sectors AMT, invoking cb for each. If the
	// filter bitfield is non-nil, only sectors in the filter are returned.
	AllSectors(ctx context.Context, filter *bitfield.BitField, cb func(*miner18.SectorOnChainInfo) error) error
}

// MinerDeadline is a version-agnostic view of one deadline's metadata.
type MinerDeadline struct {
	PostSubmissions      bitfield.BitField
	DisputableProofCount uint64
	PartitionsCount      uint64
	PartitionsRoot       cid.Cid // AMT of Partition
}

// MinerPartition is a version-agnostic view of one partition.
type MinerPartition struct {
	AllSectors        bitfield.BitField
	FaultySectors     bitfield.BitField
	RecoveringSectors bitfield.BitField
	LiveSectors       bitfield.BitField
	ActiveSectors     bitfield.BitField
}

// ErrUnsupportedVersion is returned by LoadMiner when the actor version is
// older than the V1 cut (v17). See PHASE5-BLOCKERS.md.
type ErrUnsupportedVersion struct {
	Kind    Kind
	Version int
}

func (e ErrUnsupportedVersion) Error() string {
	return fmt.Sprintf("lantern: %s actor v%d not yet supported in V1 (only v17/v18). See PHASE5-BLOCKERS.md", e.Kind, e.Version)
}

// LoadMiner fetches the actor's head state and returns a versioned wrapper.
func LoadMiner(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (MinerState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindMiner {
		return nil, fmt.Errorf("LoadMiner: code %s is %s, not storageminer", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch miner head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("miner head: %w", err)
	}
	store := newCborStore(bg)
	switch info.Version {
	case 17:
		var s miner17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding miner v17: %w", err)
		}
		return &minerV17{s: &s, store: adt17.WrapStore(ctx, store), ctx: ctx}, nil
	case 18:
		var s miner18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding miner v18: %w", err)
		}
		return &minerV18{s: &s, store: adt18.WrapStore(ctx, store), ctx: ctx}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindMiner, Version: info.Version}
}

// ----- v18 -----

type minerV18 struct {
	s     *miner18.State
	store adt18.Store
	ctx   context.Context
}

func (m *minerV18) Version() int { return 18 }

func (m *minerV18) Info(ctx context.Context) (*MinerInfo, error) {
	info, err := m.s.GetInfo(m.store)
	if err != nil {
		return nil, fmt.Errorf("miner v18 GetInfo: %w", err)
	}
	return convertMinerV18Info(info), nil
}

func (m *minerV18) LockedFunds() (abi.TokenAmount, abi.TokenAmount, abi.TokenAmount) {
	return m.s.LockedFunds, m.s.InitialPledge, m.s.PreCommitDeposits
}

func (m *minerV18) FeeDebt() abi.TokenAmount                              { return m.s.FeeDebt }
func (m *minerV18) AvailableBalance(b abi.TokenAmount) (abi.TokenAmount, error) {
	return m.s.GetAvailableBalance(b)
}
func (m *minerV18) ProvingPeriodStart() abi.ChainEpoch { return m.s.ProvingPeriodStart }
func (m *minerV18) CurrentDeadline() uint64            { return m.s.CurrentDeadline }
func (m *minerV18) SectorsAMTRoot() cid.Cid            { return m.s.Sectors }
func (m *minerV18) PreCommitsHAMTRoot() cid.Cid        { return m.s.PreCommittedSectors }
func (m *minerV18) DeadlinesRoot() cid.Cid             { return m.s.Deadlines }
func (m *minerV18) AllocatedSectorsRoot() cid.Cid      { return m.s.AllocatedSectors }

func (m *minerV18) GetSector(ctx context.Context, n abi.SectorNumber) (*miner18.SectorOnChainInfo, bool, error) {
	return m.s.GetSector(m.store, n)
}

func (m *minerV18) GetPrecommit(ctx context.Context, n abi.SectorNumber) (*miner18.SectorPreCommitOnChainInfo, bool, error) {
	return m.s.GetPrecommittedSector(m.store, n)
}

func (m *minerV18) FindSector(ctx context.Context, sno abi.SectorNumber) (uint64, uint64, error) {
	return m.s.FindSector(m.store, sno)
}

func (m *minerV18) DeadlineCount() uint64 { return uint64(miner18.WPoStPeriodDeadlines) }

func (m *minerV18) Deadline(ctx context.Context, dlIdx uint64) (MinerDeadline, error) {
	dls, err := m.s.LoadDeadlines(m.store)
	if err != nil {
		return MinerDeadline{}, fmt.Errorf("load deadlines v18: %w", err)
	}
	if dlIdx >= uint64(len(dls.Due)) {
		return MinerDeadline{}, fmt.Errorf("deadline index %d out of range", dlIdx)
	}
	dl, err := dls.LoadDeadline(m.store, dlIdx)
	if err != nil {
		return MinerDeadline{}, fmt.Errorf("load deadline %d v18: %w", dlIdx, err)
	}
	parts, err := adt18.AsArray(m.store, dl.Partitions, miner18.DeadlinePartitionsAmtBitwidth)
	if err != nil {
		return MinerDeadline{}, fmt.Errorf("load partitions v18: %w", err)
	}
	return MinerDeadline{
		PostSubmissions: dl.PartitionsPoSted,
		PartitionsCount: parts.Length(),
		PartitionsRoot:  dl.Partitions,
	}, nil
}

func (m *minerV18) Partitions(ctx context.Context, dlIdx uint64) ([]MinerPartition, error) {
	dls, err := m.s.LoadDeadlines(m.store)
	if err != nil {
		return nil, fmt.Errorf("load deadlines v18: %w", err)
	}
	if dlIdx >= uint64(len(dls.Due)) {
		return nil, fmt.Errorf("deadline index %d out of range", dlIdx)
	}
	dl, err := dls.LoadDeadline(m.store, dlIdx)
	if err != nil {
		return nil, fmt.Errorf("load deadline %d v18: %w", dlIdx, err)
	}
	parts, err := adt18.AsArray(m.store, dl.Partitions, miner18.DeadlinePartitionsAmtBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load partitions v18: %w", err)
	}
	var out []MinerPartition
	var p miner18.Partition
	err = parts.ForEach(&p, func(_ int64) error {
		live, err := p.LiveSectors()
		if err != nil {
			return err
		}
		active, err := p.ActiveSectors()
		if err != nil {
			return err
		}
		out = append(out, MinerPartition{
			AllSectors:        p.Sectors,
			FaultySectors:     p.Faults,
			RecoveringSectors: p.Recoveries,
			LiveSectors:       live,
			ActiveSectors:     active,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk partitions v18: %w", err)
	}
	return out, nil
}

func (m *minerV18) Faults(ctx context.Context) (bitfield.BitField, error) {
	return unionFaultsV18(m.s, m.store, false)
}

func (m *minerV18) Recoveries(ctx context.Context) (bitfield.BitField, error) {
	return unionFaultsV18(m.s, m.store, true)
}

func (m *minerV18) ProvingDeadlineInfo(currEpoch abi.ChainEpoch) *dline.Info {
	return miner18.NewDeadlineInfo(m.s.ProvingPeriodStart, m.s.CurrentDeadline, currEpoch)
}

func (m *minerV18) AllSectors(ctx context.Context, filter *bitfield.BitField, cb func(*miner18.SectorOnChainInfo) error) error {
	arr, err := adt18.AsArray(m.store, m.s.Sectors, miner18.SectorsAmtBitwidth)
	if err != nil {
		return fmt.Errorf("load sectors v18: %w", err)
	}
	if filter == nil {
		var si miner18.SectorOnChainInfo
		return arr.ForEach(&si, func(_ int64) error {
			cp := si
			return cb(&cp)
		})
	}
	return filter.ForEach(func(s uint64) error {
		var si miner18.SectorOnChainInfo
		found, err := arr.Get(s, &si)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		cp := si
		return cb(&cp)
	})
}

func convertMinerV18Info(in *miner18.MinerInfo) *MinerInfo {
	peer := decodePeerID(in.PeerId)
	mas := make([][]byte, 0, len(in.Multiaddrs))
	for _, m := range in.Multiaddrs {
		mas = append(mas, []byte(m))
	}
	out := &MinerInfo{
		Owner:                      in.Owner,
		Worker:                     in.Worker,
		ControlAddresses:           in.ControlAddresses,
		PeerId:                     peer,
		Multiaddrs:                 mas,
		WindowPoStProofType:        in.WindowPoStProofType,
		SectorSize:                 in.SectorSize,
		WindowPoStPartitionSectors: in.WindowPoStPartitionSectors,
		ConsensusFaultElapsed:      in.ConsensusFaultElapsed,
		Beneficiary:                in.Beneficiary,
		BeneficiaryQuota:           in.BeneficiaryTerm.Quota,
		BeneficiaryUsedQuota:       in.BeneficiaryTerm.UsedQuota,
		BeneficiaryExpiration:      in.BeneficiaryTerm.Expiration,
	}
	if in.PendingWorkerKey != nil {
		out.NewWorker = in.PendingWorkerKey.NewWorker
		out.WorkerChangeEpoch = in.PendingWorkerKey.EffectiveAt
	} else {
		out.NewWorker, _ = address.NewIDAddress(0)
		out.WorkerChangeEpoch = -1
	}
	if in.PendingBeneficiaryTerm != nil {
		out.PendingBeneficiary = &PendingBeneficiaryChange{
			NewBeneficiary:        in.PendingBeneficiaryTerm.NewBeneficiary,
			NewQuota:              in.PendingBeneficiaryTerm.NewQuota,
			NewExpiration:         in.PendingBeneficiaryTerm.NewExpiration,
			ApprovedByBeneficiary: in.PendingBeneficiaryTerm.ApprovedByBeneficiary,
			ApprovedByNominee:     in.PendingBeneficiaryTerm.ApprovedByNominee,
		}
	}
	return out
}

func unionFaultsV18(s *miner18.State, store adt18.Store, recoveries bool) (bitfield.BitField, error) {
	dls, err := s.LoadDeadlines(store)
	if err != nil {
		return bitfield.BitField{}, fmt.Errorf("load deadlines v18: %w", err)
	}
	var acc []bitfield.BitField
	err = dls.ForEach(store, func(_ uint64, dl *miner18.Deadline) error {
		parts, err := adt18.AsArray(store, dl.Partitions, miner18.DeadlinePartitionsAmtBitwidth)
		if err != nil {
			return fmt.Errorf("load partitions: %w", err)
		}
		var p miner18.Partition
		return parts.ForEach(&p, func(_ int64) error {
			if recoveries {
				acc = append(acc, p.Recoveries)
			} else {
				acc = append(acc, p.Faults)
			}
			return nil
		})
	})
	if err != nil {
		return bitfield.BitField{}, err
	}
	return bitfield.MultiMerge(acc...)
}

// ----- v17 -----
//
// v17 has the same struct shapes as v18 except for a couple of new fields
// added in v18. We share helpers where possible by converting v17 → v18 at
// the boundary.

type minerV17 struct {
	s     *miner17.State
	store adt17.Store
	ctx   context.Context
}

func (m *minerV17) Version() int { return 17 }

func (m *minerV17) Info(ctx context.Context) (*MinerInfo, error) {
	info, err := m.s.GetInfo(m.store)
	if err != nil {
		return nil, fmt.Errorf("miner v17 GetInfo: %w", err)
	}
	return convertMinerV17Info(info), nil
}

func (m *minerV17) LockedFunds() (abi.TokenAmount, abi.TokenAmount, abi.TokenAmount) {
	return m.s.LockedFunds, m.s.InitialPledge, m.s.PreCommitDeposits
}

func (m *minerV17) FeeDebt() abi.TokenAmount                              { return m.s.FeeDebt }
func (m *minerV17) AvailableBalance(b abi.TokenAmount) (abi.TokenAmount, error) {
	return m.s.GetAvailableBalance(b)
}
func (m *minerV17) ProvingPeriodStart() abi.ChainEpoch { return m.s.ProvingPeriodStart }
func (m *minerV17) CurrentDeadline() uint64            { return m.s.CurrentDeadline }
func (m *minerV17) SectorsAMTRoot() cid.Cid            { return m.s.Sectors }
func (m *minerV17) PreCommitsHAMTRoot() cid.Cid        { return m.s.PreCommittedSectors }
func (m *minerV17) DeadlinesRoot() cid.Cid             { return m.s.Deadlines }
func (m *minerV17) AllocatedSectorsRoot() cid.Cid      { return m.s.AllocatedSectors }

func (m *minerV17) GetSector(ctx context.Context, n abi.SectorNumber) (*miner18.SectorOnChainInfo, bool, error) {
	si, found, err := m.s.GetSector(m.store, n)
	if err != nil || !found {
		return nil, found, err
	}
	return sectorV17to18(si), true, nil
}

func (m *minerV17) GetPrecommit(ctx context.Context, n abi.SectorNumber) (*miner18.SectorPreCommitOnChainInfo, bool, error) {
	pi, found, err := m.s.GetPrecommittedSector(m.store, n)
	if err != nil || !found {
		return nil, found, err
	}
	return precommitV17to18(pi), true, nil
}

func (m *minerV17) FindSector(ctx context.Context, sno abi.SectorNumber) (uint64, uint64, error) {
	return m.s.FindSector(m.store, sno)
}

func (m *minerV17) DeadlineCount() uint64 { return uint64(miner17.WPoStPeriodDeadlines) }

func (m *minerV17) Deadline(ctx context.Context, dlIdx uint64) (MinerDeadline, error) {
	dls, err := m.s.LoadDeadlines(m.store)
	if err != nil {
		return MinerDeadline{}, fmt.Errorf("load deadlines v17: %w", err)
	}
	if dlIdx >= uint64(len(dls.Due)) {
		return MinerDeadline{}, fmt.Errorf("deadline index %d out of range", dlIdx)
	}
	dl, err := dls.LoadDeadline(m.store, dlIdx)
	if err != nil {
		return MinerDeadline{}, fmt.Errorf("load deadline %d v17: %w", dlIdx, err)
	}
	parts, err := adt17.AsArray(m.store, dl.Partitions, miner17.DeadlinePartitionsAmtBitwidth)
	if err != nil {
		return MinerDeadline{}, fmt.Errorf("load partitions v17: %w", err)
	}
	return MinerDeadline{
		PostSubmissions: dl.PartitionsPoSted,
		PartitionsCount: parts.Length(),
		PartitionsRoot:  dl.Partitions,
	}, nil
}

func (m *minerV17) Partitions(ctx context.Context, dlIdx uint64) ([]MinerPartition, error) {
	dls, err := m.s.LoadDeadlines(m.store)
	if err != nil {
		return nil, fmt.Errorf("load deadlines v17: %w", err)
	}
	if dlIdx >= uint64(len(dls.Due)) {
		return nil, fmt.Errorf("deadline index %d out of range", dlIdx)
	}
	dl, err := dls.LoadDeadline(m.store, dlIdx)
	if err != nil {
		return nil, fmt.Errorf("load deadline %d v17: %w", dlIdx, err)
	}
	parts, err := adt17.AsArray(m.store, dl.Partitions, miner17.DeadlinePartitionsAmtBitwidth)
	if err != nil {
		return nil, fmt.Errorf("load partitions v17: %w", err)
	}
	var out []MinerPartition
	var p miner17.Partition
	err = parts.ForEach(&p, func(_ int64) error {
		live, err := p.LiveSectors()
		if err != nil {
			return err
		}
		active, err := p.ActiveSectors()
		if err != nil {
			return err
		}
		out = append(out, MinerPartition{
			AllSectors:        p.Sectors,
			FaultySectors:     p.Faults,
			RecoveringSectors: p.Recoveries,
			LiveSectors:       live,
			ActiveSectors:     active,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk partitions v17: %w", err)
	}
	return out, nil
}

func (m *minerV17) Faults(ctx context.Context) (bitfield.BitField, error) {
	return unionFaultsV17(m.s, m.store, false)
}

func (m *minerV17) Recoveries(ctx context.Context) (bitfield.BitField, error) {
	return unionFaultsV17(m.s, m.store, true)
}

func (m *minerV17) ProvingDeadlineInfo(currEpoch abi.ChainEpoch) *dline.Info {
	return miner17.NewDeadlineInfo(m.s.ProvingPeriodStart, m.s.CurrentDeadline, currEpoch)
}

func (m *minerV17) AllSectors(ctx context.Context, filter *bitfield.BitField, cb func(*miner18.SectorOnChainInfo) error) error {
	arr, err := adt17.AsArray(m.store, m.s.Sectors, miner17.SectorsAmtBitwidth)
	if err != nil {
		return fmt.Errorf("load sectors v17: %w", err)
	}
	if filter == nil {
		var si miner17.SectorOnChainInfo
		return arr.ForEach(&si, func(_ int64) error {
			cp := si
			return cb(sectorV17to18(&cp))
		})
	}
	return filter.ForEach(func(s uint64) error {
		var si miner17.SectorOnChainInfo
		found, err := arr.Get(s, &si)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		cp := si
		return cb(sectorV17to18(&cp))
	})
}

func convertMinerV17Info(in *miner17.MinerInfo) *MinerInfo {
	peer := decodePeerID(in.PeerId)
	mas := make([][]byte, 0, len(in.Multiaddrs))
	for _, m := range in.Multiaddrs {
		mas = append(mas, []byte(m))
	}
	out := &MinerInfo{
		Owner:                      in.Owner,
		Worker:                     in.Worker,
		ControlAddresses:           in.ControlAddresses,
		PeerId:                     peer,
		Multiaddrs:                 mas,
		WindowPoStProofType:        in.WindowPoStProofType,
		SectorSize:                 in.SectorSize,
		WindowPoStPartitionSectors: in.WindowPoStPartitionSectors,
		ConsensusFaultElapsed:      in.ConsensusFaultElapsed,
		Beneficiary:                in.Beneficiary,
		BeneficiaryQuota:           in.BeneficiaryTerm.Quota,
		BeneficiaryUsedQuota:       in.BeneficiaryTerm.UsedQuota,
		BeneficiaryExpiration:      in.BeneficiaryTerm.Expiration,
	}
	if in.PendingWorkerKey != nil {
		out.NewWorker = in.PendingWorkerKey.NewWorker
		out.WorkerChangeEpoch = in.PendingWorkerKey.EffectiveAt
	} else {
		out.NewWorker, _ = address.NewIDAddress(0)
		out.WorkerChangeEpoch = -1
	}
	if in.PendingBeneficiaryTerm != nil {
		out.PendingBeneficiary = &PendingBeneficiaryChange{
			NewBeneficiary:        in.PendingBeneficiaryTerm.NewBeneficiary,
			NewQuota:              in.PendingBeneficiaryTerm.NewQuota,
			NewExpiration:         in.PendingBeneficiaryTerm.NewExpiration,
			ApprovedByBeneficiary: in.PendingBeneficiaryTerm.ApprovedByBeneficiary,
			ApprovedByNominee:     in.PendingBeneficiaryTerm.ApprovedByNominee,
		}
	}
	return out
}

func unionFaultsV17(s *miner17.State, store adt17.Store, recoveries bool) (bitfield.BitField, error) {
	dls, err := s.LoadDeadlines(store)
	if err != nil {
		return bitfield.BitField{}, fmt.Errorf("load deadlines v17: %w", err)
	}
	var acc []bitfield.BitField
	err = dls.ForEach(store, func(_ uint64, dl *miner17.Deadline) error {
		parts, err := adt17.AsArray(store, dl.Partitions, miner17.DeadlinePartitionsAmtBitwidth)
		if err != nil {
			return fmt.Errorf("load partitions: %w", err)
		}
		var p miner17.Partition
		return parts.ForEach(&p, func(_ int64) error {
			if recoveries {
				acc = append(acc, p.Recoveries)
			} else {
				acc = append(acc, p.Faults)
			}
			return nil
		})
	})
	if err != nil {
		return bitfield.BitField{}, err
	}
	return bitfield.MultiMerge(acc...)
}

// sectorV17to18 promotes a v17 SectorOnChainInfo to the v18 shape. The two
// versions share the same fields; the conversion is field-by-field.
func sectorV17to18(in *miner17.SectorOnChainInfo) *miner18.SectorOnChainInfo {
	return &miner18.SectorOnChainInfo{
		SectorNumber:          in.SectorNumber,
		SealProof:             in.SealProof,
		SealedCID:             in.SealedCID,
		DeprecatedDealIDs:     in.DeprecatedDealIDs,
		Activation:            in.Activation,
		Expiration:            in.Expiration,
		DealWeight:            in.DealWeight,
		VerifiedDealWeight:    in.VerifiedDealWeight,
		InitialPledge:         in.InitialPledge,
		ExpectedDayReward:     in.ExpectedDayReward,
		ExpectedStoragePledge: in.ExpectedStoragePledge,
		PowerBaseEpoch:        in.PowerBaseEpoch,
		ReplacedDayReward:     in.ReplacedDayReward,
		SectorKeyCID:          in.SectorKeyCID,
		Flags:                 miner18.SectorOnChainInfoFlags(in.Flags),
		DailyFee:              in.DailyFee,
	}
}

func precommitV17to18(in *miner17.SectorPreCommitOnChainInfo) *miner18.SectorPreCommitOnChainInfo {
	return &miner18.SectorPreCommitOnChainInfo{
		Info: miner18.SectorPreCommitInfo{
			SealProof:     in.Info.SealProof,
			SectorNumber:  in.Info.SectorNumber,
			SealedCID:     in.Info.SealedCID,
			SealRandEpoch: in.Info.SealRandEpoch,
			DealIDs:       in.Info.DealIDs,
			Expiration:    in.Info.Expiration,
			UnsealedCid:   in.Info.UnsealedCid,
		},
		PreCommitDeposit: in.PreCommitDeposit,
		PreCommitEpoch:   in.PreCommitEpoch,
	}
}

// newCborStore returns an ipldcbor.IpldStore backed by the BlockGetter.
func newCborStore(bg hamt.BlockGetter) cbornode.IpldStore {
	return cbornode.NewCborStore(&bgBlockstore{bg: bg})
}

// bgBlockstore adapts a hamt.BlockGetter to the ipldcbor.IpldBlockstore
// interface required by cbornode.NewCborStore. CIDs are verified on read.
type bgBlockstore struct{ bg hamt.BlockGetter }

func (b *bgBlockstore) Get(ctx context.Context, c cid.Cid) (blockformat.Block, error) {
	raw, err := b.bg.Get(ctx, c)
	if err != nil {
		return nil, err
	}
	if err := hamt.VerifyBlockCID(c, raw); err != nil {
		return nil, err
	}
	return blockformat.NewBlockWithCid(raw, c)
}

func (b *bgBlockstore) Put(ctx context.Context, blk blockformat.Block) error {
	return fmt.Errorf("lantern: read-only state store; Put not supported")
}
