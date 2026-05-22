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
	mrand "math/rand"
	"sync/atomic"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multiaddr"

	"github.com/Reiers/lantern/build"
)

// DHTOptions configures the Kademlia client.
type DHTOptions struct {
	// BootstrapPeers seeds the DHT routing table. Reuse the libp2p
	// host's bootstrap list for simplicity.
	BootstrapPeers []string
	// NetworkName is the Filecoin network identifier used to construct
	// the DHT protocol ID. Defaults to build.MainnetNetworkName.
	//
	// Why this matters: Filecoin's DHT protocol is
	// /fil/kad/<network>/kad/1.0.0 (e.g. /fil/kad/testnetnet/kad/1.0.0
	// on mainnet). A bare /fil/kad/1.0.0 client cannot peer with any
	// real Filecoin node — the protocol negotiation fails and the peer
	// gets evicted from the routing table. We pass
	// build.MainnetNetworkName here to match what Lotus and Forest
	// advertise.
	NetworkName string
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
	if opts.NetworkName == "" {
		opts.NetworkName = build.MainnetNetworkName
	}
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
	//
	// Protocol ID must match the Filecoin network's DHT protocol:
	// /fil/kad/<networkName>/kad/1.0.0 (e.g. on mainnet:
	// /fil/kad/testnetnet/kad/1.0.0). A naked /fil/kad/1.0.0 talks to
	// nobody and the routing table evicts every peer that fails the
	// handshake. Verified against a live Lotus NetPeerInfo on mainnet:
	// Filecoin nodes advertise /fil/kad/testnetnet/kad/1.0.0.
	prefix := protocol.ID(fmt.Sprintf("/fil/kad/%s", opts.NetworkName))
	d, err := dht.New(ctx, h.H,
		dht.Mode(dht.ModeClient),
		dht.ProtocolPrefix(prefix),
	)
	if err != nil {
		return fmt.Errorf("dht.New: %w", err)
	}
	fmt.Printf("libp2p[dht]: protocol=%s/kad/1.0.0 mode=client\n", prefix)

	// IMPORTANT: dht.New attaches a Notifiee to the host that listens
	// for new connections and runs the DHT protocol handshake against
	// each one. Peers that speak /fil/kad/1.0.0 then get added to the
	// routing table. That works for fresh dials AFTER dht.New has
	// returned, but the libp2p Host's New() above already kicked off
	// connectBootstrap as a goroutine, and several of those bootstrap
	// dials almost always complete BEFORE we get here. h.H.Connect is
	// idempotent: re-calling it on an already-connected peer doesn't
	// re-fire the notifiee, so those bootstrap peers never get into
	// the routing table on their own. We fix this by explicitly
	// pushing every currently-connected peer (and every bootstrap peer
	// as we re-dial it below) into the routing table.
	seedRoutingTable(ctx, h, d, opts.BootstrapPeers)

	if err := d.Bootstrap(ctx); err != nil {
		// Bootstrap is best-effort; log + continue.
		fmt.Printf("libp2p: dht.Bootstrap returned %v (continuing)\n", err)
	}
	fmt.Printf("libp2p[dht]: routing table seeded rt_size=%d connected=%d\n",
		d.RoutingTable().Size(), h.PeerCount())

	h.mu.Lock()
	h.kdht = d
	h.dhtOpts = opts
	h.dhtOptsOK.Store(true)
	h.mu.Unlock()

	// Background loops: refresh + V1.2.1 discovery walks. All loops
	// share one derived context so host.Close cancels them cleanly.
	refreshCtx, cancel := context.WithCancel(ctx)
	h.AddCleanup(cancel)
	go h.dhtRefreshLoop(refreshCtx, opts)
	go h.keepaliveLoop(refreshCtx, opts)
	h.RunDHTDiscovery(refreshCtx, d, opts)
	return nil
}

// KeepaliveStats reports observable activity from the keepalive loop.
// Exposed so the dashboard / lantern info can show whether the loop is
// actively topping up peer count.
type KeepaliveStats struct {
	Cycles        uint64 // total keepalive ticks
	Triggered     uint64 // cycles where we were below MinPeers and acted
	BootstrapDial uint64 // cumulative bootstrap-peer dials
	RoutingDial   uint64 // cumulative routing-table-walk dials
	// Stuck: cumulative count of peers we dialed on the previous tick
	// that were NOT still connected when the next tick fired. High Stuck
	// values relative to RoutingDial indicate peers are accepting the
	// libp2p stream then closing the connection. That's the failure mode
	// the issue #9 follow-up was diagnosed against.
	Stuck uint64
	// ClosestWalks fired by the keepalive (aggressive, only when peer
	// count is below MinPeers/2). Separate from the periodic 5-minute
	// dhtClosestWalkLoop.
	ClosestWalks  uint64
	LastPeerCount int // peer count observed at the last tick
}

