// dhtBeaconProvider: PeerProvider implementation that maintains a refreshed
// pool of Lantern beacons via the DHT rendezvous discovery the beacon
// already runs. Used by SwarmCertSource in startCertExch.
//
// Issue #6.

package main

import (
	"context"
	"strings"
	"sync"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/multiformats/go-multiaddr"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/f3/subscriber"
)

// dhtBeaconProvider snapshots a periodically-refreshed pool of Lantern
// beacons discovered via the DHT rendezvous + any operator-pinned peers
// supplied via --certexch-peers. Goroutine-safe.
type dhtBeaconProvider struct {
	mu     sync.RWMutex
	pinned []peer.AddrInfo
	dyn    []peer.AddrInfo
	selfID peer.ID // skip self when listing peers
}

// Peers returns a copy of the current pool, with the operator-pinned set
// always at the head so they're tried first by SwarmCertSource's
// round-robin rotation.
func (p *dhtBeaconProvider) Peers() []peer.AddrInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]peer.AddrInfo, 0, len(p.pinned)+len(p.dyn))
	for _, ai := range p.pinned {
		if ai.ID == p.selfID {
			continue
		}
		out = append(out, ai)
	}
	for _, ai := range p.dyn {
		if ai.ID == p.selfID {
			continue
		}
		out = append(out, ai)
	}
	return out
}

// setDynamic replaces the dynamically-discovered pool. Pinned peers are
// untouched. Security #59: the dynamic (DHT-discovered, untrusted-origin)
// pool is capped at build.MaxDynamicBeaconPeers so a rendezvous flood can't
// crowd the rotation unboundedly. The trusted floor + operator pins always
// sit ahead of the dynamic pool in Peers() and are never evicted.
func (p *dhtBeaconProvider) setDynamic(peers []peer.AddrInfo) {
	if len(peers) > build.MaxDynamicBeaconPeers {
		peers = peers[:build.MaxDynamicBeaconPeers]
	}
	p.mu.Lock()
	p.dyn = peers
	p.mu.Unlock()
}

// buildBeaconPeerProvider constructs the PeerProvider used by SwarmCertSource.
// It:
//   - parses operator-pinned multiaddrs from cfg.pinnedPeers
//   - kicks off a background DHT rendezvous discovery loop refreshed every
//     60 seconds when announceEnabled is true and rendezvousKDHT is set
//
// When the DHT isn't available, the provider falls back to whatever
// pinned set was supplied (possibly empty, in which case SwarmCertSource
// degrades cleanly to its fallback).
func buildBeaconPeerProvider(ctx context.Context, cfg certExchConfig) subscriber.PeerProvider {
	prov := &dhtBeaconProvider{}

	// Security #59: seed the built-in trusted beacon floor first so a fresh
	// node has an honest cert-exchange source before DHT discovery warms and
	// can't be fully eclipsed by a hostile rendezvous flood. Parsed the same
	// way as operator pins; both sit ahead of dynamic peers and are never
	// evicted. (Empty by default today; see build.DefaultBeaconPeers.)
	for _, ma := range cfg.network.BeaconPeers() {
		ma = strings.TrimSpace(ma)
		if ma == "" {
			continue
		}
		mAddr, err := multiaddr.NewMultiaddr(ma)
		if err != nil {
			continue
		}
		ai, err := peer.AddrInfoFromP2pAddr(mAddr)
		if err != nil {
			continue
		}
		prov.pinned = append(prov.pinned, *ai)
	}

	// Parse pinned peers from --certexch-peers, comma-separated.
	if strings.TrimSpace(cfg.pinnedPeers) != "" {
		for _, ma := range strings.Split(cfg.pinnedPeers, ",") {
			ma = strings.TrimSpace(ma)
			if ma == "" {
				continue
			}
			mAddr, err := multiaddr.NewMultiaddr(ma)
			if err != nil {
				continue
			}
			ai, err := peer.AddrInfoFromP2pAddr(mAddr)
			if err != nil {
				continue
			}
			prov.pinned = append(prov.pinned, *ai)
		}
	}

	// Dynamic discovery via DHT rendezvous. The beacon advertises itself
	// under BeaconRendezvous; FindPeers gives us everyone else doing the
	// same. Refresh every 60s so the pool stays current.
	if cfg.rendezvousKDHT != nil && cfg.announceEnabled {
		go runRendezvousDiscovery(ctx, cfg.rendezvousKDHT, prov)
	}

	return prov
}

// runRendezvousDiscovery polls the DHT rendezvous for Lantern beacons and
// updates the provider's dynamic pool. The first cycle runs after a 15s
// delay so the host's own bootstrap dials + the rendezvous announce have
// had time to land first.
func runRendezvousDiscovery(ctx context.Context, kdht *dht.IpfsDHT, prov *dhtBeaconProvider) {
	const startDelay = 15 * time.Second
	const refreshInterval = 60 * time.Second
	const findTimeout = 10 * time.Second

	select {
	case <-ctx.Done():
		return
	case <-time.After(startDelay):
	}

	rd := routing.NewRoutingDiscovery(kdht)
	tick := time.NewTicker(refreshInterval)
	defer tick.Stop()

	for {
		fctx, cancel := context.WithTimeout(ctx, findTimeout)
		ch, err := rd.FindPeers(fctx, BeaconRendezvous)
		if err == nil {
			peers := make([]peer.AddrInfo, 0, 8)
			for ai := range ch {
				if ai.ID == "" {
					continue
				}
				peers = append(peers, ai)
			}
			prov.setDynamic(peers)
		}
		cancel()

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
