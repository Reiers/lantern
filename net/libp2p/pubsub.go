// Filecoin-shape gossipsub configuration.
//
// Issue #1 follow-up: the daemon's gossipsub mesh was using stock IPFS
// defaults, not Filecoin's. Effect on the live mainnet daemon: blocks
// arrived ~13 seconds slower than Lotus on the same box, because the
// mesh density was lower (D=6 instead of D=8) and message IDs didn't
// match the rest of the Filecoin network (default SHA1 of the gossipsub
// envelope instead of blake2b of the payload, which is what Lotus and
// Forest use).
//
// Reference: lotus/node/modules/lp2p/pubsub.go and Forest's
// f3-sidecar/pubsub.go. Both apply the same shape; we copy it here so
// Lantern looks like a real Filecoin peer to its neighbours.

package libp2p

import (
	"context"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pubsub_pb "github.com/libp2p/go-libp2p-pubsub/pb"

	"github.com/Reiers/lantern/build"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"golang.org/x/crypto/blake2b"
)

// applyFilecoinGossipSubGlobals reshapes the gossipsub overlay constants
// to match Lotus/Forest. These are package-level globals in
// go-libp2p-pubsub, so they have to be set BEFORE NewGossipSub is called.
// Calling this multiple times is safe (idempotent assignment).
//
// Values copied from lotus/node/modules/lp2p/pubsub.go.
func applyFilecoinGossipSubGlobals() {
	pubsub.GossipSubD = 8
	pubsub.GossipSubDscore = 6
	pubsub.GossipSubDout = 3
	pubsub.GossipSubDlo = 6
	pubsub.GossipSubDhi = 12
	pubsub.GossipSubDlazy = 12
	pubsub.GossipSubDirectConnectInitialDelay = 30 * time.Second
	pubsub.GossipSubIWantFollowupTime = 5 * time.Second
	pubsub.GossipSubHistoryLength = 10
	pubsub.GossipSubGossipFactor = 0.1
}

// filecoinMessageID computes the Filecoin gossipsub message ID: blake2b
// hash of the payload. This is what every Filecoin node uses; without it
// our message-IDs differ from the rest of the network and we either
// (a) re-process duplicates because their IDs don't match, or
// (b) deduplicate against the wrong key. Either is bad.
func filecoinMessageID(m *pubsub_pb.Message) string {
	h := blake2b.Sum256(m.Data)
	return string(h[:])
}

// Filecoin peer-score parameters, lifted from Lotus.
//
// The thresholds here are intentionally generous; Lantern is a participant
// in the mesh, not a gatekeeper, and we'd rather receive a borderline
// peer's messages than reject them.
var (
	gossipScoreThreshold             = -500.0
	publishScoreThreshold            = -1000.0
	graylistScoreThreshold           = -2500.0
	acceptPXScoreThreshold           = 1000.0
	opportunisticGraftScoreThreshold = 3.5
)

func filecoinPeerScoreParams() *pubsub.PeerScoreParams {
	return &pubsub.PeerScoreParams{
		AppSpecificScore:  func(p peer.ID) float64 { return 0 },
		AppSpecificWeight: 1,

		IPColocationFactorThreshold: 5,
		IPColocationFactorWeight:    -100,

		BehaviourPenaltyThreshold: 6,
		BehaviourPenaltyWeight:    -10,
		BehaviourPenaltyDecay:     pubsub.ScoreParameterDecay(time.Hour),

		DecayInterval: pubsub.DefaultDecayInterval,
		DecayToZero:   pubsub.DefaultDecayToZero,

		RetainScore: 6 * time.Hour,

		Topics: filecoinTopicScoreParams(),
	}
}