// KeepaliveStats returns a snapshot of keepalive activity counters.
func (h *Host) KeepaliveStats() KeepaliveStats {
	return KeepaliveStats{
		Cycles:        h.kaCycles.Load(),
		Triggered:     h.kaTriggered.Load(),
		BootstrapDial: h.kaBootDial.Load(),
		RoutingDial:   h.kaRouteDial.Load(),
		Stuck:         h.kaStuck.Load(),
		ClosestWalks:  h.kaClosestWalks.Load(),
		LastPeerCount: int(h.kaLastCount.Load()),
	}
}

// TriggerKeepalive runs a single keepalive cycle synchronously and returns
// the peer count observed before and after. Issue #14 exposes this so the
// dashboard 'Find more peers' button can manually fire the loop instead of
// waiting up to 30s for the periodic tick.
//
// Safe to call from HTTP handlers: bounded by the ctx timeout, no
// long-running side effects, no goroutine leaks. Returns the same
// counters runKeepalive bumps so the caller can include them in the
// response.
func (h *Host) TriggerKeepalive(ctx context.Context) (before, after int, err error) {
	if !h.dhtOptsOK.Load() {
		return 0, 0, fmt.Errorf("DHT not enabled")
	}
	h.mu.Lock()
	opts := h.dhtOpts
	h.mu.Unlock()

	before = h.PeerCount()
	const maxRoutingDials = 15 // a bit more aggressive than the periodic loop
	min := h.MinPeers()
	if min == 0 {
		min = 30
	}
	// runKeepalive's first action is to audit previous-tick dials and to
	// no-op when count >= min. For the manual trigger we want to do work
	// even when we're already at MinPeers, since the operator clicked the
	// button for a reason. Bypass the >=min gate by passing min+1.
	h.runKeepalive(ctx, opts, min+1, maxRoutingDials)
	after = h.PeerCount()
	return before, after, nil
}

// keepaliveLoop is the 30s tight redial loop that maintains MinPeers.
//
// Issue #9: the 5-minute dhtRefreshLoop is too slow to catch decay; peer
// count routinely drifted from 50 down to ~15 within a few minutes after
// boot. This loop fires every 30s. When the count is below MinPeers, it
// (a) re-dials all bootstrap peers, then (b) walks the DHT routing
// table and dials up to N=10 unconnected peers, and (c) pushes
// everything that successfully connects back into the routing table.
//
// When at or above MinPeers, the loop does nothing -- no churn, no dials,
// no log spam. connmgr's grace period protects fresh connections from
// being immediately re-trimmed.
func (h *Host) keepaliveLoop(ctx context.Context, opts DHTOptions) {
	const keepaliveInterval = 30 * time.Second
	const maxRoutingDialsPerCycle = 10

	// Wait a short while after boot before starting; gives the host's
	// own bootstrap dials a chance to land first.
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}

	min := h.MinPeers()
	if min <= 0 {
		min = 50 // defensive default; should never hit because Host.MinPeers always returns the configured value
	}

	tick := time.NewTicker(keepaliveInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.runKeepalive(ctx, opts, min, maxRoutingDialsPerCycle)
		}
	}
}

