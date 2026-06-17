// start.go: the wiring that brings a Daemon online.
//
// Status: bootstrap implementation. The real implementation will live
// here once cmd/lantern's cmdDaemon body is extracted into pkg/daemon.
// For now, this file provides the minimum API surface external callers
// (Curio Core, integration tests) need today so they can begin building
// against pkg/daemon while the extraction lands incrementally.
//
// The current implementation:
//
//   - validates Config
//   - fetches a TrustedRoot (the load-bearing first step every Lantern
//     daemon does)
//   - exposes that TrustedRoot via Daemon.TrustedRoot()
//   - holds on lifecycle (Start blocks until ctx is cancelled)
//
// Not yet wired (will land in subsequent commits as cmd/lantern's body
// migrates here):
//
//   - JSON-RPC server (rpc/server.New + chainAPI binding)
//   - libp2p host + DHT + gossipsub block ingestor
//   - Bitswap client
//   - VM bridge for AllowBlockSubmit=true
//   - Header store + sync agent
//   - Metrics + dashboard endpoints
//   - Hello + ChainExchange responders
//
// External callers can start building against `daemon.New(cfg).Start(ctx)`
// today; the runtime that Start exposes will grow over subsequent commits
// without changing the public API.

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/f3/subscriber"
	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/headnotify"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/blockingest"
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/glif"
	"github.com/Reiers/lantern/net/hsync"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/net/mpool"
	"github.com/Reiers/lantern/rpc/handlers"
	rpcserver "github.com/Reiers/lantern/rpc/server"
	"github.com/Reiers/lantern/state/hamt"
	"github.com/Reiers/lantern/state/prefetch"
	"github.com/Reiers/lantern/vm/bridge"
)

var log = logging.Logger("lantern/daemon")

