// Package certexch implements Lantern's F3 cert-exchange responder.
//
// V1.2.0 shipped the bootstrap quorum but Lantern beacons could only
// answer Bitswap requests; they could not respond to F3 cert-exchange
// queries. That left LanternBeaconSource as a stub and the
// KindLanternBeacon class as a placeholder in the trust model.
//
// V1.2.1 (B-11-01) closes that gap. This package:
//
//   - Maintains an in-process go-f3/certstore.Store seeded by Lantern's
//     embedded F3 trust anchor (chain/f3/anchor.Embedded).
//   - Pulls verified finality certificates forward from an upstream
//     Lotus-compatible JSON-RPC cert source (Glif by default).
//   - Wraps go-f3/certexchange.Server so any libp2p peer can ask
//     /f3/certexch/get/1/<networkName> for the latest cert chain.
//
// The responder shares the beacon's existing libp2p host (no separate
// listener, no extra port). It is safe to call concurrently with
// Bitswap and DHT advertising.
//
// All validation is delegated to go-f3 (certs.ValidateFinalityCertificates
// + blssig.VerifierWithKeyOnG1). No CGo, no filecoin-ffi.
package certexch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/filecoin-project/go-f3/certexchange"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/certstore"
	"github.com/filecoin-project/go-f3/gpbft"
	"github.com/filecoin-project/go-f3/manifest"
	datastore "github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/protocol"

	"github.com/Reiers/lantern/chain/f3"
	"github.com/Reiers/lantern/chain/f3/anchor"
	"github.com/Reiers/lantern/chain/f3/subscriber"
)

var log = logging.Logger("lantern/f3/certexch")

// Config configures a Responder.
type Config struct {
	// Host is the libp2p host that will serve cert-exchange streams.
	// Required.
	Host host.Host
	// Anchor is the embedded F3 trust anchor (seeds the initial power
	// table and instance number for the internal certstore). Required.
	Anchor *anchor.Anchor
	// Manifest carries the F3 network name (used for the protocol id).
	// Required.
	Manifest *manifest.Manifest
	// CertSource is the upstream source we pull verified certs from.
	// Typically a subscriber.NewJSONRPCSource(...) pointing at Glif or
	// any other Lotus-compatible RPC that publishes
	// Filecoin.F3GetCertificate / Filecoin.F3GetLatestCertificate.
	// Required.
	CertSource subscriber.CertSource
	// PollInterval is how often we pull new certs from CertSource.
	// Default 30s; cert finalization on mainnet runs ~30s.
	PollInterval time.Duration
	// RequestTimeout is the per-cert-exchange-request deadline.
	// Default 15s.
	RequestTimeout time.Duration
}

// Responder serves F3 cert-exchange over libp2p backed by a local
// certstore populated from an upstream Lotus-compatible JSON-RPC.
type Responder struct {
	cfg     Config
	store   *certstore.Store
	server  *certexchange.Server
	proto   protocol.ID
	cancel  context.CancelFunc
	stopped chan struct{}

	mu    sync.Mutex
	pt    gpbft.PowerEntries // power table to validate the *next* instance
	inst  uint64             // next instance to fetch (== latestStored + 1)
	stats Stats
}

// Stats are responder activity counters, exposed for /metrics.
type Stats struct {
	CertsServed    uint64
	CertsIngested  uint64
	LastInstance   uint64
	LastIngestErr  string
	LastIngestTime time.Time
}

