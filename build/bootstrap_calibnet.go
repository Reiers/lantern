// Calibration libp2p bootstrap peers. Copied verbatim from
// github.com/filecoin-project/lotus/build/bootstrap/calibnet.pi at tag
// v1.36.0 (2026-05-23 verified). See LICENSE-LOTUS.
//
// These are the seed peers Lantern dials on startup to enter the Filecoin
// calibration DHT + gossipsub mesh. The calibration network name is
// "calibrationnet" (confirmed live against api.calibration.node.glif.io
// 2026-05-23) — note this differs from mainnet's "testnetnet" wire-name.

package build

// CalibnetBootstrapPeers is the canonical calibration bootstrap multiaddr list.
var CalibnetBootstrapPeers = []string{
	"/dns/bootstrap.calibration.filecoin.chain.love/tcp/1237/p2p/12D3KooWQPYouEAsUQKzvFUA9sQ8tz4rfpqtTzh2eL6USd9bwg7x",
	"/dns/bootstrap-calibnet-0.chainsafe-fil.io/tcp/34000/p2p/12D3KooWABQ5gTDHPWyvhJM7jPhtNwNJruzTEo32Lo4gcS5ABAMm",
	"/dns/bootstrap-calibnet-1.chainsafe-fil.io/tcp/34000/p2p/12D3KooWS3ZRhMYL67b4bD5XQ6fcpTyVQXnDe8H89LvwrDqaSbiT",
	"/dns/bootstrap-calibnet-2.chainsafe-fil.io/tcp/34000/p2p/12D3KooWEiBN8jBX8EBoM3M47pVRLRWV812gDRUJhMxgyVkUoR48",
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
