package beacon_test

import (
	"encoding/hex"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/blake2b"

	"github.com/Reiers/lantern/chain/beacon"
)

// TestDrawRandomnessFromDigestKAT is a known-answer test where we compute
// the same hash by hand using `blake2b` and `binary.BigEndian`, then check
// our wrapper produces the same bytes. This guards against accidental
// formula drift.
func TestDrawRandomnessFromDigestKAT(t *testing.T) {
	var digest [32]byte
	for i := range digest {
		digest[i] = byte(i)
	}
	pers := gscrypto.DomainSeparationTag(7) // arbitrary
	round := abi.ChainEpoch(42)
	entropy := []byte("test-entropy")

	// Expected hash, computed inline.
	want := blake2b.Sum256(makeRandPreimage(int64(pers), digest, int64(round), entropy))

	got, err := beacon.DrawRandomnessFromDigest(digest, pers, round, entropy)
	require.NoError(t, err)
	require.Equal(t, want[:], got, "draw randomness must match hand-computed preimage")
}

// TestDrawRandomnessFromBaseHashesFirst checks DrawRandomnessFromBase ==
// DrawRandomnessFromDigest(blake2b(base)).
func TestDrawRandomnessFromBaseHashesFirst(t *testing.T) {
	base := []byte("some base bytes")
	pers := gscrypto.DomainSeparationTag(2)
	round := abi.ChainEpoch(100)
	entropy := []byte{0xde, 0xad, 0xbe, 0xef}

	digest := blake2b.Sum256(base)
	a, err := beacon.DrawRandomnessFromDigest(digest, pers, round, entropy)
	require.NoError(t, err)
	b, err := beacon.DrawRandomnessFromBase(base, pers, round, entropy)
	require.NoError(t, err)
	require.Equal(t, a, b)
}

// TestMaxBeaconRoundForEpoch_Mainnet checks the drand-round formula matches
// observed mainnet values. Reference: lotus chain/beacon/drand/drand.go
// maxBeaconRoundV2 formula at master @ 2026-05.
//
// At Filecoin epoch 6035749 (a recent mainnet head observed during demo),
// the canonical tipset carried BeaconEntries with Round = 28858492.
// Formula:
//   latestTs = 6035749 * 30 + 1598306400 - 30 = 1779378840
//   fromGenesis = 1779378840 - 1692803367 = 86575473
//   round = 86575473/3 + 1 = 28858491 + 1 = 28858492
func TestMaxBeaconRoundForEpoch_Mainnet(t *testing.T) {
	p := beacon.MainnetQuicknetParams()
	got := p.MaxBeaconRoundForEpoch(6035749)
	require.Equal(t, uint64(28858492), got)

	// Sanity: epoch 0 is well before drand genesis (drand-quicknet was
	// activated long after Filecoin genesis); maxBeaconRoundV2 returns 1.
	require.Equal(t, uint64(1), p.MaxBeaconRoundForEpoch(0))
}

// makeRandPreimage builds the preimage bytes Lotus' DrawRandomnessFromDigest
// hashes. It exists solely to provide a hand-computed reference for KAT.
func makeRandPreimage(pers int64, digest [32]byte, round int64, entropy []byte) []byte {
	out := make([]byte, 0, 8+32+8+len(entropy))
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[7-i] = byte(pers >> (i * 8))
	}
	out = append(out, b[:]...)
	out = append(out, digest[:]...)
	for i := 7; i >= 0; i-- {
		b[7-i] = byte(round >> (i * 8))
	}
	out = append(out, b[:]...)
	out = append(out, entropy...)
	return out
}

// TestStableHexFixture is a stable hex fixture so anyone running the suite
// can grep for "expected_randomness=" and notice if the formula changed.
func TestStableHexFixture(t *testing.T) {
	var digest [32]byte
	for i := range digest {
		digest[i] = 0xaa
	}
	got, err := beacon.DrawRandomnessFromDigest(digest, gscrypto.DomainSeparationTag(3), abi.ChainEpoch(123456), []byte("entropy-x"))
	require.NoError(t, err)
	// Fixture: blake2b256 over (BE int64 3 || 32x0xaa || BE int64 123456 || "entropy-x").
	require.Equal(t, "35a465a6a1aeb49d6c8bc2a4e7fde015697c9cde85ebef830bacdb451fa5b291", hex.EncodeToString(got))
}
