// Phase 8 Part D + V1.2.1 — Kademlia DHT for peer discovery.
//
// Pre-Phase-8 Lantern's libp2p host only connected to the small set of
// hardcoded bootstrap peers (typically 3-5 from Filecoin's mainnet
// bootstrap list). Without DHT it had no way to learn about other
// peers, so the connected-peer count never grew above ~3.
//
// With DHT in client mode + a periodic refresh, the host can discover
// peers via the standard libp2p Kademlia routing protocol. We don't run
// in server mode: Lantern is a light client and doesn't want the
// inbound query load.
//
// V1.2.1 lift (see PHASE11-PEER-COUNT-ASK.md):
//   - The plain "re-bootstrap when we drop below TargetPeers" loop only
//     ever recovered to the bootstrap set (3-5 peers). Real growth comes
//     from walking the DHT routing table, which is the population that
//     Kademlia bootstrap fills in across the wider swarm.
//   - We now run two concurrent loops: a fast `GetClosestPeers(myID)`
//     walk (default 5min) to keep the routing table fresh, and a slower
//     dial-out loop (default 10min) that opportunistically connects to
//     peers from the routing table we don't already have a stream to,
//     capped by the host's connmgr high-water-mark.
//
// The discovery loops are split into a free function (RunDHTDiscovery)
// so the beacon path — which constructs its own server-mode DHT — can
// reuse the exact same walk logic without going through EnableDHT.

package libp2p

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// DHTOptions configures the Kademlia client.
type DHTOptions struct {
	// BootstrapPeers seeds the DHT routing table. Reuse the libp2p
	// host's bootstrap list for simplicity.
	BootstrapPeers []string
	// RefreshInterval controls how often the background loop refreshes
	// the DHT routing table + reconnects to bootstrap peers if peer
	// count fell. Default 5 minutes.
	RefreshInterval time.Duration
	// TargetPeers is the floor below which the refresh loop tries
	// harder (re-bootstraps + dials more aggressively). Default 30.
	TargetPeers int

	// V1.2.1 — discovery loop knobs.

	// ClosestWalkInterval drives the fast loop that runs
	// dht.GetClosestPeers(self) to populate the routing table from
	// across the swarm. Default 5 minutes.
	ClosestWalkInterval time.Duration
	// DialWalkInterval drives the slower loop that walks the DHT
	// routing table and opportunistically dials peers we don't already
	// have a connection to, capped by the host's connmgr high-water.
	// Default 10 minutes.
	DialWalkInterval time.Duration
	// MaxDialsPerCycle caps the number of outbound dials a single
	// dial-walk cycle initiates. Default 25 — enough to grow the peer
	// set without thundering, and well below the connmgr trim trigger.
	MaxDialsPerCycle int
	// PerDialTimeout caps each outbound dial. Default 8 seconds.
	PerDialTimeout time.Duration
}

func (opts *DHTOptions) applyDefaults() {
	if opts.RefreshInterval == 0 {
		opts.RefreshInterval = 5 * time.Minute
	}
	if opts.TargetPeers == 0 {
		opts.TargetPeers = 30
	}
	if opts.ClosestWalkInterval == 0 {
		opts.ClosestWalkInterval = 5 * time.Minute
	}
	if opts.DialWalkInterval == 0 {
		opts.DialWalkInterval = 10 * time.Minute
	}
	if opts.MaxDialsPerCycle == 0 {
		opts.MaxDialsPerCycle = 25
	}
	if opts.PerDialTimeout == 0 {
		opts.PerDialTimeout = 8 * time.Second
	}
}

