package header_test

import (
	"testing"

	"github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/chain/header"
	ltypes "github.com/Reiers/lantern/chain/types"
)

func mkCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

func mkAddr(t *testing.T, id uint64) address.Address {
	t.Helper()
	a, err := address.NewIDAddress(id)
	require.NoError(t, err)
	return a
}

func mkBlock(t *testing.T, height abi.ChainEpoch, parents []cid.Cid, miner uint64) *ltypes.BlockHeader {
	return &ltypes.BlockHeader{
		Miner:                 mkAddr(t, miner),
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t")},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e")},
		BeaconEntries:         nil,
		Parents:               parents,
		ParentWeight:          ltypes.NewInt(0),
		Height:                height,
		ParentStateRoot:       mkCID(t, "state"),
		ParentMessageReceipts: mkCID(t, "receipts"),
		Messages:              mkCID(t, "msgs"),
		Timestamp:             1_700_000_000,
		ParentBaseFee:         ltypes.NewInt(100),
	}
}

func TestVerifyBlockHeaderCID_MatchAndMismatch(t *testing.T) {
	bh := mkBlock(t, 10, []cid.Cid{mkCID(t, "p")}, 1000)
	got := bh.Cid()
	require.NoError(t, header.VerifyBlockHeaderCID(bh, got))

	require.Error(t, header.VerifyBlockHeaderCID(bh, mkCID(t, "other")))
}

func TestValidateTipsetShape(t *testing.T) {
	parents := []cid.Cid{mkCID(t, "p")}
	b1 := mkBlock(t, 10, parents, 1001)
	b2 := mkBlock(t, 10, parents, 1002)
	ts, err := header.ValidateTipsetShape([]*ltypes.BlockHeader{b1, b2})
	require.NoError(t, err)
	require.Equal(t, abi.ChainEpoch(10), ts.Height())
}

func TestVerifyParentLinkage(t *testing.T) {
	parentB := mkBlock(t, 9, nil, 999)
	parentTS, err := ltypes.NewTipSet([]*ltypes.BlockHeader{parentB})
	require.NoError(t, err)

	child := mkBlock(t, 10, parentTS.Cids(), 1001)
	require.NoError(t, header.VerifyParentLinkage(child.Parents, parentTS))

	badChild := mkBlock(t, 10, []cid.Cid{mkCID(t, "wrong")}, 1001)
	require.Error(t, header.VerifyParentLinkage(badChild.Parents, parentTS))
}

func TestValidateHeader_HeightRule(t *testing.T) {
	parentB := mkBlock(t, 5, nil, 999)
	parentTS, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{parentB})

	// height equal to parent is rejected
	sameHeight := mkBlock(t, 5, parentTS.Cids(), 1001)
	require.Error(t, header.ValidateHeader(sameHeight, parentTS, nil, nil))

	// height greater is accepted
	higher := mkBlock(t, 6, parentTS.Cids(), 1001)
	require.NoError(t, header.ValidateHeader(higher, parentTS, nil, nil))
}

func TestValidateHeader_RequiresElectionProofPostGenesis(t *testing.T) {
	parentB := mkBlock(t, 5, nil, 999)
	parentTS, _ := ltypes.NewTipSet([]*ltypes.BlockHeader{parentB})

	bh := mkBlock(t, 6, parentTS.Cids(), 1001)
	bh.ElectionProof = nil
	require.Error(t, header.ValidateHeader(bh, parentTS, nil, nil))
}
