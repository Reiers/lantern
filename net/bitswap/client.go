// Phase 10 Part B — real boxo/bitswap client wired against the Lantern
// libp2p host.
//
// Replaces the Phase 2 stub. The previous behaviour (always-error) made
// net/combined fall straight through to the HTTP gateway. After Phase 10
// the fetch order is:
//
//   1. local Badger cache (state/cache)
//   2. Bitswap from preferred beacons (this package, narrow deadline)
//   3. Bitswap from full swarm (this package, broader deadline)
//   4. HTTP gateway (last resort)
//
// Trust note: every block returned by Bitswap is hashed and the CID is
// checked against the requested CID in net/combined.Fetcher.Get before
// being trusted. Bitswap peers cannot lie undetected; the worst they can
// do is refuse to serve.

package bitswap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"

	boxobs "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	bstore "github.com/ipfs/boxo/blockstore"
	exchange "github.com/ipfs/boxo/exchange"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
)

// noopRouter satisfies routing.ContentDiscovery with a closed channel
// (i.e. "no providers known"). Used when no DHT is wired; Bitswap can still
// broadcast WANT-HAVE to its currently-connected peers without it.
type noopRouter struct{}

func (noopRouter) FindProvidersAsync(_ context.Context, _ cid.Cid, _ int) <-chan peer.AddrInfo {
	ch := make(chan peer.AddrInfo)
	close(ch)
	return ch
}

// Config controls Bitswap wiring. All fields optional except Host.
type Config struct {
	// Host is the libp2p host Bitswap rides on top of. Required.
	Host host.Host
	// ProviderFinder optionally feeds Bitswap with provider hints. When
	// nil (no DHT wired), Bitswap broadcasts WANT-HAVE to its connected
	// peer set, which is sufficient when preferred peers are explicitly
	// connected.
	ProviderFinder routing.ContentDiscovery
	// PreferredPeers is the list of always-keep-connected peers tried
	// first. Beacon nodes (Phase 10 Part C) live here.
	PreferredPeers []peer.AddrInfo
	// FastDeadline is how long to wait for preferred peers before
	// escalating to the full swarm. Default 1.5s.
	FastDeadline time.Duration
	// FullDeadline is the total deadline applied when no per-call
	// timeout is set. Default 5s.
	FullDeadline time.Duration
}

// Client is Lantern's Bitswap client. It satisfies state/hamt.BlockGetter
// (the .Get(ctx, cid) ([]byte, error) shape).
type Client struct {
	bs   *boxobs.Bitswap
	host host.Host
	ds   ds.Batching
	bs0  bstore.Blockstore

	preferred []peer.AddrInfo
	fastDL    time.Duration
	fullDL    time.Duration

	// stats
	gotBlocks atomic.Uint64
	misses    atomic.Uint64
	errs      atomic.Uint64
	totalIn   atomic.Uint64 // bytes received

	closeOnce sync.Once
}

// New constructs a Bitswap client and starts dialing PreferredPeers.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Host == nil {
		return nil, errors.New("net/bitswap: Host is required")
	}
	if cfg.FastDeadline <= 0 {
		cfg.FastDeadline = 1500 * time.Millisecond
	}
	if cfg.FullDeadline <= 0 {
		cfg.FullDeadline = 5 * time.Second
	}
	if cfg.ProviderFinder == nil {
		cfg.ProviderFinder = noopRouter{}
	}

	// In-memory blockstore for Bitswap's internal use. We don't reuse
	// Lantern's verified cache here on purpose: the boxo blockstore
	// expects unverified-write semantics from Bitswap's BlockReceived
	// path, and Lantern's cache enforces hash verification. Combined
	// fetcher caches successful results upstream of this client.
	mds := dssync.MutexWrap(ds.NewMapDatastore())
	bs0 := bstore.NewBlockstore(mds)

	network := bsnet.NewFromIpfsHost(cfg.Host)
	bs := boxobs.New(ctx, network, cfg.ProviderFinder, bs0)

	c := &Client{
		bs:        bs,
		host:      cfg.Host,
		ds:        mds,
		bs0:       bs0,
		preferred: cfg.PreferredPeers,
		fastDL:    cfg.FastDeadline,
		fullDL:    cfg.FullDeadline,
	}

	// Best-effort dial of preferred peers. We don't block start on this.
	if len(cfg.PreferredPeers) > 0 {
		go c.dialPreferred(ctx, cfg.PreferredPeers)
	}

	return c, nil
}