// startInternal is the bring-up sequence: anchor trust + mount the
// JSON-RPC server. The lighter subsystems (libp2p, gossipsub, header
// store, bitswap, metrics, dashboard) remain in cmd/lantern for now.
// Embedded consumers (curio-core) only need the RPC mount to dial
// in-process.
func (d *Daemon) startInternal(ctx context.Context) error {
	tr, err := fetchTrustedHead(ctx, d.cfg.Gateway, build.Network(d.cfg.Network))
	if err != nil {
		return fmt.Errorf("fetch trusted head: %w", err)
	}
	if !d.cfg.EmbeddedMode {
		fmt.Printf("daemon: anchored at epoch %d (state root %s)\n", tr.Epoch, tr.StateRoot)
	}
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()

	// Mount the JSON-RPC server. Skipped only when the operator passed
	// RPCListen="" explicitly (currently no such code path, but cheap
	// to support).
	if d.cfg.RPCListen == "" {
		return nil
	}

	network := build.Network(d.cfg.Network)
	gw := d.cfg.Gateway

	// Combined fetcher: gateway race + glif fallback. Matches the
	// cmd/lantern wiring (without bitswap, since libp2p isn't mounted
	// here yet — the gateway+glif pair covers cold state-tree reads).
	cache := hamt.NewMemBlockStore()
	// glifURL is the RPC fallback for the polling Sync head source AND the
	// gossipsub ingestor's head+N backfill source. Gossipsub is the PRIMARY
	// (Glif-free) head source when libp2p is enabled; this URL is the
	// fallback that keeps head advancing when the gossip mesh is cold or
	// stalls. Previously empty on mainnet, which left the mainnet daemon
	// with no working catch-up source: when gossip stalled, head froze.
	// Callers can override via Config.Gateway / a future Config.FallbackRPC.
	glifURL := "https://api.node.glif.io/rpc/v1"
	if network == build.Calibration {
		glifURL = "https://api.calibration.node.glif.io/rpc/v1"
	}
	fetcher := combined.New(cache,
		combined.Source{Name: "gateway", Getter: hsync.NewClient([]string{gw}, 20*time.Second), Timeout: 5 * time.Second, Race: true},
		combined.Source{Name: "glif", Getter: glif.New(glifURL, 20*time.Second), Timeout: 20 * time.Second},
	)

	chainAPI := handlers.New(tr, fetcher, d.cfg.Wallet, nil, network.String())

	// Persistent header store + head-change distributor + sync.
	//
	// Without this, Filecoin.ChainNotify returns 'method not supported
	// in this mode (no out channel support)' because go-jsonrpc can't
	// stream channels over plain HTTP POST. Embedded consumers reach
	// the distributor directly via Daemon.HeadChanges(); external
	// consumers reach it via ChainNotify over WebSocket (when the RPC
	// server is upgraded — separate workstream).
	//
	// The wiring mirrors cmd/lantern's standalone-daemon path: Badger
	// header store at <data-dir>/<network>/headerstore, distributor
	// fanning subscribers off the store, hstore.Sync polling the same
	// Glif source used as the cold-state fallback fetcher.
	if !d.cfg.NoHeaderStore {
		hsPath := d.cfg.HeaderStorePath
		if hsPath == "" {
			hsPath = filepath.Join(d.cfg.DataDir, network.String(), "headerstore")
		}
		if err := os.MkdirAll(hsPath, 0o700); err != nil {
			return fmt.Errorf("create header store dir: %w", err)
		}
		store, err := hstore.Open(hsPath, hstore.Options{})
		if err != nil {
			return fmt.Errorf("open header store: %w", err)
		}
		chainAPI.HeaderStore = store

		dist := headnotify.New(store, d.cfg.NotifyBufSize)
		dist.Start()
		chainAPI.HeadNotify = dist

		src := glif.New(glifURL, 8*time.Second)

		// #40: when libp2p is enabled, gossipsub is the PRIMARY head
		// source (0-1 epoch latency, no upstream-RPC dependency) and the
		// polling Sync drops to a relaxed cadence as the catch-up fallback,
		// matching standalone cmd/lantern. When libp2p is disabled (the
		// curio-core default today), Sync stays at the configured interval
		// as the sole head source.
		libp2pEnabled := !d.cfg.NoLibp2p && d.cfg.P2PListen != ""
		syncInterval := d.cfg.SyncInterval
		if libp2pEnabled {
			syncInterval = 30 * time.Second
		}

		// Issue #33: the hardened Sync resumes catch-up contiguously from
		// currentHead+1 in CatchUpChunk-sized steps (never skips epochs),
		// and advances head only as far as the chain is gap-free (retrying
		// the rest next poll rather than aborting). MaxBacktrack is raised
		// so reorg/backfill depth comfortably exceeds the deepest realistic
		// single-wait lag.
		sync := hstore.NewSync(store, src, hstore.SyncOptions{
			Interval:       syncInterval,
			MaxBacktrack:   900, // ~7.5h at 30s blocks; covers long proving waits
			BootstrapDepth: 3,
			CatchUpChunk:   200, // bounded per-poll catch-up work
		})
		if err := sync.Start(ctx); err != nil {
			_ = store.Close()
			return fmt.Errorf("start header sync: %w", err)
		}

		d.mu.Lock()
		d.headerStore = store
		d.headerSync = sync
		d.headNotify = dist
		d.mu.Unlock()

		log.Infow("header store wired",
			"path", hsPath,
			"sync_interval", syncInterval,
			"notify_buf", d.cfg.NotifyBufSize,
			"gossipsub", libp2pEnabled,
			"embedded", d.cfg.EmbeddedMode)
		if !d.cfg.EmbeddedMode {
			fmt.Printf("daemon: header store %s (sync every %s, buf=%d)\n",
				hsPath, syncInterval, d.cfg.NotifyBufSize)
		}

		// #40: mount libp2p host + gossipsub block ingestor for live head
		// tracking. Best-effort: a libp2p failure must not sink the daemon
		// (the polling Sync still tracks head). src doubles as the
		// ingestor's bounded inline-backfill source for head+N>1 arrivals.
		if libp2pEnabled {
			if err := d.startGossipHead(ctx, store, src, network, chainAPI, fetcher); err != nil {
				log.Warnw("gossipsub head-tracking unavailable; falling back to polling Sync", "err", err)
			}
		}

		// lantern#44: FEVM contract-state prefetcher. On every head advance,
		// walk the configured EVM contract addresses' bytecode + storage
		// subtree into the local blockstore cache so later eth_calls hit
		// the cache instead of falling back to the bridge. Strictly
		// best-effort: walks run on goroutines, and a failed walk is logged
		// at debug and dropped.
		if len(d.cfg.FEVMPrefetchAddrs) > 0 {
			pf := prefetch.New(prefetch.Config{
				Addrs:            d.cfg.FEVMPrefetchAddrs,
				MaxBlocksPerAddr: d.cfg.FEVMPrefetchMaxBlocksPerAddr,
				PerAddrTimeout:   d.cfg.FEVMPrefetchPerAddrTimeout,
				MinInterval:      d.cfg.FEVMPrefetchMinInterval,
			}, fetcher)
			store.OnHeadChange(func(ts *types.TipSet) {
				pf.Trigger(ctx, ts)
			})
			d.mu.Lock()
			d.fevmPrefetch = pf
			d.mu.Unlock()
			log.Infow("FEVM state prefetcher wired",
				"addrs", len(d.cfg.FEVMPrefetchAddrs),
				"max_blocks_per_addr", d.cfg.FEVMPrefetchMaxBlocksPerAddr,
				"per_addr_timeout", d.cfg.FEVMPrefetchPerAddrTimeout,
				"min_interval", d.cfg.FEVMPrefetchMinInterval,
			)
		}
	}

	// lantern#44: pipe FEVM fetch retry config from daemon.Config to
	// the eth_call backend. Zero values let evmexec.go apply its own
	// sensible defaults; negative retries disables.
	chainAPI.LocalFEVMFetchRetries = d.cfg.FEVMFetchRetries
	chainAPI.LocalFEVMFetchTimeout = d.cfg.FEVMFetchTimeout

	// lantern#44 adaptive warming: when an eth_call misses locally and
	// falls back to the bridge, feed the contract address to the
	// prefetcher so its state is warm on the next head advance. This is
	// what lets the read path reach zero-bridge over a few epochs even
	// for linked contracts (e.g. FilecoinPay) not in the static seed
	// list. No-op if the prefetcher isn't wired.
	if pf := d.fevmPrefetch; pf != nil {
		chainAPI.OnLocalMiss = pf.AddAddr
	}

	// Stash for accessor-style reads (LocalEthCallStats, etc.).
	d.mu.Lock()
	d.chainAPI = chainAPI
	d.mu.Unlock()

	// Optional VM bridge: when configured, FEVM read methods (eth_call,
	// eth_estimateGas) and SendRawTransaction get forwarded to an
	// upstream Forest/Lotus node. Without this, eth_call against any
	// contract returns "FEVM method requires --vm-bridge-rpc".
	if d.cfg.VMBridgeRPC != "" {
		vmBr := bridge.NewForestBridge(d.cfg.VMBridgeRPC, d.cfg.VMBridgeToken, d.cfg.VMBridgeTimeout)
		chainAPI.WithBridge(vmBr)
		chainAPI.AllowBlockSubmit = d.cfg.AllowBlockSubmit
		if !d.cfg.EmbeddedMode {
			fmt.Printf("daemon: vm-bridge %s\n", vmBr.Provenance())
		}
	}

	srv, err := rpcserver.New(rpcserver.Config{
		ListenAddress: d.cfg.RPCListen,
		DataDir:       d.cfg.DataDir,
	}, chainAPI)
	if err != nil {
		return fmt.Errorf("build rpc server: %w", err)
	}
	chainAPI.AuthIssuer = srv.Auth()

	if err := srv.Start(); err != nil {
		return fmt.Errorf("start rpc server: %w", err)
	}

	d.mu.Lock()
	d.rpcServer = srv
	d.auth = srv.Auth()
	d.rpcAddr = srv.Addr().String()
	d.mu.Unlock()

	// Persist the resolved RPC listen address (issue #34) so `lantern
	// info` reports the real port for embedded installs too — important
	// because embedded mode often binds an ephemeral port (:0). Mirrors
	// the standalone daemon's <netDir>/rpc-listen write. Best-effort.
	if d.cfg.DataDir != "" {
		netDir := filepath.Join(d.cfg.DataDir, network.String())
		if err := os.WriteFile(filepath.Join(netDir, "rpc-listen"),
			[]byte(srv.Addr().String()+"\n"), 0o600); err != nil {
			log.Warnw("persist rpc-listen", "err", err)
		}
	}

	if !d.cfg.EmbeddedMode {
		fmt.Printf("daemon: rpc at http://%s/rpc/v1\n", srv.Addr().String())
	}
	return nil
}

