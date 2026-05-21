// Randomness derivation for Lantern.
//
// The hashing formula is lifted byte-for-byte from
// github.com/filecoin-project/lotus/chain/rand/rand.go (commit master @
// 2026-05; functions `DrawRandomnessFromBase` and `DrawRandomnessFromDigest`)
// to guarantee bit-stable cross-compatibility with Lotus.
//
// See LICENSE-LOTUS-MIT and LICENSE-LOTUS-APACHE in the repo root.
//
// Lotus' rand.go pseudo-code (paraphrased):
//
//	DrawRandomnessFromDigest(digest, pers, round, entropy):
//	    h = blake2b256()
//	    write(h, int64-BE(pers))
//	    write(h, digest[:])
//	    write(h, int64-BE(round))
//	    write(h, entropy)
//	    return h.Sum()
//
//	Chain  randomness digest = blake2b256(randTs.MinTicketBlock().Ticket.VRFProof)
//	Beacon randomness digest = blake2b256(beaconEntry.Data)
//
// For nv >= 13 (network.Version13), chain randomness walks back to the
// exact tipset at `round` (or the canonical tipset for that height —
// "lookback=false"). For nv >= 14 (Filecoin nv14 / Hyperdrive), beacon
// randomness picks the beacon entry whose `round` matches
// `MaxBeaconRoundForEpoch(filecoinEpoch)`.
//
// MaxBeaconRoundForEpoch (drand quicknet on mainnet, period 3s):
//   - genesis_time = 1692803367
//   - period       = 3
//   - filecoinEpochToBeaconRound(epoch):
//       latestTs = genesis_filecoin_time + (epoch+1)*30 - 1
//       (1 epoch ahead because GenesisBlockDelaySecs of FilecoinAvg = 30s)
//       beacon_round = floor((latestTs - drand_genesis) / drand_period) + 1
//
// This package exposes the pure-Go helpers; the chain-walk glue (mapping
// `(filecoinEpoch, tipset)` → ticketTipset / beaconEntry) lives in
// rpc/handlers.

package beacon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"

	abi "github.com/filecoin-project/go-state-types/abi"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"golang.org/x/crypto/blake2b"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// DrawRandomnessFromBase produces randomness from a "base" byte slice by
// hashing the base with blake2b-256 first. Matches Lotus rand.go.
func DrawRandomnessFromBase(rbase []byte, pers gscrypto.DomainSeparationTag, round abi.ChainEpoch, entropy []byte) ([]byte, error) {
	digest := blake2b.Sum256(rbase)
	return DrawRandomnessFromDigest(digest, pers, round, entropy)
}

// DrawRandomnessFromDigest mirrors lotus/chain/rand.go's same-named
// function. Matches byte-for-byte.
func DrawRandomnessFromDigest(digest [32]byte, pers gscrypto.DomainSeparationTag, round abi.ChainEpoch, entropy []byte) ([]byte, error) {
	h, err := blake2b.New256(nil)
	if err != nil {
		return nil, fmt.Errorf("blake2b: %w", err)
	}
	if err := writeBE(h, int64(pers)); err != nil {
		return nil, err
	}
	if _, err := h.Write(digest[:]); err != nil {
		return nil, fmt.Errorf("hash digest: %w", err)
	}
	if err := writeBE(h, int64(round)); err != nil {
		return nil, err
	}
	if _, err := h.Write(entropy); err != nil {
		return nil, fmt.Errorf("hash entropy: %w", err)
	}
	return h.Sum(nil), nil
}

func writeBE(h hash.Hash, v int64) error {
	return binary.Write(h, binary.BigEndian, v)
}

