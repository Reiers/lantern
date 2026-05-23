// FullNode is the Lotus-compatible JSON-RPC interface Lantern exposes.
// Methods correspond 1:1 with CURIO-RPC-SURFACE.md. Anything not yet
// implemented in V1 still appears here (returning ErrNotImplemented) so
// Curio's typed client can bind without compile errors.
//
// Permissions follow Lotus convention: `read` < `write` < `sign` < `admin`.

package api

import (
	"context"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-jsonrpc/auth"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	verifreg "github.com/filecoin-project/go-state-types/builtin/v9/verifreg"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
)

// Permissions match Lotus.
const (
	PermRead  auth.Permission = "read"
	PermWrite auth.Permission = "write"
	PermSign  auth.Permission = "sign"
	PermAdmin auth.Permission = "admin"
)

// AllPerms is the canonical permission list for token issuance.
var AllPerms = []auth.Permission{PermRead, PermWrite, PermSign, PermAdmin}

// DefaultPerms is the permission set assumed for unauthenticated requests
// (none — every method requires at least read).
var DefaultPerms = []auth.Permission{PermRead}

// FullNode is the interface name go-jsonrpc binds under namespace
// `Filecoin`. We use a Permission-tagged proxy struct (FullNodeStruct,
// below) so `auth.PermissionedProxy` can enforce method-level perms.
type FullNode interface {
	// --- Node admin (N) ---
	AuthVerify(ctx context.Context, token string) ([]auth.Permission, error)
	AuthNew(ctx context.Context, perms []auth.Permission) ([]byte, error)
	Version(ctx context.Context) (Version, error)
	Shutdown(ctx context.Context) error
	Session(ctx context.Context) (string, error)

	// --- Chain reads (R) ---
	ChainHead(ctx context.Context) (*types.TipSet, error)
	ChainGetTipSet(ctx context.Context, key types.TipSetKey) (*types.TipSet, error)
	ChainGetTipSetByHeight(ctx context.Context, h abi.ChainEpoch, key types.TipSetKey) (*types.TipSet, error)
	ChainGetTipSetAfterHeight(ctx context.Context, h abi.ChainEpoch, key types.TipSetKey) (*types.TipSet, error)
	ChainGetBlock(ctx context.Context, c cid.Cid) (*types.BlockHeader, error)
	ChainGetMessage(ctx context.Context, c cid.Cid) (*types.Message, error)
	ChainGetMessagesInTipset(ctx context.Context, key types.TipSetKey) ([]ApiMsg, error)
	ChainReadObj(ctx context.Context, c cid.Cid) ([]byte, error)
	ChainHasObj(ctx context.Context, c cid.Cid) (bool, error)
	ChainPutObj(ctx context.Context, blk []byte) (cid.Cid, error)
	ChainTipSetWeight(ctx context.Context, key types.TipSetKey) (big.Int, error)
	ChainNotify(ctx context.Context) (<-chan []HeadChange, error)

	// --- State (R) ---
	StateGetActor(ctx context.Context, a address.Address, key types.TipSetKey) (*types.Actor, error)
	StateLookupID(ctx context.Context, a address.Address, key types.TipSetKey) (address.Address, error)
	StateAccountKey(ctx context.Context, a address.Address, key types.TipSetKey) (address.Address, error)
	StateNetworkVersion(ctx context.Context, key types.TipSetKey) (network.Version, error)
	StateNetworkName(ctx context.Context) (string, error)
	StateReadState(ctx context.Context, a address.Address, key types.TipSetKey) (*ActorState, error)
	StateGetRandomnessFromBeacon(ctx context.Context, pers gscrypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte, key types.TipSetKey) (abi.Randomness, error)
	StateGetRandomnessFromTickets(ctx context.Context, pers gscrypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte, key types.TipSetKey) (abi.Randomness, error)
	StateGetBeaconEntry(ctx context.Context, epoch abi.ChainEpoch) (*types.BeaconEntry, error)

	// Miner-specific reads
	StateMinerInfo(ctx context.Context, m address.Address, key types.TipSetKey) (MinerInfo, error)
	StateMinerPower(ctx context.Context, m address.Address, key types.TipSetKey) (*MinerPower, error)
	StateMinerSectors(ctx context.Context, m address.Address, sectors *bitfield.BitField, key types.TipSetKey) ([]*SectorOnChainInfo, error)
	StateMinerActiveSectors(ctx context.Context, m address.Address, key types.TipSetKey) ([]*SectorOnChainInfo, error)
	StateMinerProvingDeadline(ctx context.Context, m address.Address, key types.TipSetKey) (*dline.Info, error)
	StateMinerDeadlines(ctx context.Context, m address.Address, key types.TipSetKey) ([]MinerDeadline, error)
	StateMinerPartitions(ctx context.Context, m address.Address, dlIdx uint64, key types.TipSetKey) ([]Partition, error)
	StateMinerAvailableBalance(ctx context.Context, m address.Address, key types.TipSetKey) (big.Int, error)
	StateMinerAllocated(ctx context.Context, m address.Address, key types.TipSetKey) (*bitfield.BitField, error)
	StateMinerFaults(ctx context.Context, m address.Address, key types.TipSetKey) (bitfield.BitField, error)
	StateMinerRecoveries(ctx context.Context, m address.Address, key types.TipSetKey) (bitfield.BitField, error)
	StateMinerSectorCount(ctx context.Context, m address.Address, key types.TipSetKey) (MinerSectors, error)
	StateMinerPreCommitDepositForPower(ctx context.Context, m address.Address, pci SectorPreCommitInfo, key types.TipSetKey) (big.Int, error)
	StateMinerInitialPledgeForSector(ctx context.Context, sectorDuration abi.ChainEpoch, sectorSize abi.SectorSize, verifiedSize uint64, key types.TipSetKey) (big.Int, error)
	StateMinerInitialPledgeCollateral(ctx context.Context, m address.Address, pci SectorPreCommitInfo, key types.TipSetKey) (big.Int, error)
	StateMinerSectorAllocated(ctx context.Context, m address.Address, s abi.SectorNumber, key types.TipSetKey) (bool, error)
	StateSectorPreCommitInfo(ctx context.Context, m address.Address, sector abi.SectorNumber, key types.TipSetKey) (*SectorPreCommitOnChainInfo, error)
	StateSectorGetInfo(ctx context.Context, m address.Address, sector abi.SectorNumber, key types.TipSetKey) (*SectorOnChainInfo, error)
	StateSectorPartition(ctx context.Context, m address.Address, sector abi.SectorNumber, key types.TipSetKey) (*SectorLocation, error)
	StateMinerCreationDeposit(ctx context.Context, key types.TipSetKey) (big.Int, error)

	// Market & verified-registry reads
	StateMarketBalance(ctx context.Context, a address.Address, key types.TipSetKey) (MarketBalance, error)
	StateMarketStorageDeal(ctx context.Context, dealID abi.DealID, key types.TipSetKey) (*MarketDeal, error)
	StateGetAllocation(ctx context.Context, client address.Address, allocID verifreg.AllocationId, key types.TipSetKey) (*verifreg.Allocation, error)
	StateGetAllocationIdForPendingDeal(ctx context.Context, dealID abi.DealID, key types.TipSetKey) (verifreg.AllocationId, error)
	StateGetAllocationForPendingDeal(ctx context.Context, dealID abi.DealID, key types.TipSetKey) (*verifreg.Allocation, error)
	StateVerifiedClientStatus(ctx context.Context, a address.Address, key types.TipSetKey) (*big.Int, error)
	StateDealProviderCollateralBounds(ctx context.Context, size abi.PaddedPieceSize, verified bool, key types.TipSetKey) (DealCollateralBounds, error)
	StateListMessages(ctx context.Context, match *MessageMatch, key types.TipSetKey, fromEpoch abi.ChainEpoch) ([]cid.Cid, error)
	StateListMiners(ctx context.Context, key types.TipSetKey) ([]address.Address, error)
	StateCirculatingSupply(ctx context.Context, key types.TipSetKey) (abi.TokenAmount, error)
	StateVMCirculatingSupplyInternal(ctx context.Context, key types.TipSetKey) (CirculatingSupply, error)

	// Wait / search
	StateWaitMsg(ctx context.Context, c cid.Cid, confidence uint64, limit abi.ChainEpoch, allowReplaced bool) (*MsgLookup, error)
	StateSearchMsg(ctx context.Context, from types.TipSetKey, c cid.Cid, limit abi.ChainEpoch, allowReplaced bool) (*MsgLookup, error)

	// Compute (read-only VM eval) — Tier 4 stubs for now.
	StateCall(ctx context.Context, msg *types.Message, key types.TipSetKey) (*InvocResult, error)

	// Gas estimation
	GasEstimateMessageGas(ctx context.Context, msg *types.Message, spec *MessageSendSpec, key types.TipSetKey) (*types.Message, error)
	GasEstimateFeeCap(ctx context.Context, msg *types.Message, maxqueueblks int64, key types.TipSetKey) (abi.TokenAmount, error)
	GasEstimateGasPremium(ctx context.Context, nblocksincl uint64, sender address.Address, gaslimit int64, key types.TipSetKey) (abi.TokenAmount, error)

	// Wallet (W)
	WalletNew(ctx context.Context, kt KeyType) (address.Address, error)
	WalletList(ctx context.Context) ([]address.Address, error)
	WalletHas(ctx context.Context, a address.Address) (bool, error)
	WalletDelete(ctx context.Context, a address.Address) error
	WalletExport(ctx context.Context, a address.Address) (*KeyInfo, error)
	WalletImport(ctx context.Context, ki *KeyInfo) (address.Address, error)
	WalletSetDefault(ctx context.Context, a address.Address) error
	WalletDefaultAddress(ctx context.Context) (address.Address, error)
	WalletSign(ctx context.Context, a address.Address, msg []byte) (*gscrypto.Signature, error)
	WalletSignMessage(ctx context.Context, a address.Address, msg *types.Message) (*types.SignedMessage, error)
	WalletBalance(ctx context.Context, a address.Address) (big.Int, error)

	// Mpool (G+W+C)
	MpoolPush(ctx context.Context, sm *types.SignedMessage) (cid.Cid, error)
	MpoolPushMessage(ctx context.Context, msg *types.Message, spec *MessageSendSpec) (*types.SignedMessage, error)
	MpoolGetNonce(ctx context.Context, a address.Address) (uint64, error)
	MpoolPending(ctx context.Context, tsk []types.TipSetKey) ([]*types.SignedMessage, error)

	// SP block production (SP) — Tier 4 stubs.
	MinerGetBaseInfo(ctx context.Context, m address.Address, epoch abi.ChainEpoch, key types.TipSetKey) (*MiningBaseInfo, error)
	MinerCreateBlock(ctx context.Context, template *BlockTemplate) (*types.BlockMsg, error)
	MpoolSelect(ctx context.Context, key types.TipSetKey, ticketQuality float64) ([]*types.SignedMessage, error)
	SyncSubmitBlock(ctx context.Context, blk *types.BlockMsg) error

	// Market convenience (composes Mpool/Wallet) — Tier 3 stub.
	MarketAddBalance(ctx context.Context, wallet, addr address.Address, amt big.Int) (cid.Cid, error)

	// Payment channels (Tier 3) — Phase 7 read-only + sign.
	PaychGet(ctx context.Context, from, to address.Address, amt big.Int, opts PaychGetOpts) (*ChannelInfo, error)
	PaychAvailableFunds(ctx context.Context, ch address.Address) (*ChannelAvailableFunds, error)
	PaychVoucherCreate(ctx context.Context, ch address.Address, amt big.Int, lane uint64) (*VoucherCreateResult, error)
	PaychVoucherCheckValid(ctx context.Context, ch address.Address, sv *PaychSignedVoucher) error
	PaychVoucherCheckSpendable(ctx context.Context, ch address.Address, sv *PaychSignedVoucher, secret []byte, proof []byte) (bool, error)
	PaychVoucherList(ctx context.Context, ch address.Address) ([]*PaychSignedVoucher, error)

	// Net + Eth health probes (Curio polls these for status display).
	// V1 returns stubs; Phase 10 wires them to the live libp2p host.
	NetPeers(ctx context.Context) ([]struct {
		ID    string
		Addrs []string
	}, error)
	NetAgentVersion(ctx context.Context, peerID string) (string, error)
	NetConnectedness(ctx context.Context, peerID string) (int, error)
	NetListening(ctx context.Context) (bool, error)
	NetBandwidthStats(ctx context.Context) (NetBandwidthStats, error)
	NetAutoNatStatus(ctx context.Context) (NatInfo, error)
	EthBlockNumber(ctx context.Context) (string, error)
	EthChainId(ctx context.Context) (string, error)
	EthAccounts(ctx context.Context) ([]string, error)
	EthMaxPriorityFeePerGas(ctx context.Context) (string, error)
	EthGasPrice(ctx context.Context) (string, error)
	EthSyncing(ctx context.Context) (any, error)
	EthGetBalance(ctx context.Context, address string, blockParam any) (string, error)
	EthGetBlockByNumber(ctx context.Context, blockParam string, fullTx bool) (any, error)
	EthCall(ctx context.Context, callObj any, blockParam any) (string, error)
	EthEstimateGas(ctx context.Context, callObj any) (string, error)
	EthSendRawTransaction(ctx context.Context, signedTxHex string) (string, error)
}

