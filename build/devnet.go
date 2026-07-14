// Devnet (localnet) support.
//
// Unlike Mainnet / Calibration, a devnet's identity (network wire-name,
// genesis CID, gossip topics, bootstrap peers) is not knowable at
// Lantern-build-time: the operator spins up a fresh Curio docker devnet
// (curio-fork/docker/) on their laptop, its genesis is generated
// per-boot, and the wire-name is whatever the genesis-template says.
//
// So `build.Devnet` reads its parameters from a runtime configuration
// singleton that must be populated by the CLI (via `lantern devnet-init
// --lotus-rpc <URL>`) before any Network method is called. The devnet
// config lives at <data-dir>/devnet-config.json.
//
// Rationale for a Network variant (vs. an ad-hoc code path):
//   - Every Lantern subsystem that dispatches on Network (bootstrap
//     peers, gossip topics, DHT prefix, hello handshake) picks up devnet
//     automatically once the switch statements handle it.
//   - Devnet then follows the same fetcher / hstore / mpool / persist
//     lifecycle as calibration + mainnet, so #118/#119 restart auto-heal
//     applies without new code.
//   - Adding Butterflynet or a future testnet is a trivial follow-up:
//     drop-in another variant with hardcoded constants.
//
// Trust posture:
//   - The operator OWNS the devnet (they ran `make devnet/up`). A single
//     trusted lotus RPC endpoint is the canonical head source. Multi-source
//     quorum is skipped (there's only one source available). F3 is not
//     required — the docker devnet does not run F3 by default.
//   - `lantern devnet-init` seeds the initial bootstrap-anchor.json from
//     ChainHead of the devnet lotus. Subsequent boots use the same anchor
//     load path as calibration / mainnet.

package build

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DevnetConfig is the runtime identity of a Lantern-visible devnet. All
// fields are required except LotusRPC (which is used as an anchor + head
// source but not baked into the daemon's steady-state operation).
type DevnetConfig struct {
	// NetworkName is the wire-name string libp2p protocols expect
	// (gossipsub topics, DHT prefix). Comes from
	// Filecoin.StateNetworkName on the devnet lotus.
	NetworkName string `json:"networkName"`

	// GenesisCID is the CID of block 0. Comes from
	// Filecoin.ChainGetGenesis on the devnet lotus. Used by the
	// /fil/hello/1.0.0 handshake so peers on the same devnet
	// authenticate.
	GenesisCID string `json:"genesisCID"`

	// LotusRPC is the JSON-RPC URL of the devnet lotus (used by
	// `lantern devnet-init` to fetch this config, and later by the
	// daemon as a fallback head source when there is no gossipsub
	// mesh — a single-node devnet stays reachable this way).
	LotusRPC string `json:"lotusRPC"`

	// BootstrapPeers is the list of libp2p multiaddrs the daemon
	// dials on startup. For the curio-fork docker devnet, this is
	// typically empty (single-cluster; lotus discovers itself) or
	// contains the docker-network lotus multiaddr.
	BootstrapPeers []string `json:"bootstrapPeers,omitempty"`

	// GenesisTime is the wall-clock time of block 0 (unix seconds).
	// Optional; if set, it lets the daemon compute anchor age
	// without a chain read. Comes from ChainGetGenesis's Blocks[0].Timestamp.
	GenesisTime uint64 `json:"genesisTime,omitempty"`

	// BlockDelaySecs is the block time (default 4s for the
	// //go:build 2k curio devnet; mainnet is 30s). Optional; the
	// daemon uses build.BlockDelaySecs as fallback.
	BlockDelaySecs uint64 `json:"blockDelaySecs,omitempty"`

	// EthChainID is the EIP-155 chain identifier the devnet lotus
	// reports via eth_chainId. Comes from eth_chainId on the devnet
	// lotus at devnet-init time. Devnet lotus uses this to scope
	// signatures + reject cross-chain replays. The curio-fork docker
	// devnet defaults to 31415926 (0x1df5e76); custom setups may pick
	// different values. Optional (older configs may lack the field);
	// when missing, callers should re-run `lantern devnet-init --force`.
	EthChainID uint64 `json:"ethChainID,omitempty"`

	// NetworkVersion is the Filecoin network version the devnet reports
	// (Filecoin.StateNetworkVersion). Used with ActorCodeCIDs to register
	// the devnet's custom actor bundle into Lantern's decoder registry at
	// the correct actors version. Optional (older configs lack it).
	NetworkVersion uint64 `json:"networkVersion,omitempty"`

	// ActorCodeCIDs is the devnet's actor-name -> code-CID map
	// (Filecoin.StateActorCodeCIDs). A debug-compiled devnet ships code
	// CIDs that are in no released builtin-actors bundle, so Lantern's
	// registry can't decode devnet actor state without them. Recording
	// them here lets the daemon register the bundle at startup and decode
	// devnet state (StateMinerPower, StateMinerInfo, ...). CIDs are stored
	// as strings for JSON stability. Optional (older configs lack it);
	// re-run `lantern devnet-init --force` to populate.
	ActorCodeCIDs map[string]string `json:"actorCodeCIDs,omitempty"`
}