// New builds a Responder. Call Start to begin serving.
func New(cfg Config) (*Responder, error) {
	if cfg.Host == nil {
		return nil, errors.New("certexch: nil Host")
	}
	if cfg.Anchor == nil {
		return nil, errors.New("certexch: nil Anchor")
	}
	if cfg.Manifest == nil {
		return nil, errors.New("certexch: nil Manifest")
	}
	if cfg.CertSource == nil {
		return nil, errors.New("certexch: nil CertSource")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	r := &Responder{
		cfg:     cfg,
		stopped: make(chan struct{}),
		proto:   certexchange.FetchProtocolName(cfg.Manifest.NetworkName),
	}
	return r, nil
}

// ProtocolID is the libp2p protocol the responder listens on (e.g.
// /f3/certexch/get/1/filecoin).
func (r *Responder) ProtocolID() protocol.ID { return r.proto }

// Start opens the certstore, registers the cert-exchange stream handler
// on the configured host, and spawns the poll-and-ingest goroutine. It
// blocks only long enough to bootstrap the initial state; certs are
// pulled asynchronously after Start returns.
//
// Start is safe to call once per Responder. Call Stop to release
// resources.
func (r *Responder) Start(ctx context.Context) error {
	pt, err := r.cfg.Anchor.PowerTable()
	if err != nil {
		return fmt.Errorf("anchor power table: %w", err)
	}
	if len(pt.Entries) == 0 {
		return errors.New("anchor power table is empty")
	}

	// Use an in-memory datastore; the cert chain is replayed on each
	// beacon restart from the upstream cert source, which is cheaper
	// than persisting a separate copy. Wrap in MutexDatastore so the
	// certstore's concurrent reads (from inbound cert-exchange streams)
	// don't race against the ingest goroutine's writes.
	ds := dsync.MutexWrap(datastore.NewMapDatastore())

	store, err := certstore.OpenOrCreateStore(ctx, ds, r.cfg.Anchor.Instance, pt.Entries)
	if err != nil {
		return fmt.Errorf("certstore open: %w", err)
	}
	r.store = store

	r.mu.Lock()
	r.pt = pt.Entries
	r.inst = r.cfg.Anchor.Instance
	r.mu.Unlock()

	r.server = &certexchange.Server{
		Host:           r.cfg.Host,
		NetworkName:    r.cfg.Manifest.NetworkName,
		Store:          r.store,
		RequestTimeout: r.cfg.RequestTimeout,
	}

	// certexchange.Server.Start registers a stream handler under the
	// FetchProtocolName protocol id; it does NOT block on the host.
	if err := r.server.Start(ctx); err != nil {
		return fmt.Errorf("certexchange server start: %w", err)
	}

	pollCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.pollLoop(pollCtx)

	return nil
}

// Stop unregisters the stream handler and stops the poll loop. Idempotent.
func (r *Responder) Stop(ctx context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	select {
	case <-r.stopped:
	case <-time.After(2 * time.Second):
		// Best effort; the server stop below also tears down handlers.
	}
	if r.server != nil {
		_ = r.server.Stop(ctx)
	}
	return nil
}

// Store returns the responder's certstore. Exposed for tests that
// want to directly seed verified certs without running the ingest
// loop; production code should not call this.
func (r *Responder) Store() *certstore.Store { return r.store }

// Stats returns a snapshot of responder activity counters.
func (r *Responder) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.stats
	if latest := r.store.Latest(); latest != nil {
		out.LastInstance = latest.GPBFTInstance
	}
	return out
}

// pollLoop pulls verified certs forward from the upstream JSON-RPC
// source and writes them to the local certstore. Runs until ctx is
// cancelled.
func (r *Responder) pollLoop(ctx context.Context) {
	defer close(r.stopped)
	// Try an immediate ingest so the responder reports a live instance
	// promptly after Start, instead of having to wait one PollInterval.
	r.ingestOnce(ctx)
	t := time.NewTicker(r.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ingestOnce(ctx)
		}
	}
}

// ingestOnce pulls one batch of verified certs from the upstream
// source. Logs and records errors instead of failing the whole loop;
// the responder is tolerant of upstream flakiness because the local
// certstore continues to serve whatever it already has.
func (r *Responder) ingestOnce(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()

	latest, err := r.cfg.CertSource.GetLatest(cctx)
	if err != nil {
		r.recordErr(fmt.Errorf("get latest: %w", err))
		return
	}
	if latest == nil {
		r.recordErr(errors.New("nil latest cert"))
		return
	}

	r.mu.Lock()
	startInst := r.inst
	r.mu.Unlock()

	if latest.GPBFTInstance < startInst {
		// Upstream is behind our anchor; nothing to do.
		return
	}

	// Walk forward in small batches so a multi-hour gap doesn't pin a
	// single upstream connection for the whole ingest.
	const batchSize = 50
	for cur := startInst; cur <= latest.GPBFTInstance; {
		bctx, bcancel := context.WithTimeout(ctx, r.cfg.RequestTimeout*2)
		batch := make([]*certs.FinalityCertificate, 0, batchSize)
		for i := uint64(0); i < batchSize && cur+i <= latest.GPBFTInstance; i++ {
			c, err := r.cfg.CertSource.GetCert(bctx, cur+i)
			if err != nil {
				if len(batch) == 0 {
					r.recordErr(fmt.Errorf("get cert %d: %w", cur+i, err))
					bcancel()
					return
				}
				break
			}
			batch = append(batch, c)
		}
		bcancel()
		if len(batch) == 0 {
			return
		}

		r.mu.Lock()
		prevPT := r.pt
		r.mu.Unlock()

		nextInstance, _, newPT, err := f3.VerifyCertChain(r.cfg.Manifest.NetworkName, prevPT, cur, batch)
		if err != nil {
			r.recordErr(fmt.Errorf("verify cert chain at %d (n=%d): %w", cur, len(batch), err))
			return
		}

		for _, c := range batch {
			if err := r.store.Put(ctx, c); err != nil {
				r.recordErr(fmt.Errorf("certstore put %d: %w", c.GPBFTInstance, err))
				return
			}
		}

		r.mu.Lock()
		r.pt = newPT
		r.inst = nextInstance
		r.stats.CertsIngested += uint64(len(batch))
		r.stats.LastIngestTime = time.Now()
		r.stats.LastIngestErr = ""
		r.mu.Unlock()

		log.Debugf("ingested %d certs, now at instance %d", len(batch), nextInstance-1)
		cur = nextInstance
	}
}

func (r *Responder) recordErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.LastIngestErr = err.Error()
	log.Debugf("ingest: %v", err)
}
