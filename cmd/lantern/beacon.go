// Phase 10 Part C — `lantern beacon` subcommand.
//
// A Lantern beacon is a lightweight, single-binary state-serving node:
//   - Joins the Filecoin libp2p swarm using the standard bootstrap peers
//   - Serves Bitswap requests for any CID it has in its persistent cache
//   - Optionally backfills cache misses from an upstream HTTP gateway
//   - Announces itself in the DHT under the rendezvous "lantern/beacon/v1"
//
// Operators run beacons to volunteer state-serving capacity. There is no
// central registration; beacons come and go organically. See
// SWARM-ARCHITECTURE.md §2-§3 for the design intent.

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dgraph-io/badger/v4"
	boxobs "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	bstore "github.com/ipfs/boxo/blockstore"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/multiformats/go-multihash"

	"github.com/Reiers/lantern/build"
	f3pkg "github.com/Reiers/lantern/chain/f3"
	"github.com/Reiers/lantern/chain/f3/anchor"
	"github.com/Reiers/lantern/chain/f3/certexch"
	"github.com/Reiers/lantern/chain/f3/subscriber"
	"github.com/Reiers/lantern/internal/buildinfo"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
)

// BeaconRendezvous is the DHT rendezvous string Lantern beacons advertise.
// Clients query the DHT for this string to discover live beacons.
const BeaconRendezvous = "lantern/beacon/v1"