// filecoinTopicScoreParams returns Lotus's per-topic score params for the
// blocks + msgs topics (#97). Without these, a peer that never usefully
// delivers anything scores the same as one that consistently delivers
// blocks first; WITH them, first-delivery history accumulates positive
// score for honest long-lived peers while a freshly-dialed Sybil swarm
// scores ~0 and decays out of the mesh. This is what makes the #80
// "distinct scored peers" head corroboration meaningful.
//
// Keys must be exact topic strings, so we register params for every
// network's topics; only joined topics are consulted by gossipsub, so
// the unused network's entries are inert.
//
// Values copied from lotus/node/modules/lp2p/pubsub.go (mesh-delivery
// failure penalties stay off there too - the network is too small for
// meaningful incoming-edge distribution; revisit when Lotus does).
func filecoinTopicScoreParams() map[string]*pubsub.TopicScoreParams {
	blocks := func() *pubsub.TopicScoreParams {
		return &pubsub.TopicScoreParams{
			// expected 10 blocks/min
			TopicWeight: 0.1, // max cap is 50, single invalid message is -100

			// 1 tick per second, maxes at 1 after 1 hour
			TimeInMeshWeight:  0.00027, // ~1/3600
			TimeInMeshQuantum: time.Second,
			TimeInMeshCap:     1,

			// deliveries decay after 1 hour, cap at 100 blocks
			FirstMessageDeliveriesWeight: 5, // max value is 500
			FirstMessageDeliveriesDecay:  pubsub.ScoreParameterDecay(time.Hour),
			FirstMessageDeliveriesCap:    100, // 100 blocks in an hour

			// invalid messages decay after 1 hour
			InvalidMessageDeliveriesWeight: -1000,
			InvalidMessageDeliveriesDecay:  pubsub.ScoreParameterDecay(time.Hour),
		}
	}
	msgs := func() *pubsub.TopicScoreParams {
		return &pubsub.TopicScoreParams{
			// expected > 1 tx/second
			TopicWeight: 0.1, // max cap is 5, single invalid message is -100

			// 1 tick per second, maxes at 1 hour
			TimeInMeshWeight:  0.0002778, // ~1/3600
			TimeInMeshQuantum: time.Second,
			TimeInMeshCap:     1,

			// deliveries decay after 10min, cap at 100 tx
			FirstMessageDeliveriesWeight: 0.5, // max value is 50
			FirstMessageDeliveriesDecay:  pubsub.ScoreParameterDecay(10 * time.Minute),
			FirstMessageDeliveriesCap:    100, // 100 messages in 10 minutes

			// invalid messages decay after 1 hour
			InvalidMessageDeliveriesWeight: -1000,
			InvalidMessageDeliveriesDecay:  pubsub.ScoreParameterDecay(time.Hour),
		}
	}
	return map[string]*pubsub.TopicScoreParams{
		build.MainnetGossipTopicBlocks:    blocks(),
		build.CalibnetGossipTopicBlocks:   blocks(),
		build.MainnetGossipTopicMessages:  msgs(),
		build.CalibnetGossipTopicMessages: msgs(),
	}
}

func filecoinPeerScoreThresholds() *pubsub.PeerScoreThresholds {
	return &pubsub.PeerScoreThresholds{
		GossipThreshold:             gossipScoreThreshold,
		PublishThreshold:            publishScoreThreshold,
		GraylistThreshold:           graylistScoreThreshold,
		AcceptPXThreshold:           acceptPXScoreThreshold,
		OpportunisticGraftThreshold: opportunisticGraftScoreThreshold,
	}
}

// newFilecoinPubSub constructs a GossipSub instance configured to behave
// like a Filecoin mainnet node, with optional direct peers (a hint to the
// router that these neighbours should always be in our mesh for block /
// message gossip propagation, bypassing normal mesh churn).
//
// Direct peers must already be connected at the libp2p layer; we read
// their addresses from the host's peerstore where possible, and fall back
// to multiaddr parsing for any that haven't been seen.
func newFilecoinPubSub(ctx context.Context, h host.Host, directPeerAddrs []string, tracer pubsub.RawTracer) (*pubsub.PubSub, error) {
	applyFilecoinGossipSubGlobals()

	// Resolve direct peer multiaddrs to peer.AddrInfo.
	direct := make([]peer.AddrInfo, 0, len(directPeerAddrs))
	for _, ma := range directPeerAddrs {
		mAddr, err := multiaddr.NewMultiaddr(ma)
		if err != nil {
			continue
		}
		info, err := peer.AddrInfoFromP2pAddr(mAddr)
		if err != nil {
			continue
		}
		direct = append(direct, *info)
	}

	opts := []pubsub.Option{
		pubsub.WithFloodPublish(true),
		pubsub.WithPeerExchange(true),
		pubsub.WithMessageIdFn(filecoinMessageID),
		pubsub.WithValidateQueueSize(256),
		pubsub.WithPeerScore(filecoinPeerScoreParams(), filecoinPeerScoreThresholds()),
	}
	if len(direct) > 0 {
		opts = append(opts, pubsub.WithDirectPeers(direct))
	}
	if tracer != nil {
		// #80 part 2: the corroboration tracker rides gossipsub's raw
		// tracer so head-source counting sees every duplicate delivery.
		opts = append(opts, pubsub.WithRawTracer(tracer))
	}

	return pubsub.NewGossipSub(ctx, h, opts...)
}
