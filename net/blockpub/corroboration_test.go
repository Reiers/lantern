package blockpub

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsubpb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/peer"

	ltypes "github.com/Reiers/lantern/chain/types"
)

const testTopic = "/fil/blocks/testnetnet"

// corroMsg wraps valid block bytes in a pubsub.Message from a given peer
// on a given topic.
func corroMsg(t *testing.T, data []byte, from peer.ID, topic string) *pubsub.Message {
	t.Helper()
	return &pubsub.Message{
		Message:      &pubsubpb.Message{Data: data, Topic: &topic},
		ReceivedFrom: from,
	}
}

// validBlockBytesAtHeight is validBlockBytes with a distinct height so
// tests can mint many distinct block CIDs.
func validBlockBytesAtHeight(t *testing.T, h int64) []byte {
	t.Helper()
	blk := new(ltypes.BlockMsg)
	if err := blk.UnmarshalCBOR(bytes.NewReader(validBlockBytes(t))); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	blk.Header.Height = abi.ChainEpoch(h)
	var buf bytes.Buffer
	if err := blk.MarshalCBOR(&buf); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return buf.Bytes()
}

func blockCID(t *testing.T, data []byte) cid.Cid {
	t.Helper()
	blk := new(ltypes.BlockMsg)
	if err := blk.UnmarshalCBOR(bytes.NewReader(data)); err != nil {
		t.Fatalf("unmarshal test block: %v", err)
	}
	return blk.Header.Cid()
}

// TestCorroborationTracker_CountsDistinctSources: the same block
// forwarded by three distinct peers counts 3; a repeat forward from an
// already-counted peer does not inflate the count.
func TestCorroborationTracker_CountsDistinctSources(t *testing.T) {
	data := validBlockBytes(t)
	c := blockCID(t, data)
	tr := NewCorroborationTracker(testTopic)
	tracer := tr.Tracer()

	tracer.DeliverMessage(corroMsg(t, data, peer.ID("peerA"), testTopic))
	tracer.DuplicateMessage(corroMsg(t, data, peer.ID("peerB"), testTopic))
	tracer.DuplicateMessage(corroMsg(t, data, peer.ID("peerC"), testTopic))
	// Repeat from peerA must not double count.
	tracer.DuplicateMessage(corroMsg(t, data, peer.ID("peerA"), testTopic))

	if got := tr.SourceCount(c); got != 3 {
		t.Fatalf("SourceCount = %d, want 3", got)
	}
	tracked, recorded := tr.Stats()
	if tracked != 1 {
		t.Fatalf("tracked = %d, want 1", tracked)
	}
	if recorded != 4 {
		t.Fatalf("recorded = %d, want 4", recorded)
	}
}

// TestCorroborationTracker_IgnoresOtherTopicsAndGarbage: messages on a
// different topic or with undecodable payloads must not register votes.
func TestCorroborationTracker_IgnoresOtherTopicsAndGarbage(t *testing.T) {
	data := validBlockBytes(t)
	c := blockCID(t, data)
	tr := NewCorroborationTracker(testTopic)
	tracer := tr.Tracer()

	// Wrong topic.
	tracer.DeliverMessage(corroMsg(t, data, peer.ID("peerA"), "/fil/msgs/testnetnet"))
	// Garbage payload on the right topic.
	tracer.DeliverMessage(corroMsg(t, []byte{0xde, 0xad}, peer.ID("peerB"), testTopic))
	// Empty ReceivedFrom.
	tracer.DeliverMessage(corroMsg(t, data, peer.ID(""), testTopic))

	if got := tr.SourceCount(c); got != 0 {
		t.Fatalf("SourceCount = %d, want 0", got)
	}
}