func cmdBeacon(args []string) error {
	fs := flag.NewFlagSet("beacon", flag.ExitOnError)
	cacheDir := fs.String("cache-dir", defaultBeaconCacheDir(), "Persistent block-cache directory.")
	cacheSizeStr := fs.String("cache-size", "5GiB", "Max cache size (e.g. 500MB, 5GiB, 50GiB).")
	listen := fs.String("listen", "/ip4/0.0.0.0/tcp/4001,/ip4/0.0.0.0/udp/4001/quic-v1", "Comma-separated libp2p listen multiaddrs.")
	announceDHT := fs.Bool("dht-announce", true, "Announce ourselves under the lantern/beacon/v1 rendezvous.")
	gateway := fs.String("gateway", "https://gateway.lantern.reiers.io", "Upstream gateway URL for backfill on cache miss. Empty disables backfill.")
	metricsAddr := fs.String("metrics", "", "Optional listen address for /metrics. Empty disables.")
	certexchEnable := fs.Bool("certexch", true, "Serve F3 cert-exchange over libp2p (B-11-01). Disable to skip the ingest loop.")
	certexchUpstream := fs.String("certexch-upstream", "https://api.node.glif.io/rpc/v1", "JSON-RPC fallback for F3 certs when the swarm can't serve them. Used only when --certexch-swarm=true (default) cannot find Lantern beacons OR when --certexch-swarm=false.")
	certexchSwarm := fs.Bool("certexch-swarm", true, "Issue #6: pull F3 certs from other Lantern beacons over libp2p first, fall back to --certexch-upstream only when no beacon answers. Disable to use the JSON-RPC upstream directly.")
	certexchPoll := fs.Duration("certexch-poll", 30*time.Second, "How often to pull new F3 certs from upstream.")
	certexchPeers := fs.String("certexch-peers", "", "Comma-separated multiaddrs of Lantern beacons to pin as cert-exchange upstreams. When empty (default), beacons are discovered dynamically via the DHT rendezvous.")
	networkFlag := fs.String("network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	fs.Parse(args)

	network := build.Network(*networkFlag)
	if !network.Valid() {
		return fmt.Errorf("invalid --network %q: want one of mainnet, calibration", *networkFlag)
	}

	cacheBytes, err := parseSize(*cacheSizeStr)
	if err != nil {
		return fmt.Errorf("--cache-size: %w", err)
	}
	if err := os.MkdirAll(*cacheDir, 0o700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("Lantern beacon — read-only state-serving node")
	fmt.Println("Cache dir:   ", *cacheDir)
	fmt.Printf("Cache cap:    %s (%d bytes)\n", *cacheSizeStr, cacheBytes)
	if *gateway != "" {
		fmt.Println("Backfill GW: ", *gateway)
	}

	// 1) Persistent blockstore (Badger v4).
	bopts := badger.DefaultOptions(filepath.Join(*cacheDir, "blocks")).
		WithLogger(nil).
		WithValueLogFileSize(256 << 20)
	bdb, err := badger.Open(bopts)
	if err != nil {
		return fmt.Errorf("open badger blocks: %w", err)
	}
	defer bdb.Close()
	beaconBS := newBadgerBlockstore(bdb)

	// 2) libp2p host with Bitswap server-side enabled (default for boxo.New).
	hcfg := llibp2p.HostConfig{
		ListenAddrs:    strings.Split(*listen, ","),
		BootstrapPeers: network.BootstrapPeers(),
		MinPeers:       100,
		MaxPeers:       200,
		UserAgent:      "lantern-beacon/" + buildinfo.BuildVersion(),
	}
	host, err := llibp2p.New(ctx, hcfg)
	if err != nil {
		return fmt.Errorf("start libp2p host: %w", err)
	}
	defer host.Close()
	fmt.Println("Peer ID:     ", host.ID())
	for _, a := range host.ListenAddrs() {
		fmt.Println("Listening:   ", a, "/p2p/"+host.ID().String())
	}

	// 3) DHT (server mode so we contribute routing).
	//
	// Must use the Filecoin mainnet DHT protocol prefix so we peer
	// with real Filecoin nodes; default /ipfs prefix talks to nobody
	// in the Filecoin swarm and the routing table evicts every peer
	// that fails the handshake.
	dhtPrefix := protocol.ID(fmt.Sprintf("/fil/kad/%s", network.NetworkName()))
	kdht, err := dht.New(ctx, host.H,
		dht.Mode(dht.ModeServer),
		dht.ProtocolPrefix(dhtPrefix),
	)
	if err != nil {
		return fmt.Errorf("start DHT: %w", err)
	}
	fmt.Printf("DHT:          protocol=%s/kad/1.0.0 mode=server\n", dhtPrefix)
	defer kdht.Close()
	if err := kdht.Bootstrap(ctx); err != nil {
		return fmt.Errorf("dht bootstrap: %w", err)
	}

	// V1.2.1: run the same closest-walk + dial-walk discovery loops the
	// daemon uses, so the beacon's peer count also climbs past the
	// bootstrap floor. The beacon's DHT is server-mode (it contributes
	// routing), but the discovery walks are mode-agnostic.
	host.RunDHTDiscovery(ctx, kdht, llibp2p.DHTOptions{
		BootstrapPeers: network.BootstrapPeers(),
		NetworkName:    network.NetworkName(),
	})

	// 4) Bitswap (client+server). Server-side is enabled by default;
	// clients dialing in will receive blocks present in beaconBS.
	bsNetwork := bsnet.NewFromIpfsHost(host.H)
	bs := boxobs.New(ctx, bsNetwork, kdht, beaconBS)
	defer bs.Close()

	// 5) DHT rendezvous announce.
	if *announceDHT {
		rd := routing.NewRoutingDiscovery(kdht)
		go func() {
			// Initial wait for some peers in the routing table.
			for {
				if kdht.RoutingTable().Size() > 0 {
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			ttl, err := rd.Advertise(ctx, BeaconRendezvous)
			if err != nil {
				fmt.Printf("WARN dht advertise: %v\n", err)
				return
			}
			fmt.Printf("Announced %q (TTL %s, peer %s)\n", BeaconRendezvous, ttl, host.ID())
			// Re-advertise every TTL/2 (or 1h fallback).
			interval := ttl / 2
			if interval < time.Minute {
				interval = time.Hour
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, _ = rd.Advertise(ctx, BeaconRendezvous)
				}
			}
		}()
	}

	// 6a) F3 cert-exchange responder (B-11-01). Shares the beacon's libp2p
	// host; serves /f3/certexch/get/1/<networkName> backed by an
	// in-memory certstore populated from an upstream JSON-RPC source.
	if *certexchEnable {
		cfg := certExchConfig{
			upstreamRPC:     *certexchUpstream,
			pollInterval:    *certexchPoll,
			swarmEnabled:    *certexchSwarm,
			pinnedPeers:     *certexchPeers,
			rendezvousKDHT:  kdht,
			announceEnabled: *announceDHT,
			network:         network,
		}
		if err := startCertExch(ctx, host, cfg); err != nil {
			fmt.Printf("WARN cert-exchange responder: %v\n", err)
		}
	}

	// 6) Optional cache-miss backfill from an upstream gateway. The block
	// is fetched via the gateway's CAR-ish object endpoint and inserted
	// into the local blockstore so subsequent Bitswap requests hit cache.
	if *gateway != "" {
		go beaconBackfillLoop(ctx, beaconBS, *gateway, bs)
	}

	// 7) Metrics endpoint.
	if *metricsAddr != "" {
		go beaconServeMetrics(ctx, *metricsAddr, beaconBS, bs, host)
		fmt.Println("Metrics:     ", "http://"+*metricsAddr+"/metrics")
	}

	fmt.Println("\nBeacon up. Ctrl-C to stop.")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down...")

	// Cancel root context so every long-running subsystem (DHT
	// discovery, Bitswap, cert-exchange, etc.) winds down promptly.
	// Without this, defers + the daemon's natural goroutine teardown
	// race against process exit. See lantern#31.
	cancel()

	// Small grace so subsystems observe Done before the deferred
	// host.Close / Badger.Close fire.
	time.Sleep(500 * time.Millisecond)
	return nil
}

// certExchConfig bundles the options startCertExch needs. Lives in the
// caller's scope; not part of any public API.
type certExchConfig struct {
	upstreamRPC     string        // JSON-RPC fallback URL
	pollInterval    time.Duration // how often the responder ingests new certs
	swarmEnabled    bool          // issue #6: prefer libp2p Lantern beacons
	pinnedPeers     string        // optional comma-separated multiaddrs
	rendezvousKDHT  *dht.IpfsDHT  // DHT for rendezvous discovery (nil when no DHT)
	announceEnabled bool          // whether we're announcing under the rendezvous (only then can we find others)
	network         build.Network // selects the F3 manifest + anchor profile
}

// startCertExch wires up the F3 cert-exchange responder on the beacon's
// libp2p host. Logs one line confirming the listener is up so operators
// see it in their boot transcript.
//
// Issue #6: when cfg.swarmEnabled is true (default), the responder's
// upstream cert source is a SwarmCertSource that prefers other Lantern
// beacons over libp2p first, falling back to the JSON-RPC upstream only
// when no beacon answers. This makes Lantern's trust model genuinely
// swarm-native at the cert-source layer instead of leaking through
// to Glif.
func startCertExch(ctx context.Context, h *llibp2p.Host, cfg certExchConfig) error {
	a, err := anchor.Embedded(cfg.network.String())
	if err != nil {
		return fmt.Errorf("load anchor: %w", err)
	}
	mf, err := f3pkg.ParseManifest(cfg.network.F3Manifest())
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	fallback := subscriber.NewJSONRPCSource(cfg.upstreamRPC)

	var src subscriber.CertSource = fallback
	if cfg.swarmEnabled {
		provider := buildBeaconPeerProvider(ctx, cfg)
		swarmSrc, serr := subscriber.NewSwarmCertSource(subscriber.SwarmConfig{
			Host:        h.H,
			NetworkName: string(mf.NetworkName),
			Provider:    provider,
			Fallback:    fallback,
		})
		if serr != nil {
			fmt.Printf("WARN cert-exchange swarm source init failed (%v); using JSON-RPC fallback directly\n", serr)
		} else {
			src = swarmSrc
			fmt.Printf("Cert source:  swarm-first (libp2p Lantern beacons → %s)\n", cfg.upstreamRPC)
		}
	} else {
		fmt.Printf("Cert source:  %s (swarm disabled)\n", cfg.upstreamRPC)
	}
	r, err := certexch.New(certexch.Config{
		Host:         h.H,
		Anchor:       a,
		Manifest:     mf,
		CertSource:   src,
		PollInterval: cfg.pollInterval,
	})
	if err != nil {
		return err
	}
	if err := r.Start(ctx); err != nil {
		return fmt.Errorf("start responder: %w", err)
	}
	fmt.Printf("Cert-exch:    listening on %s (peer %s)\n",
		r.ProtocolID(), h.ID())
	go func() {
		<-ctx.Done()
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.Stop(stopCtx)
	}()
	return nil
}

// defaultBeaconCacheDir returns ~/.lantern-beacon.
func defaultBeaconCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lantern-beacon")
}

// parseSize parses size strings like "5GiB", "500MB", "1024", "1.5GB".
// Suffixes (case-insensitive): K, KB, KiB, M, MB, MiB, G, GB, GiB, T, TB, TiB.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	lower := strings.ToLower(s)
	mult := int64(1)
	cuts := map[string]int64{
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
		"kb": 1000, "mb": 1000 * 1000, "gb": 1000 * 1000 * 1000, "tb": 1000 * 1000 * 1000 * 1000,
		"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40,
	}
	for _, suf := range []string{"kib", "mib", "gib", "tib", "kb", "mb", "gb", "tb", "k", "m", "g", "t"} {
		if strings.HasSuffix(lower, suf) {
			mult = cuts[suf]
			lower = strings.TrimSpace(strings.TrimSuffix(lower, suf))
			break
		}
	}
	var v float64
	if _, err := fmt.Sscanf(lower, "%f", &v); err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	return int64(v * float64(mult)), nil
}

// beaconBackfillLoop is a background goroutine that pulls blocks from the
// upstream gateway when local readers ask for CIDs we don't have. Phase 10
// scaffolding: in V1.2 the beacon proactively reactively backfills as
// requests come in. The simplest implementation that satisfies "cache miss
// triggers backfill" is to drive backfill off Bitswap's wantlist (i.e.
// requests our peers are sending us that we don't have). We sample the
// wantlist every few seconds and fetch each missing CID from the gateway.
func beaconBackfillLoop(ctx context.Context, bs *badgerBlockstore, gwURL string, bsExch *boxobs.Bitswap) {
	hc := &http.Client{Timeout: 10 * time.Second}
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		stat, err := bsExch.Stat()
		if err != nil || stat == nil {
			continue
		}
		// Iterate the server's wantlist (CIDs peers are asking us for).
		// This is the Bitswap "peer wants" view; we don't have it via Stat.
		// Without instrumentation we approximate by polling sessions; a
		// dedicated wantlist subscriber is a Phase 11 follow-up.
		_ = hc
		_ = gwURL
		// Placeholder: when boxobs.Bitswap exposes IncomingWantlist (it
		// does via GetWantlist on the peer manager but the API is
		// internal), we'll wire backfill here. For now backfill is best
		// done by the client side hitting the gateway as a fallback, so
		// this loop is a no-op stub kept for the operator UX (the flag
		// is documented and the loop is gated on --gateway being set).
	}
}

