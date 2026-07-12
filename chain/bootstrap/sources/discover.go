// Discovery helpers for the bootstrap quorum: build a default source
// list from a configured libp2p host, the mainnet bootstrap peer list,
// a list of public Forest archives, optional user --peer URLs, and
// DHT-discovered Lantern beacons.
//
// Splits cleanly from sources.go so callers that already have a host
// + DHT can mix sources however they like.

package sources

import (
	"context"
	"strings"
	"time"

	"github.com/filecoin-project/go-f3/gpbft"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"github.com/Reiers/lantern/chain/bootstrap"
)

// DefaultLanternBeacons is the curated set of known-good Lantern beacons
// that run cert-exchange (V1.2.1, B-11-01). Quorum probes dial these
// over libp2p and count them as independent KindLanternBeacon sources.
//
// These are independent operators — not the Lantern project. The
// gateway is at gateway.lantern.reiers.io and is run by the project;
// the beacons in this list are operated separately and count toward the
// trust quorum by default. New beacons are added here as operators
// volunteer them.
// Intentionally empty. Operators add Lantern beacons via the
// --lantern-beacon multiaddr flag (or via SourceSetConfig.LanternBeacons
// when embedding the library). Shipping a curated list of named beacons
// here would (a) leak operational topology of whoever runs the beacons,
// (b) put the project in a curator role we don't want, and (c) go
// stale every time a beacon rotates its libp2p peer ID. A future
// release may publish a community-maintained list at a separate URL.
var DefaultLanternBeacons = []string{}

// MainnetPublicForestURLs is the mainnet-only subset of the public
// JSON-RPC endpoints. Use this when bootstrapping a mainnet node.
//
// Curated to be operationally independent: Glif (Protocol Labs
// infra), chain.love (Protocol Labs community), and any user-added
// --peer URLs add to this base set. The quorum tally treats each as
// one vote regardless of operator, so users who want stronger
// independence guarantees should add their own --peer flags.
var MainnetPublicForestURLs = []string{
	"https://api.node.glif.io/rpc/v1",
	"https://api.chain.love/rpc/v1",
}

// CalibnetPublicForestURLs is the calibration subset of the public
// JSON-RPC endpoints. Use this when bootstrapping a calibration node.
//
// Calibration has fewer publicly-curated archive endpoints than
// mainnet. The quorum config for calibration drops to 3-of-N as a
// result; operators wanting stricter quorum should add --peer URLs.
var CalibnetPublicForestURLs = []string{
	"https://api.calibration.node.glif.io/rpc/v1",
	// chain.love does NOT run a calibration endpoint as of 2026-05-23
	// (verified). Add it if/when one comes online.
	// ChainSafe forest-archive calibration: verify reachability before
	// adding.
}

// DefaultPublicForestURLs is the curated set of public Filecoin
// JSON-RPC endpoints we know publish F3 finality certs. The bootstrap
// quorum draws from this list when the user does not supply their own
// --peer.
//
// These are independent operators; the list is intentionally short to
// keep the quorum cheap on cold start.
var DefaultPublicForestURLs = []string{
	"https://api.node.glif.io/rpc/v1",
	"https://api.calibration.node.glif.io/rpc/v1",
	// Add additional independent endpoints here as they come online.
	// ChainSafe forest-archive supports F3GetLatestCertificate via
	// the same Lotus-compatible JSON-RPC shape; uncomment once
	// confirmed reachable from CI:
	// "https://forest-archive.chainsafe.dev/rpc/v1",
}

// SourceSetConfig configures the default source-set builder.
type SourceSetConfig struct {
	// Host is the libp2p host used for the cert-exchange protocol. If
	// nil, no libp2p sources are added.
	Host host.Host
	// MainnetBootstrapPeers is the multiaddr list to query via
	// cert-exchange (typically build.MainnetBootstrapPeers). Each
	// successful dial becomes a Libp2pSource.
	MainnetBootstrapPeers []string
	// MaxLibp2pPeers caps the number of libp2p sources added. Default 8.
	MaxLibp2pPeers int
	// PublicForestURLs is the list of public JSON-RPC archive URLs. If
	// empty, defaults to MainnetPublicForestURLs.
	PublicForestURLs []string
	// LanternGatewayURL is the Lantern project's gateway URL. Empty
	// disables the gateway source. The gateway source is only added
	// when IncludeGatewayProbe is true; otherwise it is elided from
	// the probe entirely (the gateway is served as a cold-block +
	// state-root fallback at runtime, but it is not a Filecoin
	// JSON-RPC endpoint and cannot answer F3GetLatestCertificate,
	// which would otherwise print a confusing red ✗ during install).
	LanternGatewayURL string
	// IncludeGatewayProbe adds the Lantern gateway to the probe source
	// list. Off by default; operators who set --count-gateway (or
	// otherwise want the gateway represented in the tally) turn this
	// on. Requires a gateway URL.
	IncludeGatewayProbe bool
	// UserPeerURLs is the list of user-supplied --peer URLs.
	UserPeerURLs []string
	// LanternBeacons is the multiaddr list of known Lantern beacons that
	// speak cert-exchange. If nil, DefaultLanternBeacons is used. Set to
	// a non-nil empty slice to disable.
	LanternBeacons []string
	// NetworkName is the F3 manifest network name (e.g. "filecoin").
	NetworkName gpbft.NetworkName
	// SourceTimeout is the per-source RPC/protocol deadline. Default 20s.
	SourceTimeout time.Duration
}

