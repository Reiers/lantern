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
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
	"github.com/Reiers/lantern/chain/ecfinality"
	"github.com/Reiers/lantern/chain/f3/subscriber"
	"github.com/Reiers/lantern/chain/fullvalidate"
	"github.com/Reiers/lantern/chain/headcheck"
	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/headnotify"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/blockingest"
	"github.com/Reiers/lantern/net/blockpub"
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/glif"
	"github.com/Reiers/lantern/net/hsync"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/net/mpool"
	"github.com/Reiers/lantern/rpc/handlers"
	rpcserver "github.com/Reiers/lantern/rpc/server"
	statecache "github.com/Reiers/lantern/state/cache"
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
	//
	// Cache tier selection: the PDP / mid-node tier uses a PERSISTENT
	// (Badger) block cache so the warm contract set survives restart; the
	// light node stays in-memory. Both satisfy combined.Cache.
	var cache combined.Cache
	if d.cfg.PersistentCache {
		bcPath := filepath.Join(d.cfg.DataDir, network.String(), "blockcache")
		if err := os.MkdirAll(bcPath, 0o700); err != nil {
			return fmt.Errorf("create block cache dir: %w", err)
		}
		bc, err := statecache.Open(bcPath, statecache.Options{SoftCapBytes: d.cfg.PersistentCacheBytes})
		if err != nil {
			return fmt.Errorf("open persistent block cache: %w", err)
		}
		d.mu.Lock()
		d.blockCache = bc
		d.mu.Unlock()
		cache = bc
		if !d.cfg.EmbeddedMode {
			fmt.Printf("daemon: persistent block cache %s (soft cap %d bytes)\n", bcPath, bc.Stats().SoftCapBytes)
		}
		log.Infow("persistent block cache enabled (PDP tier)", "path", bcPath, "soft_cap", bc.Stats().SoftCapBytes)
	} else {
		cache = hamt.NewMemBlockStore()
	}
	// fallbackRPC is the Lotus-compatible RPC used as the polling Sync head
	// source AND the gossipsub ingestor's head+N backfill source AND the
	// last-resort cold state-block fetcher. Gossipsub is the PRIMARY
	// (RPC-free) head source when libp2p is enabled; this URL is the
	// fallback that keeps head advancing when the gossip mesh is cold or
	// stalls.
	//
	// lantern#50 part 3: bridge-off trust is now EXPLICIT.
	//   - Config.FallbackRPC overrides the URL (e.g. operator's own Forest).
	//   - Config.NoFallbackRPC removes the upstream RPC entirely: head comes
	//     only from gossipsub, cold blocks only from gateway+Bitswap. A
	//     gossip stall then surfaces as a stalled head (observable) instead
	//     of a silent Glif fetch. Previously a bridge-off node fell back to
	//     Glif here with no way to opt out - a hidden third-party dependency.
	fallbackRPC := d.cfg.FallbackRPC
	if fallbackRPC == "" {
		fallbackRPC = "https://api.node.glif.io/rpc/v1"
		if network == build.Calibration {
			fallbackRPC = "https://api.calibration.node.glif.io/rpc/v1"
		}
	}
	noFallback := d.cfg.NoFallbackRPC

	fetcherSources := []combined.Source{
		{Name: "gateway", Getter: hsync.NewClient([]string{gw}, 20*time.Second), Timeout: 5 * time.Second, Race: true},
	}
	if !noFallback {
		fetcherSources = append(fetcherSources,
			combined.Source{Name: "glif", Getter: glif.New(fallbackRPC, 20*time.Second), Timeout: 20 * time.Second})
	} else {
		log.Infow("bridge-off: no upstream RPC fallback wired (lantern#50)",
			"head_source", "gossipsub-only", "cold_blocks", "gateway+bitswap")
	}
	fetcher := combined.New(cache, fetcherSources...)

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
		// #87: follow the live verified head for actor-state reads (see the
		// FollowHeadState doc + the CLI daemon wiring). Embedded consumers
		// (curio-core) get the same head-following state surface.
		chainAPI.FollowHeadState()

		dist := headnotify.New(store, d.cfg.NotifyBufSize)
		dist.Start()
		chainAPI.HeadNotify = dist

		// #40: when libp2p is enabled, gossipsub is the PRIMARY head
		// source (0-1 epoch latency, no upstream-RPC dependency) and the
		// polling Sync drops to a relaxed cadence as the catch-up fallback,
		// matching standalone cmd/lantern. When libp2p is disabled (the
		// curio-core default today), Sync stays at the configured interval
		// as the sole head source.
		libp2pEnabled := !d.cfg.NoLibp2p && d.cfg.P2PListen != ""

		// #50 part 3: when NoFallbackRPC is set there is no RPC Sync source.
		// Gossipsub must be the head driver; the polling Sync becomes a no-op.
		// IMPORTANT: build syncSrc / backfillSrc as truly-nil INTERFACES (not
		// a typed-nil *glif.Client), so the nil guards in hstore.Sync and the
		// gossip ingestor fire correctly. A typed-nil wrapped in an interface
		// is non-nil and would nil-panic on first method call.
		var syncSrc hstore.RPCSource
		var backfillSrc blockingest.BackfillSource
		if !noFallback {
			gc := glif.New(fallbackRPC, 8*time.Second)
			syncSrc = gc
			backfillSrc = gc
		} else if !libp2pEnabled {
			// No RPC fallback AND no gossip head source = no way to track
			// head at all. Refuse rather than silently freeze.
			_ = store.Close()
			return fmt.Errorf("NoFallbackRPC requires libp2p/gossipsub enabled (it is the only head source); enable P2PListen or unset NoFallbackRPC")
		}
		syncInterval := d.cfg.SyncInterval
		if libp2pEnabled {
			syncInterval = 30 * time.Second
		}
		// lantern#123 findings 8+9: on devnet, a single-node docker cluster
		// can't form a gossipsub mesh, so head arrives ONLY via polling. The
		// mainnet-oriented 30s cadence set above is 7.5x block-time on a 4s
		// devnet. Honor an explicit DevnetHeadPollInterval override, otherwise
		// derive from the devnet-config BlockDelaySecs (captured at
		// devnet-init time by #125), falling back to 4s.
		if network == build.Devnet {
			switch {
			case d.cfg.DevnetHeadPollInterval > 0:
				syncInterval = d.cfg.DevnetHeadPollInterval
			default:
				cadence := 4 * time.Second
				if devnetCfg := build.GetDevnetConfig(); devnetCfg != nil && devnetCfg.BlockDelaySecs > 0 {
					cadence = time.Duration(devnetCfg.BlockDelaySecs) * time.Second
				}
				syncInterval = cadence
			}
		}

		// Issue #33: the hardened Sync resumes catch-up contiguously from
		// currentHead+1 in CatchUpChunk-sized steps (never skips epochs),
		// and advances head only as far as the chain is gap-free (retrying
		// the rest next poll rather than aborting). MaxBacktrack is raised
		// so reorg/backfill depth comfortably exceeds the deepest realistic
		// single-wait lag.
		// #51 "down for a maintenance window" auto-heal: if the persisted
		// store is more than staleReset epochs behind live head, re-anchor
		// near live head instead of trying (and failing) to backfill an
		// un-connectable gap. Without this the embedded daemon wedges its
		// head after any downtime longer than MaxBacktrack and needs a manual
		// `lantern reset --chain-state` (this is what forced that workaround
		// for embedded testers). The standalone cmd/lantern daemon already
		// wires this; pkg/daemon (the embedded path curio-core / maxboom use)
		// previously did not. Chain state only — keys are never touched.
		// Config.StaleResetThreshold: 0 => default 2880 (~1 day); <0 => off.
		staleReset := resolveStaleResetThreshold(d.cfg.StaleResetThreshold)
		sync := hstore.NewSync(store, syncSrc, hstore.SyncOptions{
			Interval:            syncInterval,
			MaxBacktrack:        900, // ~7.5h at 30s blocks; covers long proving waits
			BootstrapDepth:      3,
			CatchUpChunk:        200, // bounded per-poll catch-up work
			StaleResetThreshold: staleReset,
			OnStaleReset: func(storeHead, liveHead abi.ChainEpoch) {
				log.Warnw("header store too stale to backfill contiguously; re-anchoring near live head (chain state only, keys untouched)",
					"store_head", storeHead, "live_head", liveHead, "lag", liveHead-storeHead)
			},
		})
		// Full tier (#90): wire the pure-Go per-block consensus validator so
		// each ingested block's signature / VRF / win-count is re-verified
		// against resident F3-anchored state. Observe-only unless
		// FullValidationFatal is set. nil on Light/PDP (zero cost).
		if d.cfg.FullValidation && chainAPI != nil {
			sv := chainAPI.FullValidateView()
			hsForBeacon := store
			// Opt-in WinningPoSt SNARK verify (#87 + #88): loads the miner's
			// active-sector set from parent state, derives the challenge
			// randomness + selected sector, and runs pure-Go Groth16 verify.
			// Failures are logged whatever FullValidationFatal is set to (we
			// do NOT gate ingest on this until the pipeline is calibration-
			// soaked; see #87 / #104).
			var wpsv fullvalidate.MinerSectorSetView
			var wpParamsDir string
			if d.cfg.WinningPoStVerify {
				wpsv = chainAPI.WinningPoStSectorView()
				wpParamsDir = d.cfg.WinningPoStParamsDir
				if wpParamsDir == "" {
					wpParamsDir = filepath.Join(d.cfg.DataDir, "proof-params")
				}
			}
			sync.SetBlockValidator(func(ctx context.Context, bh *types.BlockHeader) error {
				// Resolve prevBeacon from the store so entry-less blocks
				// validate fully (nil only if none found within the walk).
				prevBeacon, _ := hsForBeacon.LatestBeaconEntry(bh)
				_, err := fullvalidate.ValidateBlockConsensus(ctx, bh, prevBeacon, sv)
				if wpsv != nil {
					if werr := fullvalidate.VerifyBlockWinningPoSt(
						ctx, bh, prevBeacon, sv, wpsv, wpParamsDir,
					); werr != nil {
						log.Warnw("winning-post verify failed (observe-only; not gating ingest)",
							"epoch", bh.Height, "miner", bh.Miner.String(), "err", werr)
					}
				}
				return err
			}, d.cfg.FullValidationFatal)
			log.Infow("full-node block validation wired (#90)",
				"fatal", d.cfg.FullValidationFatal,
				"winning_post_verify", d.cfg.WinningPoStVerify)
		}
		if err := sync.Start(ctx); err != nil {
			_ = store.Close()
			return fmt.Errorf("start header sync: %w", err)
		}

		d.mu.Lock()
		d.headerStore = store
		d.headerSync = sync
		d.headNotify = dist
		// #96: observed-data EC finality over the header store (FRC-0089).
		// 900 = Filecoin ChainFinality (mainnet + calibration).
		d.ecFinality = ecfinality.NewCache(store, 900)
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
			if err := d.startGossipHead(ctx, store, backfillSrc, network, chainAPI, fetcher); err != nil {
				log.Warnw("gossipsub head-tracking unavailable; falling back to polling Sync", "err", err)
			}
		}

		// lantern#123: devnet lotus-RPC mpool sink.
		//
		// On a single-node docker devnet the gossipsub mesh can't form
		// (no peers), so the standard mpool.Pool wired inside
		// startGossipHead has nothing to publish onto. Wire a Pool whose
		// Config.Sink POSTs directly to the devnet's lotus via
		// Filecoin.MpoolPush. All other Pool semantics (persist journal,
		// pending set, nonce derivation, #47 reconcile/rebroadcast loop)
		// work identically; only the wire transport changes.
		//
		// Trust posture: unchanged from #122. Devnet is single-source by
		// design (operator owns the devnet). This path is guarded on
		// network == Devnet so it never fires on mainnet/calibration.
		if chainAPI != nil && chainAPI.Mpool == nil && network == build.Devnet {
			devnetCfg := build.GetDevnetConfig()
			if devnetCfg != nil && devnetCfg.LotusRPC != "" {
				lotusClient := glif.New(devnetCfg.LotusRPC, 15*time.Second)
				persistPath := ""
				if !d.cfg.MpoolPersistDisabled {
					if d.cfg.MpoolPersistPath != "" {
						persistPath = d.cfg.MpoolPersistPath
					} else {
						persistPath = filepath.Join(d.cfg.DataDir, network.String(), "mpool", "pending.jsonl")
					}
				}
				sink := func(sctx context.Context, sm *types.SignedMessage, _ []byte) (cid.Cid, error) {
					return lotusClient.MpoolPush(sctx, sm)
				}
				mp, mperr := mpool.New(ctx, nil, mpool.Config{
					Topic:       network.GossipTopicMessages(),
					PersistPath: persistPath,
					Sink:        sink,
				})
				if mperr != nil {
					log.Warnw("devnet lotus-RPC mpool wiring failed; MpoolPushMessage will error out", "err", mperr)
				} else {
					chainAPI.Mpool = mp
					d.mu.Lock()
					d.mpool = mp
					d.mu.Unlock()
					log.Infow("devnet mpool wired via lotus RPC (lantern#123)",
						"lotusRPC", devnetCfg.LotusRPC,
						"persistPath", persistPath,
						"restored", mp.Stats().Restored)
				}
			} else {
				log.Warnw("devnet mode active but devnet-config missing LotusRPC; MpoolPushMessage will error out",
					"hint", "re-run `lantern devnet-init --lotus-rpc <URL>`")
			}
		}

		// lantern#44: FEVM contract-state prefetcher. On every head advance,
		// walk the configured EVM contract addresses' bytecode + storage
		// subtree into the local blockstore cache so later eth_calls hit
		// the cache instead of falling back to the bridge. Strictly
		// best-effort: walks run on goroutines, and a failed walk is logged
		// at debug and dropped.
		// lantern#69: merge Lantern's built-in per-network warm-set (the
		// well-known PDP/FWSS/registry/USDFC proxies) with any addresses the
		// consumer injected. This makes the zero-bridge read path work even
		// for consumers that supply nothing (e.g. stock upstream Curio),
		// whose Settle / provider-lookup eth_calls would otherwise local-miss
		// and fall back to the bridge.
		builtinWarm := prefetch.BuiltinWarmSet(d.cfg.Network)
		warmAddrs := prefetch.MergeWarmSets(builtinWarm, d.cfg.FEVMPrefetchAddrs)
		if len(warmAddrs) > 0 {
			pf := prefetch.New(prefetch.Config{
				Addrs:            warmAddrs,
				MaxBlocksPerAddr: d.cfg.FEVMPrefetchMaxBlocksPerAddr,
				PerAddrTimeout:   d.cfg.FEVMPrefetchPerAddrTimeout,
				MinInterval:      d.cfg.FEVMPrefetchMinInterval,
			}, fetcher)
			// PDP tier: pin the warmed static-contract subtrees in the
			// persistent cache so the warm set survives restart un-evicted.
			if d.cfg.PersistentCache {
				d.mu.Lock()
				bc := d.blockCache
				d.mu.Unlock()
				if bc != nil {
					pf.SetPinner(bc)
					log.Infow("prefetcher pinning enabled (PDP warm set persists)")
				}
			}
			store.OnHeadChange(func(ts *types.TipSet) {
				pf.Trigger(ctx, ts)
			})
			d.mu.Lock()
			d.fevmPrefetch = pf
			d.mu.Unlock()
			log.Infow("FEVM state prefetcher wired",
				"addrs", len(warmAddrs),
				"builtin", len(builtinWarm),
				"consumer", len(d.cfg.FEVMPrefetchAddrs),
				"network", d.cfg.Network,
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

	// lantern#50 prefetch-on-send: when eth_sendRawTransaction publishes a
	// tx locally, warm its message/receipt blocks into the Bitswap cache in
	// the background so the follow-up receipt poll resolves locally instead
	// of racing a cold cross-peer fetch. Bound to the daemon root context
	// (cancelled on Stop). Best-effort and read-only; nil hook = unchanged.
	d.sendWarmer = newSendWarmer(ctx, chainAPI)
	chainAPI.OnSentTx = d.sendWarmer.Warm

	// Stash for accessor-style reads (LocalEthCallStats, etc.).
	d.mu.Lock()
	d.chainAPI = chainAPI
	d.mu.Unlock()

	// Optional VM bridge: when configured, FEVM read methods (eth_call,
	// eth_estimateGas) and SendRawTransaction get forwarded to an
	// upstream Forest/Lotus node. Without this, eth_call against any
	// contract returns "FEVM method requires --vm-bridge-rpc".
	//
	// lantern#123: on devnet, if the operator didn't set --vm-bridge-rpc
	// explicitly, auto-wire the devnet lotus as the bridge. The trust
	// posture is unchanged from #122: devnet is single-source by design
	// (operator owns both the devnet lotus and the Lantern client).
	// Without this, eth_getCode / eth_call / eth_getStorageAt time out
	// on cold state (no libp2p means no bitswap for state blocks) and
	// fall through to `errBridgeUnconfigured`.
	vmBridgeRPC := d.cfg.VMBridgeRPC
	vmBridgeToken := d.cfg.VMBridgeToken
	if vmBridgeRPC == "" && network == build.Devnet {
		if devnetCfg := build.GetDevnetConfig(); devnetCfg != nil && devnetCfg.LotusRPC != "" {
			vmBridgeRPC = devnetCfg.LotusRPC
			log.Infow("devnet auto-wiring lotus as VMBridge (lantern#123)", "lotusRPC", vmBridgeRPC)
		}
	}
	if vmBridgeRPC != "" {
		vmBr := bridge.NewForestBridge(vmBridgeRPC, vmBridgeToken, d.cfg.VMBridgeTimeout)
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
	blockCache := d.blockCache
	d.rpcServer = nil
	d.auth = nil
	d.headerSync = nil
	d.headerStore = nil
	d.headNotify = nil
	d.p2pHost = nil
	d.ingestor = nil
	d.bitswap = nil
	d.blockCache = nil
	d.blockPub = nil
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
	// Close the persistent block cache last (nothing else rides on it).
	if blockCache != nil {
		if err := blockCache.Close(); err != nil {
			return fmt.Errorf("close block cache: %w", err)
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
	// #80 part 2: when head corroboration is enabled, the tracker rides
	// gossipsub's raw tracer so every duplicate block delivery is counted
	// per source peer. Nil tracker => nil tracer => zero overhead.
	var corroTracker *blockpub.CorroborationTracker
	if d.cfg.HeadCorroborationPeers > 0 {
		corroTracker = blockpub.NewCorroborationTracker(network.GossipTopicBlocks())
	}
	host, err := llibp2p.New(ctx, llibp2p.HostConfig{
		ListenAddrs:    listeners,
		BootstrapPeers: network.BootstrapPeers(),
		MinPeers:       20,
		MaxPeers:       200,
		PubSubTracer:   corroTracker.Tracer(),
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

	ing, blockPub, err := blockingest.Start(ctx, host.PubSub, store, src, network.GossipTopicBlocks())
	if err != nil {
		_ = host.Close()
		return fmt.Errorf("start gossipsub block ingestor: %w", err)
	}
	// #80 part 2: head adoption requires corroboration from distinct
	// scored peers. Trusted floor peers super-vote; the requirement
	// clamps to the connected-peer count so a small embedded node (a
	// curio-core box with a handful of peers) never wedges.
	if corroTracker != nil {
		ing.SetHeadCorroboration(blockpub.CorroborationGate(
			corroTracker, d.cfg.HeadCorroborationPeers,
			host.IsTrustedPeer,
			func() int { return host.PeerCount() }))
		log.Infow("head-source corroboration enabled", "minSources", d.cfg.HeadCorroborationPeers)
	}
	// Capture the /fil/blocks publisher so a PDP/backup-tier daemon can
	// actually SUBMIT blocks (SyncSubmitBlock -> BlockPublisher). The CLI
	// path already wired this; the embedded daemon previously discarded it,
	// so an embedded node could create but never publish a block. It's only
	// USED when AllowBlockSubmit=true (+ a VM bridge for a valid state
	// root); otherwise it just sits idle as the block-topic subscriber.
	// Always wire the publisher onto the ChainAPI; SyncSubmitBlock still
	// independently gates on AllowBlockSubmit, so wiring it unconditionally
	// is safe and means a later toggle needs no re-plumb.
	d.mu.Lock()
	d.blockPub = blockPub
	if d.chainAPI != nil {
		d.chainAPI.SetBlockPublisher(blockPub)
	}
	d.mu.Unlock()

	// #71: now that the gossip ingestor exists, let the polling Sync skip
	// its upstream-RPC HeadEpoch() poll whenever gossip installed a block
	// recently. Freshness window = 2x the (relaxed, 30s) sync interval so a
	// single missed gossip block doesn't immediately re-trigger Glif polls.
	// When gossip goes quiet the window lapses and the Sync resumes polling.
	d.mu.Lock()
	hsync := d.headerSync
	d.mu.Unlock()
	if hsync != nil {
		// libp2p is on here (startGossipHead only runs when enabled), so the
		// Sync interval was relaxed to 30s above; mirror that for the window.
		freshWindow := 60 * time.Second // 2x the relaxed 30s sync interval
		if d.cfg.SyncInterval > 0 && d.cfg.SyncInterval*2 > freshWindow {
			freshWindow = d.cfg.SyncInterval * 2
		}
		hsync.SetGossipFresh(func() bool { return ing.Fresh(freshWindow) })
		// #83: make the gossip-fresh skip lag-aware so a fresh-but-lagging
		// node (gossip skipping head+N>1 blocks it can't backfill) resumes
		// catch-up instead of wedging ~10-20 epochs behind the tip.
		hsync.SetGossipObservedHead(func() abi.ChainEpoch { return ing.ObservedHead() }, 0)
	}

	// #85 item 2: running-head divergence monitor. Best-effort and
	// observational - it never moves our head (gossip + #79 fork choice do
	// that). It periodically asks a diversity of independent RPC observers
	// what epoch the head is at and raises an eclipse/fork alarm if a
	// quorum of independent sources disagrees with our gossip head beyond
	// the 3-block lookback.
	//
	// #80 head-source diversity: build a Kind-DIVERSE source set, not just
	// operator RPC URLs. We add, when available: the operator's HeadCheckRPCs
	// (KindForest), the Lantern gateway (KindLanternGateway, HTTP /state/root),
	// and the fallback RPC / Glif (KindForest). headcheck counts agreement by
	// Kind, so N URLs of one kind = 1 source - this gives an honest multi-kind
	// quorum on the running head instead of boot-only. The monitor starts
	// whenever at least one corroborating source exists; it self-reports
	// StatusInsufficient (a no-op alarm) until enough distinct kinds are
	// reachable, so enabling it broadly is safe.
	{
		var hcSources []headcheck.HeadSource
		for _, u := range d.cfg.HeadCheckRPCs {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			hcSources = append(hcSources, headcheck.NewRPCHeadSource("", bootstrap.KindForest, u, "", 0))
		}
		// Gateway as an independent kind (unless the operator went
		// gateway-less). Distinct Kind => real diversity even with one RPC.
		// (Derived from cfg here since startGossipHead doesn't take the
		// startInternal-local gw/fallback vars; same resolution logic.)
		if d.cfg.Gateway != "" {
			hcSources = append(hcSources, headcheck.NewGatewayHeadSource(d.cfg.Gateway, 0))
		}
		// Fallback RPC / Glif as another corroborating source, unless the
		// operator explicitly went no-fallback (bridge-off purist).
		if !d.cfg.NoFallbackRPC {
			hcFallback := d.cfg.FallbackRPC
			if hcFallback == "" {
				hcFallback = "https://api.node.glif.io/rpc/v1"
				if network == build.Calibration {
					hcFallback = "https://api.calibration.node.glif.io/rpc/v1"
				}
			}
			hcSources = append(hcSources, headcheck.NewRPCHeadSource("glif", bootstrap.KindForest, hcFallback, "", 0))
		}
		if len(hcSources) > 0 {
			// #79 item 2: feed the divergence verdict back to the ingestor
			// as a head-adoption gate. While the running head diverges from
			// the independent-source quorum, hold head (no-adopt) instead of
			// only logging. StatusInsufficient does NOT close the gate: a
			// node the operator gave too few sources must not freeze head.
			var hcDiverged atomic.Bool
			ing.SetHeadAdoptionGate(func() bool { return !hcDiverged.Load() })
			mon := headcheck.New(headcheck.Config{
				Local:   func() abi.ChainEpoch { return ing.ObservedHead() },
				Sources: hcSources,
				OnResult: func(r headcheck.Result) {
					switch r.Status {
					case headcheck.StatusDiverge:
						hcDiverged.Store(true)
						log.Warnw("headcheck: running head DIVERGES from independent sources (possible eclipse/fork); HOLDING head adoption",
							"localHead", r.LocalHead, "medianExtHead", r.MedianExtHead,
							"agreeing", r.Agreeing, "disagreeing", r.Disagreeing, "reachable", r.Reachable)
					case headcheck.StatusAgree:
						if hcDiverged.Swap(false) {
							log.Infow("headcheck: running head re-corroborated; resuming head adoption",
								"localHead", r.LocalHead, "agreeing", r.Agreeing)
						}
					case headcheck.StatusInsufficient:
						// Too few reachable sources to judge: do not close the
						// gate (avoid freezing a lightly-corroborated node).
						hcDiverged.Store(false)
						log.Debugw("headcheck: head uncorroborated (too few reachable sources)",
							"localHead", r.LocalHead, "reachable", r.Reachable)
					}
				},
			})
			mon.Start(ctx)
			d.mu.Lock()
			d.headcheck = mon
			d.mu.Unlock()
			log.Infow("running-head divergence monitor started", "sources", len(hcSources), "lookback", headcheck.DefaultLookback)
		}
	}

	// lantern#45 Stage 4: wire the gossipsub mempool publisher on the same
	// pubsub instance, so eth_sendRawTransaction can broadcast SP txs over
	// /fil/msgs/<network> locally instead of forwarding to the bridge.
	// Best-effort: a mpool wiring failure must not sink head-tracking, it
	// just leaves eth_sendRawTransaction on the bridge fallback.
	if chainAPI != nil {
		// #119: derive the durable persist path. Default is
		// <DataDir>/<Network>/mpool/pending.jsonl unless the caller passed
		// an override, or MpoolPersistDisabled=true (empty path = memory-only).
		persistPath := ""
		if !d.cfg.MpoolPersistDisabled {
			if d.cfg.MpoolPersistPath != "" {
				persistPath = d.cfg.MpoolPersistPath
			} else {
				persistPath = filepath.Join(d.cfg.DataDir, network.String(), "mpool", "pending.jsonl")
			}
		}
		// lantern#123: on devnet, skip the gossipsub mpool wiring entirely.
		// A single-node docker devnet has no gossipsub mesh so a Publish
		// would silently drop the message into the void. Instead we let
		// startInternal's devnet block (further up in the caller) wire an
		// mpool.Pool with Config.Sink pointing at the devnet lotus's
		// Filecoin.MpoolPush. Leaving chainAPI.Mpool nil here is the trigger
		// for that later block. Bitswap wiring below still runs.
		if network == build.Devnet {
			log.Infow("gossipsub mempool wiring skipped on devnet; lotus-RPC sink wires next",
				"reason", "single-node docker devnet has no gossipsub mesh")
			goto afterMpoolWiring
		}
		{
			mp, mperr := mpool.New(ctx, host.PubSub, mpool.Config{
				Topic:       network.GossipTopicMessages(),
				PersistPath: persistPath,
			})
			if mperr != nil {
				log.Warnw("mpool publisher unavailable; eth_sendRawTransaction stays on bridge", "err", mperr)
			} else {
				chainAPI.Mpool = mp
				d.mu.Lock()
				d.mpool = mp
				d.mu.Unlock()
				log.Infow("gossipsub mempool publisher wired", "topic", network.GossipTopicMessages(), "persistPath", persistPath, "restored", mp.Stats().Restored)

				// lantern#47: drive the mpool's pending -> confirm -> rebroadcast
				// loop on every head advance. StateSearchMsg is local + zero-Glif
				// (#9/#49), so a published-but-unmined tx gets rebroadcast
				// (identical bytes, same nonce) instead of stalling silently and
				// blocking the sender's later nonces. Confirmed txs are dropped;
				// max-retries-exhausted txs surface as failed (never stuck).
				// Reconcile also walks each pending tx's message/receipt blocks
				// via StateSearchMsg, which warms exactly the blocks #50 needs
				// for a writing SP's own in-flight txs bridge-off.
				search := func(sctx context.Context, msgCID cid.Cid) (mpool.SearchResult, error) {
					lk, serr := chainAPI.StateSearchMsg(sctx, types.TipSetKey{}, msgCID, 0, false)
					if serr != nil {
						return mpool.SearchUnknown, serr
					}
					if lk != nil {
						return mpool.SearchFound, nil
					}
					return mpool.SearchUnknown, nil
				}
				// The header store fires OnHeadChange listeners INLINE on the
				// SetHead path, so the callback must not block head advancement.
				// Reconcile does StateSearchMsg I/O across every pending tx, so we
				// run it on its own goroutine, with a single-flight guard so a slow
				// reconcile can't pile up behind fast head advances (we'd rather
				// skip a tick than queue an unbounded backlog; the next head picks
				// up any still-pending tx).
				var reconciling int32
				store.OnHeadChange(func(ts *types.TipSet) {
					if ts == nil {
						return
					}
					if !atomic.CompareAndSwapInt32(&reconciling, 0, 1) {
						return // a reconcile is already in flight; skip this tick
					}
					height := int64(ts.Height())
					go func() {
						defer atomic.StoreInt32(&reconciling, 0)
						mp.Reconcile(ctx, height, search)
					}()
				})
			}
		}
	afterMpoolWiring:
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

// defaultStaleResetThreshold is the epochs-behind-live-head auto-heal
// threshold used by the embedded daemon when Config.StaleResetThreshold
// is left at 0. 2880 epochs ~= 1 day at 30s blocks, matching the
// standalone cmd/lantern daemon's default.
const defaultStaleResetThreshold = abi.ChainEpoch(2880)

// resolveStaleResetThreshold maps the Config knob to the effective
// SyncOptions value: 0 => default (2880), a negative value => 0
// (disabled), any positive value => itself. This is the #51 "down for a
// maintenance window" auto-heal that the embedded pkg/daemon path
// previously never set (only the standalone CLI did), which is what
// forced embedded testers to manually `lantern reset --chain-state`
// after downtime.
func resolveStaleResetThreshold(cfg abi.ChainEpoch) abi.ChainEpoch {
	switch {
	case cfg == 0:
		return defaultStaleResetThreshold
	case cfg < 0:
		return 0 // disabled
	default:
		return cfg
	}
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