// NetBandwidthStats mirrors libp2p/core/metrics.Stats. Re-declared here so
// the API interface doesn't pull libp2p into every consumer's import graph.
type NetBandwidthStats struct {
	TotalIn  int64
	TotalOut int64
	RateIn   float64
	RateOut  float64
}

// NatInfo mirrors lotus api.NatInfo. Reachability is the integer value of
// libp2p network.Reachability (0=Unknown, 1=Public, 2=Private).
type NatInfo struct {
	Reachability int
	PublicAddrs  []string
}

// KeyType is wallet.KeyType, re-exported so callers don't pull wallet into
// their import graph when they only need the RPC interface.
type KeyType string

// KeyInfo is the on-wire shape for WalletExport/WalletImport (matches
// Lotus types.KeyInfo).
type KeyInfo struct {
	Type       KeyType
	PrivateKey []byte
}

// Supporting struct types (Lotus-compat shapes that aren't already in
// go-state-types).

// ApiMsg mirrors api.Message — a (cid, Message) pair returned by
// ChainGetMessagesInTipset.
type ApiMsg struct {
	Cid     cid.Cid
	Message *types.Message
}

// ActorState mirrors api.ActorState.
type ActorState struct {
	Balance big.Int
	Code    cid.Cid
	State   interface{}
}

