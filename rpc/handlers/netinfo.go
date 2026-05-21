// Phase 10 Part A — wire the live libp2p host through the Net* RPC methods.
//
// Curio's webui consumes NetPeers, NetBandwidthStats, NetAutoNatStatus and
// friends to populate the "Chain Node Network" panel. Phase 9 shipped stub
// implementations that satisfied the typed bindings but always returned
// zeros/empties; this file replaces them with adapters over the live libp2p
// Host owned by net/libp2p.
//
// The handlers package intentionally does NOT depend on libp2p directly:
// instead we declare a narrow NetInfo interface, and the daemon wires a
// real *libp2p.Host into it. This keeps the rpc/handlers test suite
// libp2p-free.

package handlers

import (
	"github.com/Reiers/lantern/api"
)

// NetInfo is the narrow surface the Net* RPC methods need from the live
// libp2p host. net/libp2p.Host satisfies it; tests can supply mocks.
//
// Method semantics mirror lotus api.FullNode and the corresponding libp2p
// primitives — see net/libp2p/host.go for the implementation.
type NetInfo interface {
	// Peers returns the currently-connected peer set with each peer's
	// known multiaddrs. The slice is allocated fresh per call; callers
	// own it. Empty slice (not nil) when zero peers are connected.
	Peers() []NetInfoPeer

	// AgentVersion returns the peerstore-cached agent string for peerID,
	// or "" when the peer is unknown to the peerstore.
	AgentVersion(peerID string) string

	// Connectedness returns the libp2p network.Connectedness for peerID
	// as an int (0=NotConnected, 1=Connected, 2=CanConnect, 3=CannotConnect).
	Connectedness(peerID string) int

	// Listening returns true when the host has at least one listen addr.
	Listening() bool

	// BandwidthTotals returns cumulative + rate counters for the host.
	BandwidthTotals() api.NetBandwidthStats

	// AutoNatStatus returns reachability + the host's currently-believed
	// public addresses (typically empty on a light client behind NAT).
	AutoNatStatus() api.NatInfo
}

// NetInfoPeer is the peer-level shape NetPeers returns over RPC. We use a
// concrete struct rather than reaching into libp2p's peer types so the
// JSON-RPC wire format stays anchored regardless of upstream version bumps.
type NetInfoPeer struct {
	ID    string
	Addrs []string
}

// WithNetInfo attaches a NetInfo source to the handler, used by Net* probes.
// Returns the receiver for chained init in cmd/lantern.
func (c *ChainAPI) WithNetInfo(n NetInfo) *ChainAPI {
	c.NetInfoSource = n
	return c
}
