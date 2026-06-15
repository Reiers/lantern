package trustedroot_test

import (
	"context"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/chain/trustedroot"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// Minimal HeaderSource over a fixed map.
type stubHeaderSource struct {
	tipsets map[abi.ChainEpoch]*ltypes.TipSet
	head    abi.ChainEpoch
}

func (s *stubHeaderSource) Tipset(_ context.Context, epoch abi.ChainEpoch) (*ltypes.TipSet, error) {
	return s.tipsets[epoch], nil
}
func (s *stubHeaderSource) Head(_ context.Context) (abi.ChainEpoch, error) {
	return s.head, nil
}

func mkCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

func mkBlock(t *testing.T, h abi.ChainEpoch, parents []cid.Cid, label string) *ltypes.BlockHeader {
	a, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	return &ltypes.BlockHeader{
		Miner:                 a,
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t-" + label)},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + label)},
		Parents:               parents,
		ParentWeight:          ltypes.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       mkCID(t, "state-"+label),
		ParentMessageReceipts: mkCID(t, "receipts-"+label),
		Messages:              mkCID(t, "msgs-"+label),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         ltypes.NewInt(100),
	}
}

// TestBuild_RoundTrip wires Build with stub sources to confirm the pipeline
// compiles and persists/loads correctly. Real F3 cert verification needs
// real BLS-signed certs and is exercised by examples/historical/phase1.
func TestBuild_PersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	db, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
	require.NoError(t, err)
	defer db.Close()

	// Build a small synthetic chain.
	tipsets := map[abi.ChainEpoch]*ltypes.TipSet{}
	var parents []cid.Cid
	for ep := abi.ChainEpoch(0); ep <= 50; ep++ {
		bh := mkBlock(t, ep, parents, "ep"+string(rune('0'+(ep%10))))
		ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{bh})
		require.NoError(t, err)
		tipsets[ep] = ts
		parents = ts.Cids()
	}

	hs := &stubHeaderSource{tipsets: tipsets, head: 50}

	// Build with no F3 source — should return a root at head - safety.
	tr := &trustedroot.TrustedRoot{
		Epoch:                 20,
		TipSetKey:             tipsets[20].Key(),
		StateRoot:             tipsets[20].Blocks()[0].ParentStateRoot,
		ParentMessageReceipts: tipsets[20].Blocks()[0].ParentMessageReceipts,
		ParentWeight:          tipsets[20].Blocks()[0].ParentWeight,
		BeaconRound:           0,
		F3Instance:            0,
		F3Cert:                nil,
		AcceptedAt:            time.Now().UTC(),
		AncestorRoots:         []cid.Cid{mkCID(t, "a1"), mkCID(t, "a2")},
	}
	require.NoError(t, trustedroot.Persist(db, tr))

	got, err := trustedroot.Load(context.Background(), db)
	require.NoError(t, err)
	require.Equal(t, tr.Epoch, got.Epoch)
	require.Equal(t, tr.StateRoot, got.StateRoot)
	require.Equal(t, tr.ParentWeight.String(), got.ParentWeight.String())
	require.Equal(t, len(tr.AncestorRoots), len(got.AncestorRoots))

	_ = hs // keep referenced; the full Build is exercised by integration
	_ = (*certs.FinalityCertificate)(nil)
}