// MinerDeadline mirrors api.MinerDeadline.
type MinerDeadline struct {
	PostSubmissions      bitfield.BitField
	DisputableProofCount uint64
}

// Partition mirrors api.Partition.
type Partition struct {
	AllSectors        bitfield.BitField
	FaultySectors     bitfield.BitField
	RecoveringSectors bitfield.BitField
	LiveSectors       bitfield.BitField
	ActiveSectors     bitfield.BitField
}

// MinerSectors mirrors api.MinerSectors.
type MinerSectors struct {
	Live   uint64
	Active uint64
	Faulty uint64
}

// SectorPreCommitInfo mirrors miner.SectorPreCommitInfo (parameter type).
type SectorPreCommitInfo struct {
	SealProof     abi.RegisteredSealProof
	SectorNumber  abi.SectorNumber
	SealedCID     cid.Cid
	SealRandEpoch abi.ChainEpoch
	DealIDs       []abi.DealID
	Expiration    abi.ChainEpoch
	UnsealedCid   *cid.Cid
}

// DealCollateralBounds mirrors api.DealCollateralBounds.
type DealCollateralBounds struct {
	Min abi.TokenAmount
	Max abi.TokenAmount
}

// MessageMatch mirrors api.MessageMatch (selector for StateListMessages).
type MessageMatch struct {
	To   address.Address
	From address.Address
}

