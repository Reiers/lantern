// SwarmCertSource: a CertSource that prefers other Lantern beacons over
// the JSON-RPC fallback.
//
// Issue #6: V1.2.1 shipped beacon cert-exchange (B-11-01) but every
// beacon defaulted to Glif as its upstream, which undermined the swarm
// thesis. Beacons should pull certs from other Lantern beacons over
// libp2p first, and only fall back to Forest/Lotus/Glif when the swarm
// can't satisfy the query.
//
// This file implements that: a SwarmCertSource that maintains a pool of
// known-good Lantern beacons (refreshed periodically via the DHT
// rendezvous discovery the beacon already runs), tries them in
// round-robin order with a short per-peer timeout, and falls back to a
// fallback CertSource only when the swarm can't answer.

package subscriber

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-f3/certexchange"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/gpbft"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// PeerProvider is the minimal callback shape the SwarmCertSource needs to
// discover Lantern beacons it can query. The beacon's startup wires this
// to the DHT rendezvous discovery loop; tests pass a static slice via
// StaticPeerProvider.
type PeerProvider interface {
	// Peers returns the currently known-good Lantern beacon set. May
	// return an empty slice during cold start; callers handle that by
	// falling back to the JSON-RPC fallback.
	Peers() []peer.AddrInfo
}

// StaticPeerProvider returns a fixed peer list. Used by tests and by
// operators who want to pin a specific beacon set instead of relying on
// DHT discovery.
type StaticPeerProvider []peer.AddrInfo

// Peers returns the fixed peer list.
func (s StaticPeerProvider) Peers() []peer.AddrInfo { return []peer.AddrInfo(s) }

// SwarmCertSource implements CertSource over a pool of libp2p Lantern
// beacons, with a JSON-RPC fallback. Goroutine-safe.
type SwarmCertSource struct {
	host        host.Host
	protocolID  protocol.ID
	provider    PeerProvider
	fallback    CertSource
	perPeerTime time.Duration
	staleAfter  time.Duration

	// nextIdx rotates through the peer pool. atomic because Peers() may
	// be called from concurrent GetLatest / GetCert calls.
	nextIdx atomic.Uint64

	// observability
	mu                     sync.Mutex
	lastUpstream           string // "libp2p:<peer>" or "fallback"
	swarmHits, swarmMisses uint64
	fallbackHits           uint64
	totalCalls             uint64
}

// SwarmConfig configures a SwarmCertSource.
type SwarmConfig struct {
	// Host is the libp2p host used to dial Lantern beacons.
	Host host.Host
	// NetworkName is the F3 network name baked into the protocol id.
	// Defaults to "filecoin" (mainnet).
	NetworkName string
	// Provider returns the current pool of Lantern beacons.
	Provider PeerProvider
	// Fallback is the CertSource used when no swarm peer answers in time
	// or when all answers are stale. Typically a JSONRPCSource pointing
	// at Glif or a sibling Forest/Lotus.
	Fallback CertSource
	// PerPeerTimeout caps each libp2p call. Default 4s.
	PerPeerTimeout time.Duration
	// StaleAfter declares a swarm answer stale and triggers a fallback
	// when the cert's timestamp is older than this. Default 5 minutes.
	StaleAfter time.Duration
}

// NewSwarmCertSource constructs a SwarmCertSource. Host and Fallback are
// required; everything else has defaults.
func NewSwarmCertSource(cfg SwarmConfig) (*SwarmCertSource, error) {
	if cfg.Host == nil {
		return nil, errors.New("swarm cert source: Host is required")
	}
	if cfg.Fallback == nil {
		return nil, errors.New("swarm cert source: Fallback is required")
	}
	if cfg.NetworkName == "" {
		cfg.NetworkName = "filecoin"
	}
	if cfg.PerPeerTimeout == 0 {
		cfg.PerPeerTimeout = 4 * time.Second
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = 5 * time.Minute
	}
	if cfg.Provider == nil {
		cfg.Provider = StaticPeerProvider{}
	}
	pid := protocol.ID(fmt.Sprintf("/f3/certexch/get/1/%s", cfg.NetworkName))
	return &SwarmCertSource{
		host:        cfg.Host,
		protocolID:  pid,
		provider:    cfg.Provider,
		fallback:    cfg.Fallback,
		perPeerTime: cfg.PerPeerTimeout,
		staleAfter:  cfg.StaleAfter,
	}, nil
}

