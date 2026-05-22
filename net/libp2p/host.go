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
	"sync/atomic"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/metrics"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	connmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/multiformats/go-multiaddr"

	"github.com/Reiers/lantern/internal/buildinfo"
)

// HostConfig configures the Lantern libp2p node.
type HostConfig struct {
	// Listen multiaddrs. Default: ["/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"].
	ListenAddrs []string
	// BootstrapPeers is the list of multiaddrs to dial on startup.
	BootstrapPeers []string
	// MaxPeers is the connection-manager *high-water-mark*: when peer
	// count exceeds this, connmgr starts trimming the lowest-tagged
	// connections after the grace period. Default 200 (V1.2.1 lift;
	// previously 50 hard cap).
	MaxPeers int
	// MinPeers is the connection-manager *low-water-mark*: connmgr keeps
	// at least this many peers around even under trim pressure, and the
	// DHT discovery loop targets this number when dialing fresh peers.
	// Default 50.
	MinPeers int
	// ConnMgrGrace is how long a new connection is protected from trim.
	// Default 20 seconds.
	ConnMgrGrace time.Duration
	// UserAgent overrides the libp2p User-Agent. Default is derived from
	// internal/buildinfo so the wire identifier tracks the release tag.
	UserAgent string
	// DisableBandwidthCounter skips the metrics.BandwidthCounter wiring.
	// Used by tests that don't want to allocate the counter.
	DisableBandwidthCounter bool
	// GossipSubDirectPeers is a list of libp2p multiaddrs that the
	// gossipsub router should treat as direct mesh neighbours for the
	// block / message topics. We use it to pin big fast Filecoin nodes
	// (ChainSafe, chain.love, filincubator, devtty) into our mesh so
	// new heads propagate at the same speed they do for those nodes.
	// Defaults to BootstrapPeers when empty.
	GossipSubDirectPeers []string
}

// Host wraps a libp2p Host + GossipSub PubSub instance.
type Host struct {
	H      host.Host
	PubSub *pubsub.PubSub

	mu       sync.Mutex
	closed   bool
	cancelCb []func()

	// Phase 8 Part D: optional Kademlia DHT (client mode). Nil until
	// EnableDHT is called. peerHWM tracks the last observed peer
	// count via the refresh loop.
	kdht    *dht.IpfsDHT
	peerHWM int64

	// Phase 10: BandwidthCounter is the libp2p-standard metrics reporter
	// installed via libp2p.BandwidthReporter on construction. RPC's
	// NetBandwidthStats reads from this counter directly. Nil only when
	// HostConfig.DisableBandwidthCounter is set (used by tests).
	BW *metrics.BandwidthCounter

	// Phase 10: cached reachability status, updated by subscribeReachability.
	// libp2p's AmbientAutoNAT client publishes EvtLocalReachabilityChanged on
	// the host's event bus; we mirror the latest value here so callers don't
	// need to plumb the event bus through the RPC stack.
	reachability atomic.Int32 // network.Reachability

	// V1.2.1: cached connmgr watermarks so the DHT discovery loop can
	// stop dialing once we're comfortably above MinPeers.
	cfgMin int
	cfgMax int

	// Issue #9: keepalive loop counters. The 30s redial loop in dht.go
	// publishes its activity here so the dashboard / lantern info can
	// surface 'is the loop actively topping up peer count' without
	// having to grep logs.
	kaCycles    atomic.Uint64
	kaTriggered atomic.Uint64
	kaBootDial  atomic.Uint64
	kaRouteDial atomic.Uint64
	kaLastCount atomic.Int64

	// Issue #9 follow-up: track whether dials we made on the previous
	// cycle resulted in a connection that's still alive at the current
	// cycle. kaStuck increments when a previous-cycle dial peer is no
	// longer connected (i.e., the dial "didn't stick"). Combined with
	// kaRouteDial + kaBootDial, this lets us diagnose the
	// dialed-but-immediately-dropped pattern, which is the most likely
	// reason the keepalive loop appears to do work but the count keeps
	// drifting (issue #9 comment chain on live mainnet data).
	kaStuck         atomic.Uint64
	kaClosestWalks  atomic.Uint64 // aggressive closest-walks fired by keepalive (not by the periodic 5min loop)
	kaPrevDialedMu  sync.Mutex
	kaPrevDialed    map[peer.ID]struct{}  // peers we dialed on the previous tick
	kaLastAttemptMu sync.Mutex            // covers kaLastAttempt
	kaLastAttempt   map[peer.ID]time.Time // when did we last try to dial each peer

	// Issue #14: dashboard action handlers need access to the bootstrap
	// peer list + network config to manually trigger keepalive cycles
	// from the operator UI. EnableDHT snapshots its options here so the
	// handlers don't need a second copy of the configuration plumbed
	// through.
	dhtOpts   DHTOptions
	dhtOptsOK atomic.Bool
}