// runKeepalive is one tick of the keepalive loop, broken out for clarity
// and testability.
//
// Issue #9 follow-up: four behavioural changes from the original 30s loop:
//
//  1. Before doing any work, audit peers we dialed on the previous tick.
//     Any that aren't connected now bump kaStuck. This is the diagnosis
//     counter the follow-up plan called for: "did the dial actually
//     result in a stable connection?"
//
//  2. Skip peers we attempted to dial in the last 5 minutes. libp2p has
//     per-peer dial backoff but it doesn't compose well across multiple
//     candidate lists, and we were re-trying the same dead peers every
//     30s. The per-peer cooldown saves dial budget for fresh candidates.
//
//  3. Walk the routing table in randomized order each tick instead of
//     ListPeers() insertion order. Routing tables tend to put the
//     longest-known peers first, which often correlates with peers that
//     have been bouncing the longest. Randomizing fans the dial budget
//     across the whole table.
//
//  4. When count drops below MinPeers/2, fire a GetClosestPeers walk
//     against a random key (NOT self) to force the routing table to
//     absorb peers from a different region of the keyspace. This is
//     independent of the periodic 5-minute dhtClosestWalkLoop.
func (h *Host) runKeepalive(ctx context.Context, opts DHTOptions, minPeers, maxRoutingDials int) {
	h.kaCycles.Add(1)
	count := h.PeerCount()
	h.kaLastCount.Store(int64(count))

	// (1) Audit last cycle's dials for stickiness.
	h.auditPreviousDials()

	if count >= minPeers {
		return // healthy, no work to do
	}
	h.kaTriggered.Add(1)

	dialedThisTick := make(map[peer.ID]struct{}, maxRoutingDials+8)

	// a) Re-dial bootstrap peers. Cheap, bounded (~7 dials).
	for _, p := range opts.BootstrapPeers {
		ai, err := parseAddrInfo(p)
		if err != nil {
			continue
		}
		// Skip if already connected.
		if h.H.Network().Connectedness(ai.ID) == network.Connected {
			continue
		}
		// Bootstrap peers don't go through the recent-attempt cooldown:
		// they're our anchor of last resort. If they're refusing us
		// repeatedly the cooldown applied by libp2p's dial machinery is
		// the right backstop.
		dctx, dcancel := context.WithTimeout(ctx, 6*time.Second)
		if err := h.H.Connect(dctx, ai); err == nil {
			h.kaBootDial.Add(1)
			dialedThisTick[ai.ID] = struct{}{}
			h.markDialAttempt(ai.ID)
			if h.kdht != nil {
				_, _ = h.kdht.RoutingTable().TryAddPeer(ai.ID, true, false)
			}
		} else {
			h.markDialAttempt(ai.ID)
		}
		dcancel()
	}

	// b) Walk the routing table for unconnected peers and dial up to
	//    maxRoutingDials of them, in randomized order, skipping anyone
	//    we tried in the last 5 minutes.
	if h.kdht == nil {
		h.savePreviousDialed(dialedThisTick)
		return
	}
	candidates := h.kdht.RoutingTable().ListPeers()
	shufflePeers(candidates)
	now := time.Now()
	const dialCooldown = 5 * time.Minute
	dialed := 0
	for _, pid := range candidates {
		if dialed >= maxRoutingDials {
			break
		}
		// Stop if we crossed the floor mid-walk.
		if h.PeerCount() >= minPeers {
			break
		}
		if h.H.Network().Connectedness(pid) == network.Connected {
			continue
		}
		// Skip recently-attempted peers.
		if last, ok := h.lastDialAttempt(pid); ok && now.Sub(last) < dialCooldown {
			continue
		}
		ai := h.H.Peerstore().PeerInfo(pid)
		if len(ai.Addrs) == 0 {
			continue
		}
		dctx, dcancel := context.WithTimeout(ctx, 6*time.Second)
		if err := h.H.Connect(dctx, ai); err == nil {
			h.kaRouteDial.Add(1)
			dialed++
			dialedThisTick[ai.ID] = struct{}{}
		}
		h.markDialAttempt(pid)
		dcancel()
	}

	// c) Aggressive closest-walk when we're below MinPeers/2. This pulls
	//    in peers from a different region of the keyspace than what's in
	//    the routing table today. Independent of the periodic 5-minute
	//    closest-walk loop.
	if h.PeerCount() < minPeers/2 {
		h.runClosestWalk(ctx, h.kdht)
		h.kaClosestWalks.Add(1)
	}

	h.savePreviousDialed(dialedThisTick)
}

// auditPreviousDials checks every peer we dialed on the previous tick.
// For each one that's no longer connected, increment kaStuck. This is
// the diagnosis counter the follow-up plan called for.
func (h *Host) auditPreviousDials() {
	h.kaPrevDialedMu.Lock()
	prev := h.kaPrevDialed
	h.kaPrevDialedMu.Unlock()
	if len(prev) == 0 {
		return
	}
	var stuck uint64
	for pid := range prev {
		if h.H.Network().Connectedness(pid) != network.Connected {
			stuck++
		}
	}
	if stuck > 0 {
		h.kaStuck.Add(stuck)
	}
}

// savePreviousDialed atomically replaces the previous-cycle dial set.
func (h *Host) savePreviousDialed(s map[peer.ID]struct{}) {
	h.kaPrevDialedMu.Lock()
	h.kaPrevDialed = s
	h.kaPrevDialedMu.Unlock()
}

