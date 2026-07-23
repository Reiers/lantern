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

	// Devnet targets a locally-hosted Curio devnet (curio-fork/docker,
	// `make devnet/up`). The wire-name, genesis CID, gossip topics, and
	// bootstrap peers are supplied at runtime via ConfigureDevnet (see
	// build/devnet.go), populated by `lantern devnet-init --lotus-rpc
	// <URL>`. Trust posture: single-source (the operator's own lotus);
	// F3 is not required.
	Devnet Network = "devnet"
)

// Valid reports whether n is a recognized network identifier.
func (n Network) Valid() bool {
	switch n {
	case Mainnet, Calibration, Devnet:
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
	case Devnet:
		return devnetCfgOrDie("BootstrapPeers").BootstrapPeers
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
	case Devnet:
		return devnetCfgOrDie("NetworkName").NetworkName
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
	case Devnet:
		return devnetCfgOrDie("GenesisCID").GenesisCID
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
	case Devnet:
		return "/fil/msgs/" + devnetCfgOrDie("GossipTopicMessages").NetworkName
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
	case Devnet:
		return "/fil/blocks/" + devnetCfgOrDie("GossipTopicBlocks").NetworkName
	default:
		return MainnetGossipTopicBlocks
	}
}

// BitswapProtocolPrefix returns the libp2p protocol-ID prefix Filecoin
// nodes serve bitswap under. Lotus and Forest both namespace bitswap as
// "/chain/ipfs/bitswap/...", NOT the boxo/IPFS default "/ipfs/bitswap/...".
// This is the bitswap analogue of the "/fil/kad/<net>" DHT prefix: a
// client using the IPFS default connects but can never exchange blocks
// with the Filecoin swarm. The prefix is the same across networks
// (mainnet + calibration). lantern#50.
func (n Network) BitswapProtocolPrefix() string {
	return "/chain"
}

// F3Manifest returns the embedded F3 network-manifest JSON for the
// selected network. The manifest carries the F3 NetworkName,
// BootstrapEpoch, InitialPowerTable, and gpbft parameters that the F3
// cert-chain verifier uses as its trust anchor.
func (n Network) F3Manifest() []byte {
	switch n {
	case Calibration:
		return F3ManifestCalibnetJSON
	case Devnet:
		// Devnet does not run F3 by default. Return nil; F3 subsystems
		// (beacon cert-exchange) are best-effort and skip when the
		// manifest is empty.
		return nil
	default:
		return F3ManifestMainnetJSON
	}
}

// GenesisUnix returns the wall-clock unix timestamp of epoch 0 for the
// selected network, or 0 when unknown (unconfigured devnet). Used for
// wall-clock sanity checks on bootstrap anchors: expected head epoch ≈
// (now - genesis) / BlockDelaySecs.
func (n Network) GenesisUnix() int64 {
	switch n {
	case Calibration:
		return CalibnetGenesisUnix
	case Devnet:
		if IsDevnetConfigured() {
			if cfg := GetDevnetConfig(); cfg != nil && cfg.GenesisTime > 0 {
				return int64(cfg.GenesisTime)
			}
		}
		return 0
	default:
		return MainnetGenesisUnix
	}
}

// ExpectedHeadEpoch returns the epoch the network head should be at for
// the given unix time, or -1 when the genesis time is unknown. The
// answer is exact for a healthy chain (Filecoin epochs are wall-clock
// scheduled); real heads trail it by at most a few epochs.
func (n Network) ExpectedHeadEpoch(nowUnix int64) int64 {
	genesis := n.GenesisUnix()
	if genesis <= 0 || nowUnix < genesis {
		return -1
	}
	delay := int64(BlockDelaySecs)
	if n == Devnet && IsDevnetConfigured() {
		if cfg := GetDevnetConfig(); cfg != nil && cfg.BlockDelaySecs > 0 {
			delay = int64(cfg.BlockDelaySecs)
		}
	}
	return (nowUnix - genesis) / delay
}

// MainnetGenesisUnix is the unix timestamp of mainnet epoch 0
// (2020-08-24 22:00:00 UTC).
const MainnetGenesisUnix = 1598306400

// CalibnetGenesisUnix is the unix timestamp of the current calibration
// network's epoch 0 (2022-11-01 18:13:00 UTC, the post-reset genesis).
const CalibnetGenesisUnix = 1667326380

// DefaultNetwork is what Lantern targets when no --network flag is
// passed. Mainnet, preserving V1.2.1 behavior.
const DefaultNetwork = Mainnet
