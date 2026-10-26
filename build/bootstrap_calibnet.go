// Calibration libp2p bootstrap peers. Copied verbatim from
// github.com/filecoin-project/lotus/build/bootstrap/calibnet.pi at tag
// v1.38.3 (refreshed 2026-10-26, F10). See LICENSE.
//
// These are the seed peers Lantern dials on startup to enter the Filecoin
// calibration DHT + gossipsub mesh. The calibration network name is
// "calibrationnet" (confirmed live against api.calibration.node.glif.io
// 2026-05-23), which differs from mainnet's "testnetnet" wire-name.
//
// The /dnsaddr root is expanded at runtime by net/dnsaddr, with
// BootstrapDNSAddrSnapshot (bootstrap_mainnet.go) as the offline fallback.

package build

// CalibnetBootstrapPeers is the canonical calibration bootstrap multiaddr list.
var CalibnetBootstrapPeers = []string{
	"/dns/bootstrap.calibration.filecoin.chain.love/tcp/1237/p2p/12D3KooWQPYouEAsUQKzvFUA9sQ8tz4rfpqtTzh2eL6USd9bwg7x",
	"/dnsaddr/bootstrap-calibnet.chainsafe-fil.io",
}

// CalibnetGossipTopicMessages is the calibration message-pool gossipsub topic.
// Unlike mainnet's literal "testnetnet" wire-name, calibration uses its own
// network name in the topic path.
const CalibnetGossipTopicMessages = "/fil/msgs/calibrationnet"

// CalibnetGossipTopicBlocks is the calibration block topic.
const CalibnetGossipTopicBlocks = "/fil/blocks/calibrationnet"

// CalibnetNetworkName is the wire-name string Filecoin libp2p protocols
// expect on calibration. Confirmed via Filecoin.StateNetworkName against
// api.calibration.node.glif.io on 2026-05-23.
const CalibnetNetworkName = "calibrationnet"

// CalibnetGenesisCID is the CID of block 0 of Filecoin calibration,
// returned by Filecoin.ChainGetGenesis on any healthy calibration node.
// Used by the /fil/hello/1.0.0 protocol to identify our network to
// peers: nodes on different chains close the Hello stream on genesis
// mismatch.
//
// Source: queried Filecoin.ChainGetGenesis against
// api.calibration.node.glif.io on 2026-05-23.
const CalibnetGenesisCID = "bafy2bzacecyaggy24wol5ruvs6qm73gjibs2l2iyhcqmvi7r7a4ph7zx3yqd4"