// EnableDHT starts a Kademlia DHT in client mode on this host and runs a
// background refresh loop + V1.2.1 discovery walks. Safe to call once
// per Host.
//
// The DHT itself is exposed via Host.kdht; most callers don't need it.
// The peer count is observable via Host.PeerCount().
func (h *Host) EnableDHT(ctx context.Context, opts DHTOptions) error {
	h.mu.Lock()
	if h.kdht != nil {
		h.mu.Unlock()
		return fmt.Errorf("DHT already enabled")
	}
	h.mu.Unlock()
	opts.applyDefaults()

	// Client mode: we participate in queries but don't serve them.
	d, err := dht.New(ctx, h.H,
		dht.Mode(dht.ModeClient),
		dht.ProtocolPrefix("/fil"),
	)
	if err != nil {
		return fmt.Errorf("dht.New: %w", err)
	}

	// Wire bootstrap peers into the DHT routing table.
	for _, p := range opts.BootstrapPeers {
		ai, err := parseAddrInfo(p)
		if err != nil {
			continue
		}
		// Connect first (DHT needs an open stream to populate the
		// routing table), then let DHT bootstrap discover the rest.
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
		_ = h.H.Connect(dctx, ai)
		dcancel()
	}
	if err := d.Bootstrap(ctx); err != nil {
		// Bootstrap is best-effort; log + continue.
		fmt.Printf("libp2p: dht.Bootstrap returned %v (continuing)\n", err)
	}

	h.mu.Lock()
	h.kdht = d
	h.mu.Unlock()

	// Background loops: refresh + V1.2.1 discovery walks. All loops
	// share one derived context so host.Close cancels them cleanly.
	refreshCtx, cancel := context.WithCancel(ctx)
	h.AddCleanup(cancel)
	go h.dhtRefreshLoop(refreshCtx, opts)
	h.RunDHTDiscovery(refreshCtx, d, opts)
	return nil
}

// RunDHTDiscovery starts the V1.2.1 closest-walk + dial-walk loops on
// an arbitrary DHT instance and returns immediately. Used by EnableDHT
// (client-mode DHT) and by cmd/lantern/beacon (server-mode DHT) so both
// daemon and beacon paths get the same active peer growth.
//
// The loops respect ctx cancellation; pass a context that's cancelled
// when the host is being torn down. Safe to call multiple times if you
// really want overlapping walks (don't).
func (h *Host) RunDHTDiscovery(ctx context.Context, d *dht.IpfsDHT, opts DHTOptions) {
	if d == nil {
		return
	}
	opts.applyDefaults()
	go h.dhtClosestWalkLoop(ctx, d, opts)
	go h.dhtDialWalkLoop(ctx, d, opts)
}

func (h *Host) dhtRefreshLoop(ctx context.Context, opts DHTOptions) {
	tick := time.NewTicker(opts.RefreshInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n := h.PeerCount()
			atomic.StoreInt64(&h.peerHWM, int64(n))
			if n < opts.TargetPeers {
				// Re-bootstrap: dial bootstrap peers + ask DHT to
				// refresh.
				for _, p := range opts.BootstrapPeers {
					ai, err := parseAddrInfo(p)
					if err != nil {
						continue
					}
					dctx, dcancel := context.WithTimeout(ctx, 8*time.Second)
					_ = h.H.Connect(dctx, ai)
					dcancel()
				}
				if h.kdht != nil {
					rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
					_ = h.kdht.Bootstrap(rctx)
					rcancel()
				}
			}
		}
	}
}

// dhtClosestWalkLoop is the V1.2.1 "fast" discovery loop. Every
// ClosestWalkInterval (default 5min) we run dht.GetClosestPeers(self),
// which forces a Kademlia walk and seeds the routing table with peers
// from across the swarm. The result is discarded; the side-effect is
// what we want.
//
// This is the cheapest way to populate the routing table without
// implementing a discovery protocol of our own.
func (h *Host) dhtClosestWalkLoop(ctx context.Context, d *dht.IpfsDHT, opts DHTOptions) {
	// First walk runs after a short delay so the bootstrap connect
	// pass above has time to settle. Without the delay, the first walk
	// often runs against an empty routing table and returns nothing.
	startDelay := 30 * time.Second
	if opts.ClosestWalkInterval < startDelay {
		startDelay = opts.ClosestWalkInterval / 2
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(startDelay):
	}

	tick := time.NewTicker(opts.ClosestWalkInterval)
	defer tick.Stop()
	h.runClosestWalk(ctx, d)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.runClosestWalk(ctx, d)
		}
	}
}