// TestCorroborationTracker_EvictsFIFO: the tracker holds at most corroCap
// distinct block CIDs; the oldest entry is evicted when the cap is hit.
func TestCorroborationTracker_EvictsFIFO(t *testing.T) {
	tr := NewCorroborationTracker(testTopic)
	// Directly exercise record's eviction via synthetic entries: build
	// corroCap+1 distinct blocks by varying the header height.
	first := cid.Undef
	for i := 0; i <= corroCap; i++ {
		data := validBlockBytesAtHeight(t, int64(1000+i))
		if i == 0 {
			first = blockCID(t, data)
		}
		tr.Tracer().DeliverMessage(corroMsg(t, data, peer.ID(fmt.Sprintf("p%d", i)), testTopic))
	}
	tracked, _ := tr.Stats()
	if tracked != corroCap {
		t.Fatalf("tracked = %d, want cap %d", tracked, corroCap)
	}
	if got := tr.SourceCount(first); got != 0 {
		t.Fatalf("oldest entry should be evicted, SourceCount = %d", got)
	}
}

// TestCorroborationGate_Thresholds: below-N is held, at-N adopts.
func TestCorroborationGate_Thresholds(t *testing.T) {
	data := validBlockBytes(t)
	c := blockCID(t, data)
	tr := NewCorroborationTracker(testTopic)

	gate := CorroborationGate(tr, 2, nil, nil)
	if gate == nil {
		t.Fatal("gate must not be nil for minSources=2")
	}

	// Zero sources: held.
	if gate(c) {
		t.Fatal("uncorroborated block must be held")
	}
	tr.Tracer().DeliverMessage(corroMsg(t, data, peer.ID("peerA"), testTopic))
	if gate(c) {
		t.Fatal("single-source block must be held at minSources=2")
	}
	tr.Tracer().DuplicateMessage(corroMsg(t, data, peer.ID("peerB"), testTopic))
	if !gate(c) {
		t.Fatal("two-source block must be corroborated at minSources=2")
	}
}

// TestCorroborationGate_TrustedSuperVote: a single forward from a trusted
// floor peer corroborates regardless of count.
func TestCorroborationGate_TrustedSuperVote(t *testing.T) {
	data := validBlockBytes(t)
	c := blockCID(t, data)
	tr := NewCorroborationTracker(testTopic)

	trusted := func(p peer.ID) bool { return p == peer.ID("anchor") }
	gate := CorroborationGate(tr, 3, trusted, nil)

	tr.Tracer().DeliverMessage(corroMsg(t, data, peer.ID("anchor"), testTopic))
	if !gate(c) {
		t.Fatal("trusted-peer forward must corroborate as a super-vote")
	}
}

// TestCorroborationGate_ClampsToPeerCount: a node with fewer connected
// peers than minSources must not wedge - the requirement clamps down.
func TestCorroborationGate_ClampsToPeerCount(t *testing.T) {
	data := validBlockBytes(t)
	c := blockCID(t, data)
	tr := NewCorroborationTracker(testTopic)

	gate := CorroborationGate(tr, 3, nil, func() int { return 1 })
	tr.Tracer().DeliverMessage(corroMsg(t, data, peer.ID("onlyPeer"), testTopic))
	if !gate(c) {
		t.Fatal("1-peer node must clamp the requirement and adopt")
	}
	// Even a pathological 0-peer report keeps the floor at 1.
	gate0 := CorroborationGate(tr, 3, nil, func() int { return 0 })
	if !gate0(c) {
		t.Fatal("0-peer clamp must floor at 1, not wedge")
	}
}

// TestCorroborationGate_DisabledReturnsNil: minSources<=0 or nil tracker
// disables the gate entirely.
func TestCorroborationGate_DisabledReturnsNil(t *testing.T) {
	tr := NewCorroborationTracker(testTopic)
	if CorroborationGate(tr, 0, nil, nil) != nil {
		t.Fatal("minSources=0 must return a nil gate")
	}
	if CorroborationGate(nil, 2, nil, nil) != nil {
		t.Fatal("nil tracker must return a nil gate")
	}
	var nilTracker *CorroborationTracker
	if nilTracker.Tracer() != nil {
		t.Fatal("nil tracker must return a nil tracer")
	}
}