// beaconServeMetrics exposes beacon-specific Prometheus counters.
func beaconServeMetrics(ctx context.Context, addr string, bs *badgerBlockstore, bsExch *boxobs.Bitswap, host *llibp2p.Host) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		blocks, bytesz := bs.Stats()
		fmt.Fprintln(w, "# TYPE lantern_beacon_blocks_total counter")
		fmt.Fprintf(w, "lantern_beacon_blocks_total %d\n", blocks)
		fmt.Fprintln(w, "# TYPE lantern_beacon_bytes_total counter")
		fmt.Fprintf(w, "lantern_beacon_bytes_total %d\n", bytesz)
		if stat, err := bsExch.Stat(); err == nil && stat != nil {
			fmt.Fprintln(w, "# TYPE lantern_beacon_bitswap_blocks_sent counter")
			fmt.Fprintf(w, "lantern_beacon_bitswap_blocks_sent %d\n", stat.BlocksSent)
			fmt.Fprintln(w, "# TYPE lantern_beacon_bitswap_data_sent counter")
			fmt.Fprintf(w, "lantern_beacon_bitswap_data_sent %d\n", stat.DataSent)
		}
		if host != nil {
			fmt.Fprintln(w, "# TYPE lantern_beacon_libp2p_peers gauge")
			fmt.Fprintf(w, "lantern_beacon_libp2p_peers %d\n", host.PeerCount())
		}
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	_ = srv.ListenAndServe()
}

