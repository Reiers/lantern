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

// validBlockBytes builds a well-formed, marshalled BlockMsg for tamper tests.
func validBlockBytes(t *testing.T) []byte {
	t.Helper()
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
	return buf.Bytes()
}

// TestBlockTopicValidator_RejectsTrailingGarbage covers the #85 header
// propagation integrity gate: a message that *starts* with a valid block
// CBOR but carries extra trailing bytes must be rejected, so Lantern
// never re-propagates a header whose on-wire bytes don't round-trip
// cleanly. (gossipsub only forwards messages the validator Accepts, so
// this is the propagation gate, not just an ingest check.)
func TestBlockTopicValidator_RejectsTrailingGarbage(t *testing.T) {
	good := validBlockBytes(t)
	tampered := append(append([]byte(nil), good...), 0xde, 0xad, 0xbe, 0xef)
	msg := &pubsub.Message{Message: &pubsubpb.Message{Data: tampered}}
	if got := blockTopicValidator(context.Background(), peer.ID(""), msg); got != pubsub.ValidationReject {
		t.Errorf("trailing-garbage block: got %v, want ValidationReject", got)
	}
}

// TestBlockTopicValidator_RejectsNilHeader: a CBOR message that decodes
// to a BlockMsg with no header must be rejected before the CID check.
func TestBlockTopicValidator_RejectsNilHeader(t *testing.T) {
	// superficiallyValid already guards nil header; assert the validator
	// surfaces it as a reject (defense for the propagation path).
	empty := &ltypes.BlockMsg{}
	var buf bytes.Buffer
	if err := empty.MarshalCBOR(&buf); err != nil {
		// An empty BlockMsg may legitimately fail to marshal; that path is
		// covered by the garbage-data cases. Skip if so.
		t.Skipf("empty marshal not applicable: %v", err)
	}
	msg := &pubsub.Message{Message: &pubsubpb.Message{Data: buf.Bytes()}}
	if got := blockTopicValidator(context.Background(), peer.ID(""), msg); got != pubsub.ValidationReject {
		t.Errorf("nil-header block: got %v, want ValidationReject", got)
	}
}
