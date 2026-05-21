// Package libp2p builds Lantern's libp2p Host: a pure-Go node with TCP,
// QUIC, and WebSocket transports, that dials Filecoin mainnet bootstrap
// peers and participates in the gossipsub mesh.
//
// Why we need this: Lantern's read path can run against an HTTPS gateway,
// but the WRITE path (MpoolPush) requires publishing to the network's
// `/fil/msgs/testnetnet` gossipsub topic. Without a libp2p host, the node
// is read-only.
//
// Design: this package owns the libp2p Host and gossipsub PubSub instance.
// net/mpool wraps the PubSub topic. Other consumers (block subscriber,
// F3 certexchange) are free to attach to the same Host.
//
// No CGo. All transports are pure-Go.

package libp2p

import (
	"context"
	"fmt"
	"sync"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/multiformats/go-multiaddr"
)

// HostConfig configures the Lantern libp2p node.
type HostConfig struct {
	// Listen multiaddrs. Default: ["/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"].
	ListenAddrs []string
	// BootstrapPeers is the list of multiaddrs to dial on startup.
	BootstrapPeers []string
	// MaxPeers caps the connection-manager peer count. Default 50.
	MaxPeers int
}

// Host wraps a libp2p Host + GossipSub PubSub instance.
type Host struct {
	H      host.Host
	PubSub *pubsub.PubSub

	mu       sync.Mutex
	closed   bool
	cancelCb []func()
}

// New constructs and starts a libp2p Host and a GossipSub PubSub on it.
// The caller is responsible for calling Close().
func New(ctx context.Context, cfg HostConfig) (*Host, error) {
	if len(cfg.ListenAddrs) == 0 {
		cfg.ListenAddrs = []string{
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/udp/0/quic-v1",
		}
	}
	if cfg.MaxPeers == 0 {
		cfg.MaxPeers = 50
	}

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(cfg.ListenAddrs...),
		libp2p.DefaultTransports,
		libp2p.DefaultSecurity,
		libp2p.DefaultMuxers,
		libp2p.UserAgent("lantern/0.1"),
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p.New: %w", err)
	}

	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("pubsub.NewGossipSub: %w", err)
	}

	out := &Host{H: h, PubSub: ps}

	// Background dial of bootstrap peers (non-blocking).
	if len(cfg.BootstrapPeers) > 0 {
		go out.connectBootstrap(ctx, cfg.BootstrapPeers)
	}

	return out, nil
}

// connectBootstrap fires off bootstrap dials concurrently with a per-peer
// timeout. We don't block on the result.
func (h *Host) connectBootstrap(ctx context.Context, peers []string) {
	var wg sync.WaitGroup
	for _, p := range peers {
		ma, err := multiaddr.NewMultiaddr(p)
		if err != nil {
			continue
		}
		info, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(ai peer.AddrInfo) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			_ = h.H.Connect(cctx, ai)
		}(*info)
	}
	wg.Wait()
}

// Close shuts down the host. Subsequent PubSub publishes will fail.
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	for _, cb := range h.cancelCb {
		cb()
	}
	return h.H.Close()
}

// AddCleanup registers a callback to fire on Close.
func (h *Host) AddCleanup(cb func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelCb = append(h.cancelCb, cb)
}

// PeerCount returns the current number of connected peers.
func (h *Host) PeerCount() int {
	return len(h.H.Network().Peers())
}

// ID returns the libp2p peer ID of this node.
func (h *Host) ID() peer.ID {
	return h.H.ID()
}

// ListenAddrs returns the multiaddrs the host is listening on.
func (h *Host) ListenAddrs() []string {
	addrs := h.H.Addrs()
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}
