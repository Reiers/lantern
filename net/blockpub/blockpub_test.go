package blockpub

import (
	"testing"

	"github.com/filecoin-project/go-address"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"

	ltypes "github.com/Reiers/lantern/chain/types"
)

func TestSuperficiallyValid_NilBlock(t *testing.T) {
	if superficiallyValid(nil) {
		t.Fatal("nil block must not be valid")
	}
	if superficiallyValid(&ltypes.BlockMsg{}) {
		t.Fatal("empty block must not be valid")
	}
}

func TestSuperficiallyValid_HappyPath(t *testing.T) {
	miner, err := address.NewIDAddress(1000)
	if err != nil {
		t.Fatal(err)
	}
	b := &ltypes.BlockMsg{
		Header: &ltypes.BlockHeader{
			Miner:        miner,
			BlockSig:     &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
			BLSAggregate: &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
		},
	}
	if !superficiallyValid(b) {
		t.Fatal("valid block rejected")
	}
}

func TestSuperficiallyValid_MissingSignature(t *testing.T) {
	miner, err := address.NewIDAddress(1000)
	if err != nil {
		t.Fatal(err)
	}
	b := &ltypes.BlockMsg{
		Header: &ltypes.BlockHeader{
			Miner: miner,
			// no BlockSig, no BLSAggregate
		},
	}
	if superficiallyValid(b) {
		t.Fatal("block without signatures must not be valid")
	}
}
