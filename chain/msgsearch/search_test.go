package msgsearch_test

import (
	"context"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/msgsearch"
)

// stubBG implements hamt.BlockGetter returning ErrNotFound for everything.
// Used purely to wire up a Searcher; we don't drive a real chain through it.
type stubBG struct{}

func (s stubBG) Get(_ context.Context, _ cid.Cid) ([]byte, error) {
	return nil, cid.ErrCidTooShort
}

func mkCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

// TestSearcherFindNotFoundOnEmptyStore verifies the searcher handles an
// empty store cleanly.
func TestSearcherFindNotFoundOnEmptyStore(t *testing.T) {
	s, err := hstore.Open("", hstore.Options{})
	require.NoError(t, err)
	defer s.Close()

	srch := msgsearch.New(s, stubBG{})
	_, err = srch.Find(context.Background(), abi.ChainEpoch(-1), mkCID(t, "needle"), 5)
	require.ErrorIs(t, err, msgsearch.ErrNotFound)
}

// TestSearcherFindRequiresStore validates the nil-store guard.
func TestSearcherFindRequiresStore(t *testing.T) {
	srch := msgsearch.New(nil, stubBG{})
	_, err := srch.Find(context.Background(), -1, mkCID(t, "x"), 5)
	require.Error(t, err)
}