// stopInternal performs the shutdown sequence: graceful RPC server
// stop with a bounded timeout, then mark the daemon as stopped.
func (d *Daemon) stopInternal(ctx context.Context) error {
	defer close(d.stopped)
	d.mu.Lock()
	srv := d.rpcServer
	sync := d.headerSync
	store := d.headerStore
	host := d.p2pHost
	bsClient := d.bitswap
	d.rpcServer = nil
	d.auth = nil
	d.headerSync = nil
	d.headerStore = nil
	d.headNotify = nil
	d.p2pHost = nil
	d.ingestor = nil
	d.bitswap = nil
	d.started = false
	d.mu.Unlock()

	// Close bitswap before the host (it rides on the host's network).
	if bsClient != nil {
		_ = bsClient.Close()
	}

	if srv != nil {
		if err := srv.Stop(ctx); err != nil {
			return fmt.Errorf("stop rpc server: %w", err)
		}
	}
	// Close the libp2p host (stops gossipsub + DHT). The ingestor
	// goroutine self-terminates on ctx cancellation. Best-effort.
	if host != nil {
		_ = host.Close()
	}
	// Shut down header-sync before closing the store: Sync writes
	// SetHead asynchronously, and Close on an open Badger handle while
	// a writer is mid-txn corrupts the LSM. Stop is idempotent.
	if sync != nil {
		sync.Stop()
	}
	if store != nil {
		if err := store.Close(); err != nil {
			return fmt.Errorf("close header store: %w", err)
		}
	}
	return nil
}