func (h *Host) runClosestWalk(ctx context.Context, d *dht.IpfsDHT) {
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Use our own peer ID as the query key. The walk traverses the
	// network towards self; every node along the way gets added to
	// our routing table, populating it with up to K*alpha peers per
	// walk depending on swarm density.
	peers, err := d.GetClosestPeers(wctx, string(h.H.ID()))
	rt := d.RoutingTable().Size()
	if err != nil {
		fmt.Printf("libp2p[dht]: closest-walk peers=%d rt_size=%d connected=%d err=%v\n",
			len(peers), rt, h.PeerCount(), err)
		return
	}
	fmt.Printf("libp2p[dht]: closest-walk peers=%d rt_size=%d connected=%d\n",
		len(peers), rt, h.PeerCount())
}

// dhtDialWalkLoop is the V1.2.1 "slow" discovery loop. Every
// DialWalkInterval we read the DHT routing table, filter out peers we
// already have a connection to, and concurrently dial up to
// MaxDialsPerCycle of the rest. Capped by the host's connmgr
// high-water-mark so we don't fight the trim path.
func (h *Host) dhtDialWalkLoop(ctx context.Context, d *dht.IpfsDHT, opts DHTOptions) {
	startDelay := time.Minute
	if opts.DialWalkInterval < startDelay {
		startDelay = opts.DialWalkInterval / 2
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(startDelay):
	}

	tick := time.NewTicker(opts.DialWalkInterval)
	defer tick.Stop()
	h.runDialWalk(ctx, d, opts)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.runDialWalk(ctx, d, opts)
		}
	}
}

func (h *Host) runDialWalk(ctx context.Context, d *dht.IpfsDHT, opts DHTOptions) {
	// Stop early if we're already above MaxPeers — connmgr will trim
	// fresh connections anyway, no point creating churn.
	if hwm := h.MaxPeers(); hwm > 0 && h.PeerCount() >= hwm {
		fmt.Printf("libp2p[dht]: dial-walk skipped (connected=%d >= hwm=%d)\n",
			h.PeerCount(), hwm)
		return
	}

	connected := make(map[peer.ID]struct{}, 64)
	for _, p := range h.H.Network().Peers() {
		connected[p] = struct{}{}
	}

	candidates := d.RoutingTable().ListPeers()
	// Filter and cap.
	toDial := make([]peer.ID, 0, opts.MaxDialsPerCycle)
	for _, p := range candidates {
		if _, ok := connected[p]; ok {
			continue
		}
		if h.H.Network().Connectedness(p) == network.Connected {
			continue
		}
		toDial = append(toDial, p)
		if len(toDial) >= opts.MaxDialsPerCycle {
			break
		}
	}

	dialed := 0
	for _, pid := range toDial {
		ai := h.H.Peerstore().PeerInfo(pid)
		if len(ai.Addrs) == 0 {
			continue
		}
		dctx, dcancel := context.WithTimeout(ctx, opts.PerDialTimeout)
		if err := h.H.Connect(dctx, ai); err == nil {
			dialed++
		}
		dcancel()
		// Stop if we cross the high-water mid-cycle.
		if hwm := h.MaxPeers(); hwm > 0 && h.PeerCount() >= hwm {
			break
		}
	}

	fmt.Printf("libp2p[dht]: dial-walk candidates=%d dialed=%d connected=%d rt_size=%d\n",
		len(toDial), dialed, h.PeerCount(), d.RoutingTable().Size())
}

// PeerHighWaterMark returns the peer count observed at the last refresh
// tick. Useful for `lantern info`'s observability output.
func (h *Host) PeerHighWaterMark() int {
	return int(atomic.LoadInt64(&h.peerHWM))
}

// parseAddrInfo turns a multiaddr string into a peer.AddrInfo.
func parseAddrInfo(p string) (peer.AddrInfo, error) {
	ma, err := multiaddr.NewMultiaddr(p)
	if err != nil {
		return peer.AddrInfo{}, err
	}
	ai, err := peer.AddrInfoFromP2pAddr(ma)
	if err != nil {
		return peer.AddrInfo{}, err
	}
	return *ai, nil
}

// _hostType ensures the dependency is visible. host.Host is used
// implicitly via h.H above.
var _ host.Host = (host.Host)(nil)
