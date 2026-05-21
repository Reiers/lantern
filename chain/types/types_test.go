// Test for the lifted chain/types subset. Not copied from Lotus.

package types

import (
	"bytes"
	"testing"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/stretchr/testify/require"
)

// TestBigIntRoundTrip exercises the lifted bigint.go to ensure cbor codec
// links and the lantern/build constants resolve.
func TestBigIntRoundTrip(t *testing.T) {
	v := NewInt(123456789)
	if v.Int.Sign() <= 0 {
		t.Fatalf("expected positive: %v", v)
	}

	// FromFil should equal v * FilecoinPrecision.
	fil := FromFil(2)
	expected := big.Mul(big.NewInt(2), big.NewInt(1_000_000_000_000_000_000))
	if fil.String() != expected.String() {
		t.Fatalf("FromFil(2) = %s; want %s", fil.String(), expected.String())
	}
}

// TestBlockHeaderCBORRoundTrip ensures the lifted CBOR codec for BlockHeader
// is wired correctly. We construct a minimal valid header, marshal, unmarshal,
// and compare.
func TestBlockHeaderCBORRoundTrip(t *testing.T) {
	bh := &BlockHeader{
		Miner:         testAddr(t),
		Ticket:        &Ticket{VRFProof: []byte("ticket-vrf")},
		ElectionProof: &ElectionProof{WinCount: 1, VRFProof: []byte("election-vrf")},
		BeaconEntries: []BeaconEntry{
			{Round: 12345, Data: []byte("beacon-data")},
		},
		WinPoStProof:          nil,
		Parents:               nil,
		ParentWeight:          NewInt(100),
		Height:                abi.ChainEpoch(42),
		ParentStateRoot:       testCID(t),
		ParentMessageReceipts: testCID(t),
		Messages:              testCID(t),
		BLSAggregate:          nil,
		Timestamp:             1_700_000_000,
		BlockSig:              nil,
		ForkSignaling:         0,
		ParentBaseFee:         NewInt(100),
	}

	var buf bytes.Buffer
	require.NoError(t, bh.MarshalCBOR(&buf))

	var got BlockHeader
	require.NoError(t, got.UnmarshalCBOR(bytes.NewReader(buf.Bytes())))

	require.Equal(t, bh.Height, got.Height)
	require.Equal(t, bh.Timestamp, got.Timestamp)
	require.Equal(t, bh.BeaconEntries[0].Round, got.BeaconEntries[0].Round)
	require.True(t, bytes.Equal(bh.BeaconEntries[0].Data, got.BeaconEntries[0].Data))
}