// CirculatingSupply mirrors api.CirculatingSupply.
type CirculatingSupply struct {
	FilVested           abi.TokenAmount
	FilMined            abi.TokenAmount
	FilBurnt            abi.TokenAmount
	FilLocked           abi.TokenAmount
	FilCirculating      abi.TokenAmount
	FilReserveDisbursed abi.TokenAmount
}

// InvocResult mirrors api.InvocResult (return value of StateCall).
// Lantern V1 always returns ErrNotImplemented for StateCall, so we keep
// the shape minimal but JSON-compatible.
type InvocResult struct {
	MsgCid         cid.Cid
	Msg            *types.Message
	MsgRct         *types.MessageReceipt
	GasCost        MessageGasCost
	ExecutionTrace ExecutionTrace
	Error          string
	Duration       int64
}

// MessageGasCost mirrors api.MessageGasCost.
type MessageGasCost struct {
	Message            cid.Cid
	GasUsed            abi.TokenAmount
	BaseFeeBurn        abi.TokenAmount
	OverEstimationBurn abi.TokenAmount
	MinerPenalty       abi.TokenAmount
	MinerTip           abi.TokenAmount
	Refund             abi.TokenAmount
	TotalCost          abi.TokenAmount
}

// ExecutionTrace is the trace from StateCall — stub for now.
type ExecutionTrace struct {
	Msg        *types.Message
	MsgRct     *types.MessageReceipt
	Error      string
	Duration   int64
	GasCharges []interface{}
	Subcalls   []ExecutionTrace
}