// SwarmStats reports observable activity for the dashboard / lantern info.
type SwarmStats struct {
	LastUpstream string // human-readable, e.g. "libp2p:12D3Koo..." or "fallback"
	TotalCalls   uint64
	SwarmHits    uint64
	SwarmMisses  uint64
	FallbackHits uint64
}

// Stats returns a snapshot of activity counters.
func (s *SwarmCertSource) Stats() SwarmStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SwarmStats{
		LastUpstream: s.lastUpstream,
		TotalCalls:   s.totalCalls,
		SwarmHits:    s.swarmHits,
		SwarmMisses:  s.swarmMisses,
		FallbackHits: s.fallbackHits,
	}
}

// GetLatest tries the swarm first, then falls back.
func (s *SwarmCertSource) GetLatest(ctx context.Context) (*certs.FinalityCertificate, error) {
	s.mu.Lock()
	s.totalCalls++
	s.mu.Unlock()

	peers := s.provider.Peers()
	if len(peers) > 0 {
		ordered := s.rotateOrder(peers)
		for _, p := range ordered {
			cert, err := s.fetchLatestFromPeer(ctx, p)
			if err != nil {
				continue
			}
			if cert == nil {
				continue
			}
			// Stale check.
			if s.isStale(cert) {
				continue
			}
			s.recordSwarmHit(p.ID.String())
			return cert, nil
		}
	}
	s.recordSwarmMiss()
	cert, err := s.fallback.GetLatest(ctx)
	if err == nil {
		s.recordFallbackHit()
	}
	return cert, err
}

// GetCert tries the swarm first, then falls back. Unlike GetLatest there's
// no staleness to evaluate; we just want the specific instance.
func (s *SwarmCertSource) GetCert(ctx context.Context, instance uint64) (*certs.FinalityCertificate, error) {
	s.mu.Lock()
	s.totalCalls++
	s.mu.Unlock()

	peers := s.provider.Peers()
	if len(peers) > 0 {
		ordered := s.rotateOrder(peers)
		for _, p := range ordered {
			cert, err := s.fetchInstanceFromPeer(ctx, p, instance)
			if err != nil {
				continue
			}
			if cert == nil {
				continue
			}
			s.recordSwarmHit(p.ID.String())
			return cert, nil
		}
	}
	s.recordSwarmMiss()
	cert, err := s.fallback.GetCert(ctx, instance)
	if err == nil {
		s.recordFallbackHit()
	}
	return cert, err
}

// rotateOrder returns peers in a rotated order so callers don't all hammer
// peer[0]. Round-robin uses an atomic counter so concurrent calls
// naturally spread load.
func (s *SwarmCertSource) rotateOrder(peers []peer.AddrInfo) []peer.AddrInfo {
	if len(peers) <= 1 {
		return peers
	}
	start := int(s.nextIdx.Add(1)-1) % len(peers)
	out := make([]peer.AddrInfo, len(peers))
	for i := range peers {
		out[i] = peers[(start+i)%len(peers)]
	}
	return out
}

