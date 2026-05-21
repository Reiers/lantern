// API result types — Lotus-compatible shapes for the Curio surface.
//
// Where Lotus types live in `chain/types`, this file simply aliases. Where
// Lotus invents an "API view" of a state object (e.g., MinerInfo with the
// PeerId decoded), we define a matching struct here.
//
// All JSON tags match Lotus exactly so an unmodified Lotus client can
// decode our responses.

package api

import (
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin/v18/miner"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
)

// Re-exports for ergonomic handler signatures.
type (
	TipSet        = types.TipSet
	TipSetKey     = types.TipSetKey
	BlockHeader   = types.BlockHeader
	Message       = types.Message
	SignedMessage = types.SignedMessage
	Actor         = types.Actor
)

// Version is the response shape of Filecoin.Version.
//
// Mirrors api.APIVersion in lotus/api/api_common.go.
type Version struct {
	Version    string
	APIVersion uint32
	BlockDelay uint64
}

// MsgLookup is the result of Filecoin.StateWaitMsg / StateSearchMsg.
type MsgLookup struct {
	Message   cid.Cid                  // the message CID we searched for
	Receipt   types.MessageReceipt     // execution receipt
	ReturnDec interface{}              // decoded return value (nil for Lantern V1)
	TipSet    types.TipSetKey          // tipset the message was included in
	Height    abi.ChainEpoch
}

// MarketBalance is the result of Filecoin.StateMarketBalance.
type MarketBalance struct {
	Escrow big.Int
	Locked big.Int
}

// MinerInfo is the API view of the StorageMinerActor's MinerInfo struct.
//
// Mirrors api.MinerInfo from lotus/api/types.go. JSON tags match Lotus.
type MinerInfo struct {
	Owner                      address.Address
	Worker                     address.Address
	NewWorker                  address.Address
	ControlAddresses           []address.Address
	WorkerChangeEpoch          abi.ChainEpoch
	PeerId                     *string `json:"PeerId"` // base58
	Multiaddrs                 [][]byte
	WindowPoStProofType        abi.RegisteredPoStProof
	SectorSize                 abi.SectorSize
	WindowPoStPartitionSectors uint64
	ConsensusFaultElapsed      abi.ChainEpoch
	Beneficiary                address.Address
	BeneficiaryTerm            *miner.BeneficiaryTerm
	PendingBeneficiaryTerm     *miner.PendingBeneficiaryChange
}

// MinerPower mirrors api.MinerPower.
type MinerPower struct {
	MinerPower  Claim
	TotalPower  Claim
	HasMinPower bool
}

// Claim mirrors power.Claim (raw + quality-adjusted byte power).
type Claim struct {
	RawBytePower    abi.StoragePower
	QualityAdjPower abi.StoragePower
}

// HeadChange mirrors api.HeadChange.
//
//	{"Type": "apply"|"revert"|"current", "Val": <TipSet>}
type HeadChange struct {
	Type string
	Val  *types.TipSet
}

// MessageSendSpec mirrors api.MessageSendSpec used by MpoolPushMessage.
type MessageSendSpec struct {
	MaxFee            abi.TokenAmount
	MsgUuid           string
	MaximizeFeeCap    bool
	GasOverEstimation float64
}

// MarketDeal is the API view of a deal proposal+state pair from the
// market actor.
type MarketDeal struct {
	Proposal MarketDealProposal
	State    MarketDealState
}

// MarketDealProposal lifted-shape of market.DealProposal (with API
// quirks: stringified BigInts, etc., are handled by chain/types' JSON
// codecs upstream).
type MarketDealProposal struct {
	PieceCID             cid.Cid `json:"PieceCID"`
	PieceSize            abi.PaddedPieceSize
	VerifiedDeal         bool
	Client               address.Address
	Provider             address.Address
	Label                string
	StartEpoch           abi.ChainEpoch
	EndEpoch             abi.ChainEpoch
	StoragePricePerEpoch big.Int
	ProviderCollateral   big.Int
	ClientCollateral     big.Int
}

// MarketDealState lifted-shape of market.DealState.
type MarketDealState struct {
	SectorStartEpoch abi.ChainEpoch
	LastUpdatedEpoch abi.ChainEpoch
	SlashEpoch       abi.ChainEpoch
}

// SectorOnChainInfo mirrors miner.SectorOnChainInfo (latest network version).
type SectorOnChainInfo = miner.SectorOnChainInfo

// SectorPreCommitOnChainInfo mirrors miner.SectorPreCommitOnChainInfo.
type SectorPreCommitOnChainInfo = miner.SectorPreCommitOnChainInfo

// Deadline mirrors miner.Deadline (subset used by StateMinerProvingDeadline).
type Deadline = dline.Info

// NetworkVersionResp is a small wrapper so handler signature matches.
type NetworkVersionResp = network.Version

// SectorLocation is the result of StateSectorPartition.
type SectorLocation struct {
	Deadline  uint64
	Partition uint64
}