var (
	devnetCfgMu sync.RWMutex
	devnetCfg   *DevnetConfig
)

// ConfigureDevnet installs a devnet runtime config. Called from
// cmd/lantern before any daemon subsystem asks Network methods for
// devnet values. Subsequent calls overwrite the previous config
// (useful in tests). Nil config clears it.
func ConfigureDevnet(cfg *DevnetConfig) {
	devnetCfgMu.Lock()
	defer devnetCfgMu.Unlock()
	if cfg == nil {
		devnetCfg = nil
		return
	}
	cp := *cfg
	devnetCfg = &cp
}

// GetDevnetConfig returns the current devnet config or nil when
// unconfigured. Callers on the devnet hot path should use the
// convenience getters below (they panic-with-help when unconfigured).
func GetDevnetConfig() *DevnetConfig {
	devnetCfgMu.RLock()
	defer devnetCfgMu.RUnlock()
	if devnetCfg == nil {
		return nil
	}
	cp := *devnetCfg
	return &cp
}

// IsDevnetConfigured reports whether ConfigureDevnet has been called
// with a non-nil config in this process.
func IsDevnetConfigured() bool {
	devnetCfgMu.RLock()
	defer devnetCfgMu.RUnlock()
	return devnetCfg != nil
}

// devnetCfgOrDie returns the devnet config or panics with an
// actionable error when unconfigured. Used by the Network method
// switches so callers never silently get zero values.
func devnetCfgOrDie(callSite string) *DevnetConfig {
	devnetCfgMu.RLock()
	defer devnetCfgMu.RUnlock()
	if devnetCfg == nil {
		panic(fmt.Sprintf("build.Devnet.%s called before ConfigureDevnet: run `lantern devnet-init --lotus-rpc <URL>` first", callSite))
	}
	cp := *devnetCfg
	return &cp
}

// LoadDevnetConfig reads a JSON-encoded DevnetConfig from disk.
// Returns (nil, nil) when the file doesn't exist so cold-boot code
// can detect "operator hasn't run devnet-init yet" cleanly.
func LoadDevnetConfig(path string) (*DevnetConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read devnet config: %w", err)
	}
	var c DevnetConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse devnet config: %w", err)
	}
	if c.NetworkName == "" || c.GenesisCID == "" {
		return nil, fmt.Errorf("devnet config missing required fields (networkName, genesisCID); re-run `lantern devnet-init`")
	}
	// EthChainID may be zero on configs written before lantern#123.
	// Handlers that need it should re-run devnet-init or fall back to
	// querying lotus once at boot.

	return &c, nil
}

// SaveDevnetConfig writes cfg to path atomically (tmp file + rename).
// Creates the parent directory if it doesn't exist.
func SaveDevnetConfig(path string, cfg *DevnetConfig) error {
	if cfg == nil {
		return errors.New("nil devnet config")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