// MinPeers returns the configured connection-manager low-water-mark.
func (h *Host) MinPeers() int { return h.cfgMin }

// MaxPeers returns the configured connection-manager high-water-mark.
func (h *Host) MaxPeers() int { return h.cfgMax }

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
		cfg.MaxPeers = 200
	}
	if cfg.MinPeers == 0 {
		cfg.MinPeers = 50
	}
	if cfg.MinPeers > cfg.MaxPeers {
		// Defensive: keep the watermarks well-ordered. connmgr panics
		// otherwise.
		cfg.MinPeers = cfg.MaxPeers / 2
	}
	if cfg.ConnMgrGrace == 0 {
		cfg.ConnMgrGrace = 20 * time.Second
	}

	ua := cfg.UserAgent
	if ua == "" {
		ua = buildinfo.UserAgent()
	}

	// Phase 10: a BandwidthCounter installed via libp2p.BandwidthReporter
	// makes the host's stream-level bandwidth visible to NetBandwidthStats.
	var bw *metrics.BandwidthCounter
	if !cfg.DisableBandwidthCounter {
		bw = metrics.NewBandwidthCounter()
	}

	// V1.2.1: explicit connection manager with a low watermark so the
	// host actively maintains MinPeers rather than drifting to whatever
	// the bootstrap dials happen to land on. High watermark + grace
	// period gate the trim path; low watermark is what the DHT loop
	// in dht.go targets when ranking outbound dials.
	cm, cmErr := connmgr.NewConnManager(cfg.MinPeers, cfg.MaxPeers,
		connmgr.WithGracePeriod(cfg.ConnMgrGrace))
	if cmErr != nil {
		return nil, fmt.Errorf("connmgr.NewConnManager(%d,%d): %w",
			cfg.MinPeers, cfg.MaxPeers, cmErr)
	}

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(cfg.ListenAddrs...),
		libp2p.DefaultTransports,
		libp2p.DefaultSecurity,
		libp2p.DefaultMuxers,
		libp2p.UserAgent(ua),
		libp2p.ConnectionManager(cm),
	}
	if bw != nil {
		opts = append(opts, libp2p.BandwidthReporter(bw))
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p.New: %w", err)
	}

	// Issue #1 follow-up: configure gossipsub with Filecoin-shape mesh
	// constants + blake2b message IDs + Lotus peer-score params, plus
	// pin the bootstrap peers as direct mesh neighbours so new heads
	// propagate at native Filecoin speed.
	directPeers := cfg.GossipSubDirectPeers
	if len(directPeers) == 0 {
		directPeers = cfg.BootstrapPeers
	}
	ps, err := newFilecoinPubSub(ctx, h, directPeers)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("pubsub.NewGossipSub: %w", err)
	}

	out := &Host{
		H:      h,
		PubSub: ps,
		BW:     bw,
		cfgMax: cfg.MaxPeers,
		cfgMin: cfg.MinPeers,
	}
	out.reachability.Store(int32(network.ReachabilityUnknown))

	// Subscribe to libp2p's AutoNAT reachability events so NetAutoNatStatus
	// can return the current value without consulting the event bus on every
	// RPC call. The subscription is best-effort: if the bus refuses (test
	// hosts can be minimal), we fall back to ReachabilityUnknown.
	if sub, err := h.EventBus().Subscribe(new(event.EvtLocalReachabilityChanged)); err == nil {
		go out.consumeReachability(ctx, sub)
		out.AddCleanup(func() { _ = sub.Close() })
	}

	// Background dial of bootstrap peers (non-blocking).
	if len(cfg.BootstrapPeers) > 0 {
		go out.connectBootstrap(ctx, cfg.BootstrapPeers)
	}

	return out, nil
}

// consumeReachability mirrors EvtLocalReachabilityChanged into the cached
// atomic so NetAutoNatStatus is a lock-free read.
func (h *Host) consumeReachability(ctx context.Context, sub event.Subscription) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.Out():
			if !ok {
				return
			}
			if r, ok := ev.(event.EvtLocalReachabilityChanged); ok {
				h.reachability.Store(int32(r.Reachability))
			}
		}
	}
}

// Reachability returns the latest AutoNAT-discovered reachability. Defaults
// to ReachabilityUnknown until the AmbientAutoNAT subsystem produces its
// first measurement (~30s post-bootstrap on a public peer).
func (h *Host) Reachability() network.Reachability {
	return network.Reachability(h.reachability.Load())
}

// PublicAddrs returns the host's listen addrs filtered to those libp2p
// believes are publicly dialable. On a light client behind NAT this list is
// typically empty; on a public beacon it carries the dial-back addrs.
func (h *Host) PublicAddrs() []string {
	addrs := h.H.Addrs()
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
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
