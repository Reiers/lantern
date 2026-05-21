// Phase 8 — regression test for the StateMinerInfo PeerId bug surfaced
// by docs/phase8-part-a-results.md.
//
// Before Phase 8, `convertMinerV{17,18}Info` did `s := string(in.PeerId)`,
// which leaked the raw libp2p protobuf-encoded peer.ID bytes (multicodec
// prefix \x00$\b\x01...) verbatim into the JSON-RPC response. That broke
// `lotus state miner-info`, which decodes PeerId as a base58 multihash
// string via peer.IDFromBytes.

package actors

import (
	"testing"

	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"
)

func TestDecodePeerID_NilEmpty(t *testing.T) {
	if decodePeerID(nil) != nil {
		t.Fatal("decodePeerID(nil) should return nil")
	}
	if decodePeerID([]byte{}) != nil {
		t.Fatal("decodePeerID(empty) should return nil")
	}
}

func TestDecodePeerID_RoundTrip(t *testing.T) {
	// Build a known peer.ID from a multihash. We use the simplest path:
	// take a libp2p-generated peer.ID, marshal to bytes, feed through
	// decodePeerID, expect the same base58 string back.
	//
	// The on-chain PeerId field is exactly the output of peer.ID.MarshalBinary.
	const example = "12D3KooWBFCpu7M2bUYbZk5jbAZyMFcdHjp7v2pDyQ7nF3rR1XbX"
	pid, err := libp2ppeer.Decode(example)
	if err != nil {
		t.Fatalf("decode example peer id: %v", err)
	}
	bin, err := pid.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal peer id: %v", err)
	}
	got := decodePeerID(bin)
	if got == nil {
		t.Fatal("decodePeerID returned nil for valid input")
	}
	if *got != example {
		t.Fatalf("round-trip mismatch: got %q want %q", *got, example)
	}
}

func TestDecodePeerID_GarbageReturnsNil(t *testing.T) {
	// Random short bytes: not a valid multihash. Lotus's peer.IDFromBytes
	// returns an error; we surface that as a nil pointer (matches Lotus's
	// "PeerId not set" rendering).
	got := decodePeerID([]byte{0xff, 0xff, 0xff, 0x01, 0x02})
	if got != nil {
		t.Fatalf("expected nil for garbage input, got %q", *got)
	}
}