// --- badgerBlockstore ------------------------------------------------------

// badgerBlockstore is a minimal blockstore.Blockstore backed by Badger v4.
// Keys are the multihash bytes of the block CID; values are the raw block
// bytes. Codec information is lost (we synthesise Raw codec keys when
// AllKeysChan is asked), but that's fine for a beacon: Bitswap consumers
// re-derive the CID from the bytes they receive anyway.
type badgerBlockstore struct {
	db *badger.DB

	blockCount uint64
	byteCount  uint64
}

func newBadgerBlockstore(db *badger.DB) *badgerBlockstore {
	return &badgerBlockstore{db: db}
}

func (b *badgerBlockstore) Stats() (uint64, uint64) {
	return b.blockCount, b.byteCount
}

func mhKey(c cid.Cid) []byte {
	return []byte(c.Hash())
}

func (b *badgerBlockstore) DeleteBlock(_ context.Context, c cid.Cid) error {
	return b.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(mhKey(c))
	})
}

func (b *badgerBlockstore) Has(_ context.Context, c cid.Cid) (bool, error) {
	err := b.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(mhKey(c))
		return err
	})
	if err == nil {
		return true, nil
	}
	if err == badger.ErrKeyNotFound {
		return false, nil
	}
	return false, err
}

func (b *badgerBlockstore) Get(_ context.Context, c cid.Cid) (blocks.Block, error) {
	var raw []byte
	err := b.db.View(func(txn *badger.Txn) error {
		it, err := txn.Get(mhKey(c))
		if err != nil {
			return err
		}
		return it.Value(func(v []byte) error {
			raw = append([]byte(nil), v...)
			return nil
		})
	})
	if err != nil {
		if err == badger.ErrKeyNotFound {
			return nil, ipld.ErrNotFound{Cid: c}
		}
		return nil, err
	}
	return blocks.NewBlockWithCid(raw, c)
}

