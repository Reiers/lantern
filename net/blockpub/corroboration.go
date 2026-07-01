// Corroboration tracking for the head path (#80 part 2).
//
// Gossipsub deduplicates messages before they reach subscribers, so the
// ingestor only ever sees each block once and cannot tell how many
// distinct peers forwarded it. But gossipsub's OWN plumbing sees every
// copy: the first delivery surfaces via the RawTracer's DeliverMessage
// hook and every subsequent copy via DuplicateMessage, each carrying the
// forwarding peer's ID. That makes the tracer a free corroboration
// counter: every copy of the same block CID from a distinct peer is one
// vote that this block really is circulating on the network mesh, not
// something a single (possibly hostile) peer fabricated for us.
//
// CorroborationTracker records those votes per header CID. The daemon
// wires CorroborationGate over it as the ingestor's head-corroboration
// predicate: a head advance is adopted only once >=N distinct peers have
// forwarded the block (or a trusted anchor/beacon peer has - the
// un-evictable floor from #80 part 1 counts as a super-vote).
//
// Honest boundary (TRUST-MODEL.md): peer IDs are Sybil-cheap. Distinct-
// source counting bites only in combination with gossipsub peer scoring
// (first-delivery history, IP-colocation penalties - see net/libp2p) and
// the protected trusted-peer floor. This raises the cost of a head-path
// eclipse; it is not a finality guarantee. Closing the unfinalized-tip
// fork against a powerful adversary is F3's job.

package blockpub

import (
	"bytes"
	"sync"

	"github.com/ipfs/go-cid"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// corroCap bounds how many distinct block CIDs the tracker retains.
// Blocks arrive ~5/epoch (30s) on Filecoin; 128 entries is ~13 minutes
// of history, far beyond the couple of seconds corroboration needs.
const corroCap = 128

// CorroborationTracker counts distinct forwarding peers per block header
// CID, fed by gossipsub tracer events. Safe for concurrent use: pubsub
// invokes tracer hooks from its own goroutines while the ingestor's gate
// reads counts from the processor goroutine.
type CorroborationTracker struct {
	topic string

	mu      sync.Mutex
	sources map[cid.Cid]map[peer.ID]struct{}
	order   []cid.Cid // FIFO eviction

	recorded uint64 // lifetime votes recorded (incl. repeat peers)
}

// NewCorroborationTracker builds a tracker that only counts messages on
// the given gossipsub topic (the /fil/blocks/<network> topic).
func NewCorroborationTracker(topic string) *CorroborationTracker {
	return &CorroborationTracker{
		topic:   topic,
		sources: make(map[cid.Cid]map[peer.ID]struct{}, corroCap),
	}
}

// record decodes the block message and files the forwarding peer as a
// source for that header CID. Undecodable payloads are ignored: the CID
// is derived from the header bytes themselves, so a peer cannot vote for
// block H without actually sending H's header.
func (t *CorroborationTracker) record(msg *pubsub.Message) {
	if msg == nil || len(msg.Data) == 0 {
		return
	}
	if msg.GetTopic() != t.topic {
		return
	}
	blk := new(ltypes.BlockMsg)
	if err := blk.UnmarshalCBOR(bytes.NewReader(msg.Data)); err != nil || blk.Header == nil {
		return
	}
	c := blk.Header.Cid()
	if !c.Defined() {
		return
	}
	from := msg.ReceivedFrom
	if from == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	set, ok := t.sources[c]
	if !ok {
		set = make(map[peer.ID]struct{}, 4)
		t.sources[c] = set
		t.order = append(t.order, c)
		if len(t.order) > corroCap {
			evict := t.order[0]
			t.order = t.order[1:]
			delete(t.sources, evict)
		}
	}
	set[from] = struct{}{}
	t.recorded++
}

// SourceCount returns how many distinct peers have forwarded the block.
func (t *CorroborationTracker) SourceCount(c cid.Cid) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sources[c])
}

// Sources returns the distinct peers that forwarded the block.
func (t *CorroborationTracker) Sources(c cid.Cid) []peer.ID {
	t.mu.Lock()
	defer t.mu.Unlock()
	set := t.sources[c]
	out := make([]peer.ID, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}

// Stats returns (distinct blocks tracked, lifetime votes recorded).
func (t *CorroborationTracker) Stats() (tracked int, recorded uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sources), t.recorded
}

// Tracer returns the pubsub.RawTracer to register at gossipsub
// construction (net/libp2p HostConfig.PubSubTracer). Nil-safe: a nil
// tracker returns a nil tracer, so callers can wire
// `PubSubTracer: maybeNilTracker.Tracer()` unconditionally.
func (t *CorroborationTracker) Tracer() pubsub.RawTracer {
	if t == nil {
		return nil
	}
	return corroTracer{t: t}
}

// corroTracer adapts CorroborationTracker to pubsub.RawTracer. Only
// DeliverMessage (first copy, post-validation) and DuplicateMessage
// (every further copy) matter; the rest are no-ops.
type corroTracer struct {
	t *CorroborationTracker
}

var _ pubsub.RawTracer = corroTracer{}

func (ct corroTracer) DeliverMessage(msg *pubsub.Message)   { ct.t.record(msg) }
func (ct corroTracer) DuplicateMessage(msg *pubsub.Message) { ct.t.record(msg) }
func (corroTracer) AddPeer(peer.ID, protocol.ID)            {}
func (corroTracer) RemovePeer(peer.ID)                      {}
func (corroTracer) Join(string)                             {}
func (corroTracer) Leave(string)                            {}
func (corroTracer) Graft(peer.ID, string)                   {}
func (corroTracer) Prune(peer.ID, string)                   {}
func (corroTracer) ValidateMessage(*pubsub.Message)         {}
func (corroTracer) RejectMessage(*pubsub.Message, string)   {}
func (corroTracer) ThrottlePeer(peer.ID)                    {}
func (corroTracer) RecvRPC(*pubsub.RPC)                     {}
func (corroTracer) SendRPC(*pubsub.RPC, peer.ID)            {}
func (corroTracer) DropRPC(*pubsub.RPC, peer.ID)            {}
func (corroTracer) UndeliverableMessage(*pubsub.Message)    {}

// CorroborationGate builds the head-adoption corroboration predicate the
// ingestor consults before walking head onto a new tip (#80 part 2).
//
//   - minSources <= 0 disables the gate (returns nil: always adopt).
//   - trusted, when non-nil, is the super-vote: if ANY forwarding peer is
//     trusted (the #80 part 1 protected floor / configured beacons), the
//     head is corroborated regardless of count.
//   - connectedPeers, when non-nil, is the graceful-degradation input: a
//     node with fewer connected peers than minSources cannot possibly
//     meet the bar, so the requirement clamps to the peer count (floor 1).
//     A 1-peer node must never wedge (#79 StatusInsufficient philosophy).
func CorroborationGate(t *CorroborationTracker, minSources int, trusted func(peer.ID) bool, connectedPeers func() int) func(cid.Cid) bool {
	if t == nil || minSources <= 0 {
		return nil
	}
	return func(c cid.Cid) bool {
		srcs := t.Sources(c)
		if trusted != nil {
			for _, p := range srcs {
				if trusted(p) {
					return true
				}
			}
		}
		need := minSources
		if connectedPeers != nil {
			if n := connectedPeers(); n < need {
				need = n
				if need < 1 {
					need = 1
				}
			}
		}
		return len(srcs) >= need
	}
}