// dialPreferred maintains warm connections to PreferredPeers.
func (c *Client) dialPreferred(ctx context.Context, peers []peer.AddrInfo) {
	for _, p := range peers {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = c.host.Connect(dctx, p)
		cancel()
	}
	// Keep them in the peerstore long-term so the connection manager
	// doesn't reap them under load.
	for _, p := range peers {
		c.host.Peerstore().AddAddrs(p.ID, p.Addrs, time.Hour*24)
	}
}

// Get fetches the block by CID using a two-stage deadline: try preferred
// peers fast, escalate to full swarm, then give up. Returns the raw
// block bytes. The caller is responsible for verifying CID == hash(bytes);
// net/combined.Fetcher does this for production callers.
func (c *Client) Get(ctx context.Context, k cid.Cid) ([]byte, error) {
	// Fast stage: only if we have preferred peers.
	if len(c.preferred) > 0 {
		fctx, cancel := context.WithTimeout(ctx, c.fastDL)
		blk, err := c.bs.GetBlock(fctx, k)
		cancel()
		if err == nil && blk != nil {
			c.gotBlocks.Add(1)
			c.totalIn.Add(uint64(len(blk.RawData())))
			return blk.RawData(), nil
		}
	}

	// Full stage.
	deadline := c.fullDL
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < deadline {
			deadline = rem
		}
	}
	if deadline <= 0 {
		c.misses.Add(1)
		return nil, fmt.Errorf("net/bitswap: deadline exceeded before request")
	}
	fctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	blk, err := c.bs.GetBlock(fctx, k)
	if err != nil {
		c.errs.Add(1)
		return nil, fmt.Errorf("net/bitswap: %w", err)
	}
	c.gotBlocks.Add(1)
	c.totalIn.Add(uint64(len(blk.RawData())))
	return blk.RawData(), nil
}

// GetMany is a convenience batch lookup. It opens a single session so the
// HAMT walk-style "N sequential CIDs" pattern shares a warm peer pool.
func (c *Client) GetMany(ctx context.Context, ks []cid.Cid) (map[cid.Cid][]byte, error) {
	out := make(map[cid.Cid][]byte, len(ks))
	session := c.bs.NewSession(ctx)
	type result struct {
		k   cid.Cid
		blk blocks.Block
		err error
	}
	ch := make(chan result, len(ks))
	var wg sync.WaitGroup
	for _, k := range ks {
		wg.Add(1)
		go func(k cid.Cid) {
			defer wg.Done()
			blk, err := session.GetBlock(ctx, k)
			ch <- result{k: k, blk: blk, err: err}
		}(k)
	}
	wg.Wait()
	close(ch)
	for r := range ch {
		if r.err != nil {
			continue
		}
		if r.blk == nil {
			continue
		}
		out[r.k] = r.blk.RawData()
		c.gotBlocks.Add(1)
		c.totalIn.Add(uint64(len(r.blk.RawData())))
	}
	return out, nil
}

// NewSession returns a Bitswap session — useful for HAMT-walk style fetch
// patterns where successive lookups share a warm peer pool.
func (c *Client) NewSession(ctx context.Context) exchange.Fetcher {
	return c.bs.NewSession(ctx)
}

// Stats snapshot.
func (c *Client) Stats() Stats {
	return Stats{
		GotBlocks: c.gotBlocks.Load(),
		Misses:    c.misses.Load(),
		Errors:    c.errs.Load(),
		BytesIn:   c.totalIn.Load(),
	}
}

// Stats is the snapshot shape Client.Stats() returns.
type Stats struct {
	GotBlocks uint64
	Misses    uint64
	Errors    uint64
	BytesIn   uint64
}

// Close releases the Bitswap client.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.bs.Close()
	})
	return err
}
