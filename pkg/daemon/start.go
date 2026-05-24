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
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/glif"
	"github.com/Reiers/lantern/net/hsync"
	"github.com/Reiers/lantern/rpc/handlers"
	rpcserver "github.com/Reiers/lantern/rpc/server"
	"github.com/Reiers/lantern/state/hamt"
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
	glifURL := ""
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
		sync := hstore.NewSync(store, src, hstore.SyncOptions{
			Interval:       d.cfg.SyncInterval,
			MaxBacktrack:   60,
			BootstrapDepth: 3,
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
			"sync_interval", d.cfg.SyncInterval,
			"notify_buf", d.cfg.NotifyBufSize,
			"embedded", d.cfg.EmbeddedMode)
		if !d.cfg.EmbeddedMode {
			fmt.Printf("daemon: header store %s (sync every %s, buf=%d)\n",
				hsPath, d.cfg.SyncInterval, d.cfg.NotifyBufSize)
		}
	}

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
	d.rpcServer = nil
	d.auth = nil
	d.headerSync = nil
	d.headerStore = nil
	d.headNotify = nil
	d.started = false
	d.mu.Unlock()

	if srv != nil {
		if err := srv.Stop(ctx); err != nil {
			return fmt.Errorf("stop rpc server: %w", err)
		}
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
	// calibration Glif endpoint; mainnet uses the default (empty URL
	// = api.node.glif.io). Without this, a calibration daemon would
	// silently pull mainnet chain data from mainnet Glif.
	glifURL := ""
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
