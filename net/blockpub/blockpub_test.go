package blockpub

import (
	"bytes"
	"context"
	"testing"

	"github.com/filecoin-project/go-address"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/ipfs/go-cid"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsubpb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/peer"

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

// TestBlockTopicValidator_RejectsGarbage covers issue #18:
// the gossipsub validator must return ValidationReject for non-CBOR or
// malformed messages so gossipsub knows we caught a bad block.
func TestBlockTopicValidator_RejectsGarbage(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		data []byte
		want pubsub.ValidationResult
	}{
		{"nil_data", nil, pubsub.ValidationReject},
		{"empty_data", []byte{}, pubsub.ValidationReject},
		{"garbage_data", []byte{0xde, 0xad, 0xbe, 0xef}, pubsub.ValidationReject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &pubsub.Message{Message: &pubsubpb.Message{Data: tc.data}}
			got := blockTopicValidator(ctx, peer.ID(""), msg)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBlockTopicValidator_AcceptsValid: a well-formed block message
// passes the validator with ValidationAccept (this is the signal that
// keeps remote gossipsub peer-score happy with us).
func TestBlockTopicValidator_AcceptsValid(t *testing.T) {
	miner, err := address.NewIDAddress(1000)
	if err != nil {
		t.Fatal(err)
	}
	dummyCID, _ := cid.Parse("bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")
	blk := &ltypes.BlockMsg{
		Header: &ltypes.BlockHeader{
			Miner:                 miner,
			ParentStateRoot:       dummyCID,
			ParentMessageReceipts: dummyCID,
			Messages:              dummyCID,
			BlockSig:              &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
			BLSAggregate:          &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
		},
	}
	var buf bytes.Buffer
	if err := blk.MarshalCBOR(&buf); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg := &pubsub.Message{Message: &pubsubpb.Message{Data: buf.Bytes()}}
	got := blockTopicValidator(context.Background(), peer.ID(""), msg)
	if got != pubsub.ValidationAccept {
		t.Errorf("got %v, want ValidationAccept", got)
	}
}