// markDialAttempt records that we attempted to dial pid right now.
// Used by the recent-attempt cooldown.
func (h *Host) markDialAttempt(pid peer.ID) {
	h.kaLastAttemptMu.Lock()
	defer h.kaLastAttemptMu.Unlock()
	if h.kaLastAttempt == nil {
		h.kaLastAttempt = make(map[peer.ID]time.Time)
	}
	h.kaLastAttempt[pid] = time.Now()
	// Prune entries older than 30 minutes to keep the map bounded.
	// Only do this every ~64 inserts (cheap heuristic) to amortize the cost.
	if len(h.kaLastAttempt) > 64 && len(h.kaLastAttempt)%64 == 0 {
		cutoff := time.Now().Add(-30 * time.Minute)
		for k, t := range h.kaLastAttempt {
			if t.Before(cutoff) {
				delete(h.kaLastAttempt, k)
			}
		}
	}
}

// lastDialAttempt returns (time, true) if we have a recorded dial attempt
// for pid, else (zero, false).
func (h *Host) lastDialAttempt(pid peer.ID) (time.Time, bool) {
	h.kaLastAttemptMu.Lock()
	defer h.kaLastAttemptMu.Unlock()
	if h.kaLastAttempt == nil {
		return time.Time{}, false
	}
	t, ok := h.kaLastAttempt[pid]
	return t, ok
}

// shufflePeers permutes the candidate slice in place. Uses crypto/rand
// via mrand fed by time-seeded source (good enough for a randomization
// of walk order, NOT a security primitive).
func shufflePeers(ps []peer.ID) {
	for i := len(ps) - 1; i > 0; i-- {
		j := mrand.Intn(i + 1)
		ps[i], ps[j] = ps[j], ps[i]
	}
}

// seedRoutingTable explicitly pushes every connected peer plus every
// (parseable) bootstrap peer into the DHT routing table. This is the
// only reliable way to recover from the dht.New-after-connectBootstrap
// ordering race: by the time dht.New attaches its protocol-negotiation
// notifiee, several bootstrap dials have usually already completed, and
// h.H.Connect is idempotent so re-calling it doesn't fire the notifiee.
//
// TryAddPeer is the documented kad-dht entry point for "I know this
// peer speaks the DHT protocol; add it to my routing table." The
// `queryPeer=true` flag marks the peer as known-good for queries; we
// only call this for peers we're confident speak /fil/kad/1.0.0
// (already-connected Filecoin bootstrap nodes).
func seedRoutingTable(ctx context.Context, h *Host, d *dht.IpfsDHT, bootstrapPeers []string) {
	rt := d.RoutingTable()

	// 1) Already-connected peers from the host's network. Most of the
	// connectBootstrap dials land here by the time we get called.
	for _, pid := range h.H.Network().Peers() {
		if _, err := rt.TryAddPeer(pid, true, false); err != nil {
			// TryAddPeer can refuse (table full, peer-too-far, etc.).
			// That's fine; we tried.
			_ = err
		}
	}

	// 2) Bootstrap peers we haven't yet finished dialing. Connect and
	// then TryAddPeer. The Connect is bounded by a per-peer timeout
	// so a slow bootstrap doesn't block startup.
	for _, p := range bootstrapPeers {
		ai, err := parseAddrInfo(p)
		if err != nil {
			continue
		}
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
		if err := h.H.Connect(dctx, ai); err != nil {
			dcancel()
			continue
		}
		dcancel()
		if _, err := rt.TryAddPeer(ai.ID, true, false); err != nil {
			_ = err
		}
	}
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
				// Re-seed: re-dial bootstrap peers AND push them
				// into the routing table. Plain Connect is
				// idempotent so we have to TryAddPeer ourselves;
				// otherwise the routing table stays empty even when
				// the libp2p connection count looks fine.
				if h.kdht != nil {
					seedRoutingTable(ctx, h, h.kdht, opts.BootstrapPeers)
					rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
					_ = h.kdht.Bootstrap(rctx)
					rcancel()
				} else {
					// No DHT (e.g. startup race): just dial.
					for _, p := range opts.BootstrapPeers {
						ai, err := parseAddrInfo(p)
						if err != nil {
							continue
						}
						dctx, dcancel := context.WithTimeout(ctx, 8*time.Second)
						_ = h.H.Connect(dctx, ai)
						dcancel()
					}
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
		// If the table is empty the walk has nothing to walk. Re-seed
		// from already-connected peers and bootstrap multiaddrs so the
		// next tick has something to work with. Cheaper than waiting
		// for the 5-minute refresh loop to figure it out.
		if rt == 0 {
			seedRoutingTable(ctx, h, d, nil)
			fmt.Printf("libp2p[dht]: closest-walk auto-reseed rt_size=%d\n",
				d.RoutingTable().Size())
		}
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