// startGossipHead brings up the libp2p host + Kademlia DHT discovery +
// the gossipsub block ingestor (#40), so the embedded daemon tracks head
// over /fil/blocks/<network> with the same 0-1 epoch latency as the
// standalone daemon, instead of relying solely on Glif polling.
//
// The ingestor installs gossiped blocks into the same header store the
// polling Sync writes; SetHead serializes them. src is the bounded
// inline-backfill source for head+N>1 arrivals. On any setup error the
// caller logs and continues on the polling Sync (best-effort).
func (d *Daemon) startGossipHead(ctx context.Context, store *hstore.Store, src blockingest.BackfillSource, network build.Network, chainAPI *handlers.ChainAPI, fetcher *combined.Fetcher) error {
	listeners := splitCSV(d.cfg.P2PListen)
	host, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs:    listeners,
		BootstrapPeers: network.BootstrapPeers(),
		MinPeers:       20,
		MaxPeers:       200,
	})
	if err != nil {
		return fmt.Errorf("start libp2p host: %w", err)
	}

	// Net* RPC methods (Curio's webui consumes these) get real data.
	if chainAPI != nil {
		chainAPI.NetInfoSource = host.NetInfo()
	}

	// DHT discovery so peer count climbs past the bootstrap floor and the
	// gossipsub mesh fills. Non-fatal: gossipsub still works on bootstrap
	// peers alone, just with fewer mesh members.
	if err := host.EnableDHT(ctx, llibp2p.DHTOptions{
		BootstrapPeers: network.BootstrapPeers(),
		NetworkName:    network.NetworkName(),
	}); err != nil {
		log.Warnw("libp2p EnableDHT failed; continuing without DHT discovery", "err", err)
	}

	if host.PubSub == nil {
		_ = host.Close()
		return fmt.Errorf("libp2p host has no pubsub instance")
	}

	ing, _, err := blockingest.Start(ctx, host.PubSub, store, src, network.GossipTopicBlocks())
	if err != nil {
		_ = host.Close()
		return fmt.Errorf("start gossipsub block ingestor: %w", err)
	}

	// lantern#45 Stage 4: wire the gossipsub mempool publisher on the same
	// pubsub instance, so eth_sendRawTransaction can broadcast SP txs over
	// /fil/msgs/<network> locally instead of forwarding to the bridge.
	// Best-effort: a mpool wiring failure must not sink head-tracking, it
	// just leaves eth_sendRawTransaction on the bridge fallback.
	if chainAPI != nil {
		mp, mperr := mpool.New(ctx, host.PubSub, mpool.Config{
			Topic: network.GossipTopicMessages(),
		})
		if mperr != nil {
			log.Warnw("mpool publisher unavailable; eth_sendRawTransaction stays on bridge", "err", mperr)
		} else {
			chainAPI.Mpool = mp
			d.mu.Lock()
			d.mpool = mp
			d.mu.Unlock()
			log.Infow("gossipsub mempool publisher wired", "topic", network.GossipTopicMessages())
		}
	}

	// lantern#50: mount the libp2p Bitswap client as a high-priority block
	// source on the embedded fetcher. On calibration both existing sources
	// (gateway + glif) point at Glif, so a bridge-off daemon otherwise has
	// no non-Glif way to fetch message/receipt blocks for StateSearchMsg.
	// Bitswap pulls those blocks from the gossip peers we're already
	// connected to. Best-effort: a bitswap failure leaves the HTTP sources
	// in place (bridge-off availability degrades, but head-tracking and
	// reads continue). Every Bitswap block is CID-verified in the Fetcher,
	// so peers can't lie.
	if fetcher != nil {
		bsClient, bserr := bitswap.New(ctx, bitswap.Config{
			Host:           host.H,
			ProviderFinder: host.ContentRouter(),
			// Filecoin serves bitswap under "/chain/ipfs/bitswap/...";
			// without this prefix the client speaks IPFS-default protocol
			// IDs the Filecoin swarm doesn't support and finds no blocks.
			ProtocolPrefix: network.BitswapProtocolPrefix(),
		})
		if bserr != nil {
			log.Warnw("bitswap unavailable; block fetches stay on HTTP sources", "err", bserr)
		} else {
			fetcher.AddSource(combined.Source{
				Name:    "bitswap",
				Getter:  bsClient,
				Timeout: 5 * time.Second,
				Race:    true, // race against the gateway, first wins
			}, true)
			d.mu.Lock()
			d.bitswap = bsClient
			d.mu.Unlock()
			log.Infow("bitswap block source mounted on embedded fetcher")
		}
	}

	d.mu.Lock()
	d.p2pHost = host
	d.ingestor = ing
	d.mu.Unlock()

	log.Infow("gossipsub head-tracking active",
		"peer_id", host.ID().String(),
		"topic", network.GossipTopicBlocks())
	if !d.cfg.EmbeddedMode {
		fmt.Printf("daemon: gossipsub head-tracking on %s (libp2p id=%s)\n",
			network.GossipTopicBlocks(), host.ID())
	}
	return nil
}

