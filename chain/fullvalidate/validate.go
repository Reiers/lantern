package fullvalidate

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"

	"github.com/Reiers/lantern/chain/beacon"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/crypto/sigs"
)

// StateView is the read surface a Full node exposes over its resident,
// F3-anchored, CID-verified state. It is deliberately small: the pipeline only
// needs the worker key and the power split to validate a block's consensus.
//
// Both methods take the block's Miner address and are resolved against the
// PARENT tipset's state (the state the block was produced on top of).
type StateView interface {
	// WorkerKey resolves miner -> its current worker's pubkey address
	// (BLS/secp), i.e. StateMinerInfo(miner).Worker then StateAccountKey.
	WorkerKey(ctx context.Context, miner address.Address) (address.Address, error)

	// MinerQAPower returns (thisMinerQAPower, networkTotalQAPower) so the
	// win-count can be recomputed. Matches StateMinerPower semantics.
	MinerQAPower(ctx context.Context, miner address.Address) (minerPow, totalPow abi.StoragePower, err error)
}

// Result reports which checks ran. WinningPoStVerified / StateReExecuted stay
// false until #88 / #89 land; a caller can see exactly how much was trustlessly
// verified versus F3-trusted.
type Result struct {
	SignatureOK         bool
	ElectionVRFOK       bool
	TicketVRFOK         bool
	EligibilityOK       bool
	WinCountOK          bool
	WinningPoStVerified bool // #88: pure-Go SNARK verify (not yet)
	StateReExecuted     bool // #89: pure-Go FVM (not yet)
}

// ValidateBlockConsensus runs the pure-Go consensus checks a Full node can do
// with resident state. It assumes chain/header.ValidateHeader already passed
// (structural + beacon + parent linkage). `prevBeacon` is the latest beacon
// entry from the parent epoch, used when the block carries no entries of its
// own. `sv` reads the PARENT state.
//
// It does NOT run WinningPoSt SNARK verify or FVM re-execution; those remain
// F3-trusted until #88/#89.
func ValidateBlockConsensus(
	ctx context.Context,
	bh *types.BlockHeader,
	prevBeacon *types.BeaconEntry,
	sv StateView,
) (Result, error) {
	var res Result
	if bh == nil {
		return res, errors.New("fullvalidate: nil block header")
	}
	if sv == nil {
		return res, errors.New("fullvalidate: nil state view")
	}
	if bh.ElectionProof == nil {
		return res, errors.New("fullvalidate: nil election proof")
	}
	if bh.Ticket == nil {
		return res, errors.New("fullvalidate: nil ticket")
	}

	// Worker key from parent state (pubkey-typed, ready for sigs.Verify).
	waddr, err := sv.WorkerKey(ctx, bh.Miner)
	if err != nil {
		return res, fmt.Errorf("fullvalidate: resolve worker key: %w", err)
	}

	// (1) Block signature over the worker key.
	if err := sigs.CheckBlockSignature(ctx, bh, waddr); err != nil {
		return res, fmt.Errorf("fullvalidate: block signature: %w", err)
	}
	res.SignatureOK = true

	// Randomness base: the beacon entry to draw from is the block's last
	// entry if present, else the parent-epoch beacon. Matches Lotus.
	rBeaconData, err := randBeaconData(bh, prevBeacon)
	if err != nil {
		return res, err
	}

	// Miner address CBOR is the entropy for both VRF draws.
	minerEntropy, err := minerCBOR(bh.Miner)
	if err != nil {
		return res, err
	}

	// (2) Election-proof VRF.
	evrfBase, err := beacon.DrawRandomnessFromBase(
		rBeaconData, gscrypto.DomainSeparationTag_ElectionProofProduction, bh.Height, minerEntropy)
	if err != nil {
		return res, fmt.Errorf("fullvalidate: draw election randomness: %w", err)
	}
	if err := verifyVRF(waddr, evrfBase, bh.ElectionProof.VRFProof); err != nil {
		return res, fmt.Errorf("fullvalidate: election VRF: %w", err)
	}
	res.ElectionVRFOK = true

	// (3) Ticket VRF (same worker key, ticket domain).
	tvrfBase, err := beacon.DrawRandomnessFromBase(
		rBeaconData, gscrypto.DomainSeparationTag_TicketProduction, bh.Height, minerEntropy)
	if err != nil {
		return res, fmt.Errorf("fullvalidate: draw ticket randomness: %w", err)
	}
	if err := verifyVRF(waddr, tvrfBase, bh.Ticket.VRFProof); err != nil {
		return res, fmt.Errorf("fullvalidate: ticket VRF: %w", err)
	}
	res.TicketVRFOK = true

	// (4) Win-count sanity + recomputation against parent power.
	if bh.ElectionProof.WinCount < 1 {
		return res, errors.New("fullvalidate: block does not claim to be a winner")
	}
	minerPow, totalPow, err := sv.MinerQAPower(ctx, bh.Miner)
	if err != nil {
		return res, fmt.Errorf("fullvalidate: miner power: %w", err)
	}
	want := bh.ElectionProof.ComputeWinCount(types.BigInt{Int: minerPow.Int}, types.BigInt{Int: totalPow.Int})
	if bh.ElectionProof.WinCount != want {
		return res, fmt.Errorf("fullvalidate: wrong win count: claims %d, computed %d",
			bh.ElectionProof.WinCount, want)
	}
	res.WinCountOK = true

	// WinningPoStVerified / StateReExecuted stay false: F3-trusted until #88/#89.
	return res, nil
}

// verifyVRF checks a BLS VRF proof against the worker key. A VRF proof in
// Filecoin is a BLS signature over the drawn randomness base; sigs.Verify
// dispatches to the registered BLS verifier (pure-Go gnark-crypto).
func verifyVRF(worker address.Address, base, vrfProof []byte) error {
	sig := &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: vrfProof}
	return sigs.Verify(sig, worker, base)
}

func randBeaconData(bh *types.BlockHeader, prevBeacon *types.BeaconEntry) ([]byte, error) {
	if len(bh.BeaconEntries) != 0 {
		return bh.BeaconEntries[len(bh.BeaconEntries)-1].Data, nil
	}
	if prevBeacon == nil {
		return nil, errors.New("fullvalidate: no beacon entry available (block carries none and no prev)")
	}
	return prevBeacon.Data, nil
}

func minerCBOR(miner address.Address) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := miner.MarshalCBOR(buf); err != nil {
		return nil, fmt.Errorf("fullvalidate: marshal miner addr: %w", err)
	}
	return buf.Bytes(), nil
}