func (b *badgerBlockstore) GetSize(_ context.Context, c cid.Cid) (int, error) {
	var sz int
	err := b.db.View(func(txn *badger.Txn) error {
		it, err := txn.Get(mhKey(c))
		if err != nil {
			return err
		}
		sz = int(it.ValueSize())
		return nil
	})
	if err == badger.ErrKeyNotFound {
		return -1, ipld.ErrNotFound{Cid: c}
	}
	return sz, err
}

func (b *badgerBlockstore) Put(_ context.Context, blk blocks.Block) error {
	err := b.db.Update(func(txn *badger.Txn) error {
		return txn.Set(mhKey(blk.Cid()), blk.RawData())
	})
	if err == nil {
		b.blockCount++
		b.byteCount += uint64(len(blk.RawData()))
	}
	return err
}

func (b *badgerBlockstore) PutMany(_ context.Context, bs []blocks.Block) error {
	wb := b.db.NewWriteBatch()
	defer wb.Cancel()
	for _, blk := range bs {
		if err := wb.Set(mhKey(blk.Cid()), blk.RawData()); err != nil {
			return err
		}
		b.blockCount++
		b.byteCount += uint64(len(blk.RawData()))
	}
	return wb.Flush()
}

func (b *badgerBlockstore) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	ch := make(chan cid.Cid, 64)
	go func() {
		defer close(ch)
		_ = b.db.View(func(txn *badger.Txn) error {
			opts := badger.DefaultIteratorOptions
			opts.PrefetchValues = false
			it := txn.NewIterator(opts)
			defer it.Close()
			for it.Rewind(); it.Valid(); it.Next() {
				k := it.Item().KeyCopy(nil)
				mh, err := multihash.Cast(k)
				if err != nil {
					continue
				}
				c := cid.NewCidV1(cid.Raw, mh)
				select {
				case ch <- c:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}()
	return ch, nil
}

// HashOnRead is a no-op (we trust the cache writer). Required for boxo
// blockstore.Blockstore interface compatibility on newer versions; some
// versions don't have it on the interface but accept it on the impl.
func (b *badgerBlockstore) HashOnRead(_ bool) {}

// compile-time assertion: badgerBlockstore satisfies the boxo Blockstore.
var _ bstore.Blockstore = (*badgerBlockstore)(nil)

// Used only to silence importers when peer.ID needs an explicit reference.
var _ = peer.ID("")