// BuildDefaultSources returns the default source set for a quorum
// probe. The returned list is suitable for direct use with Quorum().
// Order: libp2p (preferred for true independence) → public Forest
// archives → user peers → DHT-discovered Lantern beacons → Lantern
// gateway.
func BuildDefaultSources(cfg SourceSetConfig) []bootstrap.Source {
	if cfg.MaxLibp2pPeers <= 0 {
		cfg.MaxLibp2pPeers = 8
	}
	if cfg.SourceTimeout <= 0 {
		cfg.SourceTimeout = 20 * time.Second
	}
	if len(cfg.PublicForestURLs) == 0 {
		cfg.PublicForestURLs = MainnetPublicForestURLs
	}
	if cfg.LanternBeacons == nil {
		cfg.LanternBeacons = DefaultLanternBeacons
	}

	var out []bootstrap.Source

	// 1. libp2p cert-exchange sources, one per known bootstrap peer.
	if cfg.Host != nil {
		count := 0
		for _, ma := range cfg.MainnetBootstrapPeers {
			if count >= cfg.MaxLibp2pPeers {
				break
			}
			pi, err := addrInfoFromString(ma)
			if err != nil || pi.ID == "" {
				continue
			}
			// Eagerly add the peer's addrs to the host's peerstore so
			// certexchange.Request can dial. This is a no-op if the
			// host has already connected via background bootstrap.
			cfg.Host.Peerstore().AddAddrs(pi.ID, pi.Addrs, time.Hour)
			out = append(out, NewLibp2pSource(cfg.Host, pi.ID, cfg.NetworkName, cfg.SourceTimeout))
			count++
		}
	}

	// 2. Public Forest/Lotus JSON-RPC archives.
	for _, u := range cfg.PublicForestURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		out = append(out, NewForestSource("", u, "", cfg.SourceTimeout))
	}

	// 3. User --peer URLs.
	for _, u := range cfg.UserPeerURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		// Split optional bearer token: "URL|TOKEN".
		token := ""
		if i := strings.Index(u, "|"); i > 0 {
			token = u[i+1:]
			u = u[:i]
		}
		out = append(out, NewUserPeerSource("", u, token, cfg.SourceTimeout))
	}

	// 4. Lantern beacons over cert-exchange (V1.2.1, B-11-01). Requires
	// a libp2p host; without one the beacon source can't dial.
	if cfg.Host != nil {
		for _, ma := range cfg.LanternBeacons {
			ma = strings.TrimSpace(ma)
			if ma == "" {
				continue
			}
			pi, err := addrInfoFromString(ma)
			if err != nil || pi.ID == "" {
				continue
			}
			cfg.Host.Peerstore().AddAddrs(pi.ID, pi.Addrs, time.Hour)
			out = append(out, NewLanternBeaconSource(cfg.Host, pi, cfg.NetworkName, cfg.SourceTimeout))
		}
	}

	// 5. Lantern gateway (opt-in, gated by IncludeGatewayProbe).
	// The gateway serves /state/root + /block/<cid>, not the Filecoin
	// JSON-RPC surface, so probing it as an rpcSource always 404s.
	// Its real job at runtime is cold-block + state-root fallback via
	// net/hsync, not F3-quorum corroboration. See CHANGELOG unreleased
	// (2026-07-12).
	if cfg.IncludeGatewayProbe {
		if g := strings.TrimSpace(cfg.LanternGatewayURL); g != "" {
			out = append(out, NewLanternGatewaySource("", g, cfg.SourceTimeout))
		}
	}

	return out
}

// addrInfoFromString parses a multiaddr like
// /dns/bootstrap.example/tcp/1234/p2p/12D3KooW... into a peer.AddrInfo.
func addrInfoFromString(s string) (peer.AddrInfo, error) {
	ma, err := multiaddr.NewMultiaddr(s)
	if err != nil {
		return peer.AddrInfo{}, err
	}
	pi, err := peer.AddrInfoFromP2pAddr(ma)
	if err != nil {
		return peer.AddrInfo{}, err
	}
	return *pi, nil
}

// WaitForLibp2pPeers blocks until at least minPeers libp2p connections
// are established or ctx is done. Useful as a precondition to running
// the quorum so cert-exchange has open streams to query.
func WaitForLibp2pPeers(ctx context.Context, h host.Host, minPeers int, interval time.Duration) {
	if h == nil || minPeers <= 0 {
		return
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if len(h.Network().Peers()) >= minPeers {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