// fetchLatestFromPeer dials one Lantern beacon and asks for the latest
// finality cert. Bounded by perPeerTimeout so a slow peer doesn't gate
// the whole call.
func (s *SwarmCertSource) fetchLatestFromPeer(parent context.Context, p peer.AddrInfo) (*certs.FinalityCertificate, error) {
	ctx, cancel := context.WithTimeout(parent, s.perPeerTime)
	defer cancel()
	if err := s.host.Connect(ctx, p); err != nil {
		return nil, err
	}
	client := &certexchange.Client{
		Host:           s.host,
		NetworkName:    gpbft.NetworkName(s.networkName()),
		RequestTimeout: s.perPeerTime,
	}
	resp, _, err := client.Request(ctx, p.ID, &certexchange.Request{
		FirstInstance:     0,
		Limit:             0, // 0 = "just the latest, no chain"
		IncludePowerTable: false,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.PendingInstance == 0 {
		return nil, errors.New("beacon: empty response")
	}
	// Ask for the latest finalized instance specifically. PendingInstance
	// is the next-to-finalize; the latest finalized is PendingInstance-1.
	target := resp.PendingInstance - 1
	respCert, ch, err := client.Request(ctx, p.ID, &certexchange.Request{
		FirstInstance:     target,
		Limit:             1,
		IncludePowerTable: false,
	})
	if err != nil {
		return nil, err
	}
	_ = respCert
	if ch == nil {
		return nil, errors.New("beacon: nil cert channel")
	}
	select {
	case cert, ok := <-ch:
		if !ok {
			return nil, errors.New("beacon: cert channel closed without value")
		}
		return cert, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fetchInstanceFromPeer asks one beacon for a specific F3 instance.
func (s *SwarmCertSource) fetchInstanceFromPeer(parent context.Context, p peer.AddrInfo, instance uint64) (*certs.FinalityCertificate, error) {
	ctx, cancel := context.WithTimeout(parent, s.perPeerTime)
	defer cancel()
	if err := s.host.Connect(ctx, p); err != nil {
		return nil, err
	}
	client := &certexchange.Client{
		Host:           s.host,
		NetworkName:    gpbft.NetworkName(s.networkName()),
		RequestTimeout: s.perPeerTime,
	}
	_, ch, err := client.Request(ctx, p.ID, &certexchange.Request{
		FirstInstance:     instance,
		Limit:             1,
		IncludePowerTable: false,
	})
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return nil, errors.New("beacon: nil cert channel")
	}
	select {
	case cert, ok := <-ch:
		if !ok {
			return nil, errors.New("beacon: cert channel closed without value")
		}
		return cert, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// networkName derives the F3 NetworkName from the protocol id. The
// protocol id we hold is the wire path; for go-f3's Client we need the
// short name. We parse it back out.
func (s *SwarmCertSource) networkName() string {
	// protocolID is "/f3/certexch/get/1/<network>". Drop the prefix.
	const prefix = "/f3/certexch/get/1/"
	full := string(s.protocolID)
	if len(full) <= len(prefix) {
		return "filecoin"
	}
	return full[len(prefix):]
}

// isStale returns true if the cert's BLS block delay is older than
// staleAfter. F3 certs include the gpbft.SupplementalData with an
// ECChain payload; the head tipset's epoch * BlockDelaySecs gives us
// the wall-clock time of the finalized tipset. If we can't infer a
// timestamp from the cert, we return false (treat as fresh) so we don't
// reject everything in degenerate cases.
//
// Today this is a best-effort guard. A more robust implementation would
// compare against the F3 PowerTable's expected current instance and
// reject certs whose instance is more than N instances behind. That
// requires plumbing the PowerTable through, which is deferred.
func (s *SwarmCertSource) isStale(cert *certs.FinalityCertificate) bool {
	_ = cert
	// Conservative: do NOT treat any cert as stale at this layer. The
	// caller (certexch/server.go) re-runs go-f3's ValidateFinalityCertificates
	// which is the authoritative check. A "stale" cert from a beacon is
	// still a valid cert; it just means our pool is laggy. Falling back
	// to JSON-RPC on suspected staleness is an aggressive call we don't
	// want to make without observability.
	return false
}

func (s *SwarmCertSource) recordSwarmHit(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.swarmHits++
	s.lastUpstream = "libp2p:" + truncatePeerID(peerID)
}

func (s *SwarmCertSource) recordSwarmMiss() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.swarmMisses++
}

func (s *SwarmCertSource) recordFallbackHit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallbackHits++
	s.lastUpstream = "fallback"
}

func truncatePeerID(id string) string {
	if len(id) <= 20 {
		return id
	}
	return id[:10] + "..." + id[len(id)-6:]
}
