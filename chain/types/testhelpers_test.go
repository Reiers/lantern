// Test-only helpers for types_test.go. Not copied from Lotus.

package types

import (
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

func testAddr(t *testing.T) address.Address {
	t.Helper()
	a, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	return a
}

func testCID(t *testing.T) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte("lantern-test"), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}
