// Network selector. Lantern can run against Filecoin mainnet or
// calibration. The constants for each network live in
// bootstrap_<network>.go; this file is the dispatch layer that lets
// callers say `network.BootstrapPeers()` instead of branching on
// `Mainnet*` vs `Calibnet*` at every site.
//
// Adding a third network in the future (e.g. butterflynet) means:
//   1. Add bootstrap_<name>.go with the constants.
//   2. Add the Network constant below.
//   3. Add the case to each selector method.
// No changes needed at call sites that already go through Network.
//
// Default is Mainnet. Lantern V1.2.1 behavior is byte-identical to
// constructing the daemon with Network unset.

package build

// Network selects which Filecoin network Lantern targets.
type Network string

const (
	// Mainnet is the default — Filecoin mainnet. Wire-name "testnetnet"
	// (historical from pre-rebrand). DRAND chain switches: started on
	// chained DrandMainnet, post-FIP-0063 uses unchained DrandQuicknet.
	Mainnet Network = "mainnet"

	// Calibration is Filecoin's public test network. Wire-name
	// "calibrationnet". DRAND chain: Quicknet only (Calibration was
	// launched post-FIP-0063).
	Calibration Network = "calibration"
)

// Valid reports whether n is a recognized network identifier.
func (n Network) Valid() bool {
	switch n {
	case Mainnet, Calibration:
		return true
	default:
		return false
	}
}

// String returns the canonical network identifier.
func (n Network) String() string { return string(n) }

// BootstrapPeers returns the libp2p multiaddr list Lantern dials on
// startup to enter the DHT + gossipsub mesh for the selected network.
func (n Network) BootstrapPeers() []string {
	switch n {
	case Calibration:
		return CalibnetBootstrapPeers
	default:
		return MainnetBootstrapPeers
	}
}

// NetworkName returns the wire-name string Filecoin libp2p protocols
// (DHT prefix, gossipsub topics) expect for the selected network.
// Mainnet returns "testnetnet" for historical reasons; calibration
// returns "calibrationnet".
func (n Network) NetworkName() string {
	switch n {
	case Calibration:
		return CalibnetNetworkName
	default:
		return MainnetNetworkName
	}
}

// GenesisCID returns the CID of block 0 for the selected network. Used
// by the /fil/hello/1.0.0 handshake to identify which chain a peer is
// on.
func (n Network) GenesisCID() string {
	switch n {
	case Calibration:
		return CalibnetGenesisCID
	default:
		return MainnetGenesisCID
	}
}

// GossipTopicMessages returns the message-pool gossipsub topic name for
// the selected network.
func (n Network) GossipTopicMessages() string {
	switch n {
	case Calibration:
		return CalibnetGossipTopicMessages
	default:
		return MainnetGossipTopicMessages
	}
}

// GossipTopicBlocks returns the block gossipsub topic name for the
// selected network.
func (n Network) GossipTopicBlocks() string {
	switch n {
	case Calibration:
		return CalibnetGossipTopicBlocks
	default:
		return MainnetGossipTopicBlocks
	}
}

// F3Manifest returns the embedded F3 network-manifest JSON for the
// selected network. The manifest carries the F3 NetworkName,
// BootstrapEpoch, InitialPowerTable, and gpbft parameters that the F3
// cert-chain verifier uses as its trust anchor.
func (n Network) F3Manifest() []byte {
	switch n {
	case Calibration:
		return F3ManifestCalibnetJSON
	default:
		return F3ManifestMainnetJSON
	}
}

// DefaultNetwork is what Lantern targets when no --network flag is
// passed. Mainnet, preserving V1.2.1 behavior.
const DefaultNetwork = Mainnet