// splitCSV splits a comma-separated string, trimming spaces and dropping
// empties.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// fetchTrustedHead probes the primary gateway's /state/root endpoint,
// falling back to Glif's Filecoin.ChainHead when the gateway is down.
// Both responses are CID-verified before becoming a TrustedRoot.
//
// AcceptedAt is stamped on every TrustedRoot so the dashboard's anchor
// age stat populates. Also attempts a best-effort F3 latest-cert probe
// so F3Instance is populated.
//
// (Mirrored from cmd/lantern/main.go's fetchTrustedHead. Once the full
// extraction lands, cmd/lantern will call this version and the
// duplicate will be removed from cmd/lantern.)
func fetchTrustedHead(ctx context.Context, gw string, network build.Network) (*trustedroot.TrustedRoot, error) {
	now := time.Now().UTC()

	if gw != "" {
		hc := hsync.NewClient([]string{gw}, 5*time.Second)
		if head, err := hc.GetStateHead(ctx); err == nil {
			if stateRoot, e := cid.Parse(head.StateRoot); e == nil {
				tskCids := make([]cid.Cid, 0, len(head.TipsetKey))
				for _, s := range head.TipsetKey {
					if c, e := cid.Parse(s); e == nil {
						tskCids = append(tskCids, c)
					}
				}
				pw, _ := big.FromString(head.ParentWeight)
				tr := &trustedroot.TrustedRoot{
					Epoch:        abi.ChainEpoch(head.Epoch),
					StateRoot:    stateRoot,
					TipSetKey:    types.NewTipSetKey(tskCids...),
					ParentWeight: pw,
					AcceptedAt:   now,
				}
				attachF3Latest(ctx, tr)
				return tr, nil
			}
		}
	}

	// Fallback to Glif when the gateway is unreachable or returned an
	// unparseable head. Network-aware: calibration falls back to the
	// calibration Glif endpoint; mainnet uses the mainnet endpoint.
	// Without this, a calibration daemon would silently pull mainnet
	// chain data from mainnet Glif.
	glifURL := "https://api.node.glif.io/rpc/v1"
	if network == build.Calibration {
		glifURL = "https://api.calibration.node.glif.io/rpc/v1"
	}
	gc := glif.New(glifURL, 10*time.Second)
	gh, err := gc.FetchHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("both gateway and Glif failed: %w", err)
	}
	tr := &trustedroot.TrustedRoot{
		Epoch:                 gh.Epoch,
		StateRoot:             gh.StateRoot,
		TipSetKey:             gh.TipSetKey,
		ParentWeight:          gh.ParentWeight,
		ParentMessageReceipts: gh.ParentMessageReceipts,
		AcceptedAt:            now,
	}
	attachF3Latest(ctx, tr)
	return tr, nil
}

// attachF3Latest does a best-effort Filecoin.F3GetLatestCertificate
// probe against Glif so observability surfaces (dashboard, logs) can
// render F3 instance. Failures are silent.
func attachF3Latest(ctx context.Context, tr *trustedroot.TrustedRoot) {
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	src := subscriber.NewJSONRPCSource("https://api.node.glif.io/rpc/v1")
	cert, err := src.GetLatest(probeCtx)
	if err != nil || cert == nil {
		return
	}
	tr.F3Instance = cert.GPBFTInstance
	tr.F3Cert = cert
}
