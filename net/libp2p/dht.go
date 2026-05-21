// Phase 8 Part D — Kademlia DHT for peer discovery.
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

package libp2p

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
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
	// TargetPeers is the number of connected peers we aim for. Once we
	// drop below this number the refresh loop tries harder. Default 30.
	TargetPeers int
}

// EnableDHT starts a Kademlia DHT in client mode on this host and runs a
// background refresh loop. Safe to call once per Host.
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
	if opts.RefreshInterval == 0 {
		opts.RefreshInterval = 5 * time.Minute
	}
	if opts.TargetPeers == 0 {
		opts.TargetPeers = 30
	}

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

	// Background refresh: periodically re-bootstrap if peer count fell
	// below TargetPeers.
	refreshCtx, cancel := context.WithCancel(ctx)
	h.AddCleanup(cancel)
	go h.dhtRefreshLoop(refreshCtx, opts)
	return nil
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