// TicketDigest returns blake2b-256 of the canonical (min-ticket) block's
// Ticket.VRFProof, ready to feed into DrawRandomnessFromDigest. Returns an
// error if ts has no blocks or its min-ticket block has no Ticket.
func TicketDigest(ts *ltypes.TipSet) ([32]byte, error) {
	var zero [32]byte
	if ts == nil || len(ts.Blocks()) == 0 {
		return zero, errors.New("ticket digest: empty tipset")
	}
	b := ts.MinTicketBlock()
	if b == nil || b.Ticket == nil {
		return zero, errors.New("ticket digest: min-ticket block has no ticket")
	}
	return blake2b.Sum256(b.Ticket.VRFProof), nil
}

// BeaconDigest returns blake2b-256 of the beacon entry's Data bytes.
func BeaconDigest(entry ltypes.BeaconEntry) [32]byte {
	return blake2b.Sum256(entry.Data)
}

// DrawChainRandomness composes TicketDigest + DrawRandomnessFromDigest.
func DrawChainRandomness(ts *ltypes.TipSet, pers gscrypto.DomainSeparationTag, round abi.ChainEpoch, entropy []byte) ([]byte, error) {
	d, err := TicketDigest(ts)
	if err != nil {
		return nil, err
	}
	return DrawRandomnessFromDigest(d, pers, round, entropy)
}

// DrawBeaconRandomness composes BeaconDigest + DrawRandomnessFromDigest.
func DrawBeaconRandomness(entry ltypes.BeaconEntry, pers gscrypto.DomainSeparationTag, round abi.ChainEpoch, entropy []byte) ([]byte, error) {
	d := BeaconDigest(entry)
	return DrawRandomnessFromDigest(d, pers, round, entropy)
}

// ----- drand round selection -----

// QuicknetParams are the mainnet drand-quicknet timing parameters used to
// map a Filecoin epoch to a drand round. Source: build/buildconstants drand.go
// (chain hash 52db9ba7...quicknet).
type QuicknetParams struct {
	DrandGenesisTime int64 // 1692803367
	DrandPeriodSecs  int64 // 3
	// FilecoinGenesisTime is the unix timestamp of Filecoin block 0.
	// Source: mainnet build params (1598306400 for mainnet).
	FilecoinGenesisTime int64
	// BlockDelaySecs is the Filecoin block delay (30s for mainnet).
	BlockDelaySecs int64
}

// MainnetQuicknetParams returns the parameters for Filecoin mainnet using
// drand-quicknet. Values match Lotus build/buildconstants/* @ commit
// a0ecb8687f1c60d5e66040b6de364dbc9cc4d253.
func MainnetQuicknetParams() QuicknetParams {
	return QuicknetParams{
		DrandGenesisTime:    1692803367,
		DrandPeriodSecs:     3,
		FilecoinGenesisTime: 1598306400,
		BlockDelaySecs:      30,
	}
}

// MaxBeaconRoundForEpoch returns the highest drand round whose seal time
// happens no later than the start of the given Filecoin epoch.
//
// Lotus implementation (chain/beacon/drand/drand.go @ master 2026-05):
//
//	latestTs := (uint64(filEpoch) * filRoundTime) + filGenTime - filRoundTime
//
//	maxBeaconRoundV2 (nv >= 16 / current mainnet):
//	    if latestTs < drandGenTime { return 1 }
//	    return (latestTs - drandGenTime) / period + 1
//
// We match maxBeaconRoundV2 byte-for-byte; mainnet has been past nv16 for
// years, so we don't bother with the V1 branch.
func (p QuicknetParams) MaxBeaconRoundForEpoch(filecoinEpoch abi.ChainEpoch) uint64 {
	if filecoinEpoch < 0 {
		return 1
	}
	latestTs := uint64(filecoinEpoch)*uint64(p.BlockDelaySecs) + uint64(p.FilecoinGenesisTime) - uint64(p.BlockDelaySecs)
	if latestTs < uint64(p.DrandGenesisTime) {
		return 1
	}
	fromGenesis := latestTs - uint64(p.DrandGenesisTime)
	return fromGenesis/uint64(p.DrandPeriodSecs) + 1
}