// MiningBaseInfo mirrors api.MiningBaseInfo (block production).
type MiningBaseInfo struct {
	MinerPower        abi.StoragePower
	NetworkPower      abi.StoragePower
	Sectors           []SectorInfo
	WorkerKey         address.Address
	SectorSize        abi.SectorSize
	PrevBeaconEntry   types.BeaconEntry
	BeaconEntries     []types.BeaconEntry
	EligibleForMining bool
}

// SectorInfo is a (RegisteredPoStProof, SectorNumber, SealedCID) tuple.
type SectorInfo struct {
	SealProof    abi.RegisteredSealProof
	SectorNumber abi.SectorNumber
	SealedCID    cid.Cid
}

// BlockTemplate mirrors api.BlockTemplate.
type BlockTemplate struct {
	Miner            address.Address
	Parents          types.TipSetKey
	Ticket           *types.Ticket
	Eproof           *types.ElectionProof
	BeaconValues     []types.BeaconEntry
	Messages         []*types.SignedMessage
	Epoch            abi.ChainEpoch
	Timestamp        uint64
	WinningPoStProof []interface{}
}

// --- Paych types (Lotus-compatible subset) ---

// PaychGetOpts mirrors paychapi.PaychGetOpts.
type PaychGetOpts struct {
	OffChain bool
}

// ChannelInfo mirrors paychapi.ChannelInfo.
type ChannelInfo struct {
	Channel      address.Address
	WaitSentinel cid.Cid
}

// ChannelAvailableFunds mirrors paychapi.ChannelAvailableFunds.
type ChannelAvailableFunds struct {
	Channel             *address.Address
	From                address.Address
	To                  address.Address
	ConfirmedAmt        big.Int
	PendingAmt          big.Int
	NonReservedAmt      big.Int
	PendingAvailableAmt big.Int
	PendingWaitSentinel *cid.Cid
	QueuedAmt           big.Int
	VoucherReedeemedAmt big.Int
}

// VoucherCreateResult mirrors paychapi.VoucherCreateResult.
type VoucherCreateResult struct {
	Voucher   *PaychSignedVoucher
	Shortfall big.Int
}

// PaychSignedVoucher is a Lantern-side mirror of paych.SignedVoucher
// from go-state-types/builtin/v18/paych. Re-declaring it here keeps the
// api package free of go-state-types/v18 imports in handler signatures.
type PaychSignedVoucher struct {
	ChannelAddr     address.Address
	TimeLockMin     abi.ChainEpoch
	TimeLockMax     abi.ChainEpoch
	SecretHash      []byte
	Extra           *PaychModVerifyParams
	Lane            uint64
	Nonce           uint64
	Amount          big.Int
	MinSettleHeight abi.ChainEpoch
	Merges          []PaychMerge
	Signature       *PaychSignature
}

// PaychModVerifyParams mirrors paych.ModVerifyParams.
type PaychModVerifyParams struct {
	Actor  address.Address
	Method abi.MethodNum
	Data   []byte
}

// PaychMerge mirrors paych.Merge.
type PaychMerge struct {
	Lane  uint64
	Nonce uint64
}

// PaychSignature is a thin alias for crypto.Signature; declared here so
// callers don't have to import go-state-types/crypto.
type PaychSignature struct {
	Type uint8
	Data []byte
}
