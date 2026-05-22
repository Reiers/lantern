// Command lantern is the user-facing Filecoin light-node CLI.
//
// Subcommands:
//
//	lantern init                    — interactive wizard (creates ~/.lantern + first wallet)
//	lantern daemon                  — runs the RPC server on 127.0.0.1:1234
//	lantern wallet new --type=bls   — create a key
//	lantern wallet new --type=secp
//	lantern wallet new --type=delegated
//	lantern wallet list
//	lantern wallet balance <addr>
//	lantern wallet send <to> <amount>
//	lantern chain head
//	lantern state get-actor <addr>
//	lantern info                    — print FULLNODE_API_INFO + status
//
// Network: defaults to mainnet via the public gateway at
// gateway.lantern.reiers.io. Override with `--gateway <url>`.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/f3/subscriber"
	hstore "github.com/Reiers/lantern/chain/header/store"
	headnotify "github.com/Reiers/lantern/chain/headnotify"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/internal/buildinfo"
	lbitswap "github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/glif"
	"github.com/Reiers/lantern/net/hello"
	"github.com/Reiers/lantern/net/hsync"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/rpc/handlers"
	"github.com/Reiers/lantern/rpc/server"
	"github.com/Reiers/lantern/state/hamt"
	"github.com/Reiers/lantern/vm/bridge"
	"github.com/Reiers/lantern/wallet"
)

const (
	defaultGateway = "https://gateway.lantern.reiers.io"
	defaultListen  = "127.0.0.1:1234"
)

// versionTag is set by the release build via -ldflags "-X main.versionTag=v...".
// Empty when built from source without -ldflags.
var versionTag string

func init() {
	// Push the ldflags-injected tag into internal/buildinfo so RPC
	// handlers and the libp2p user-agent pick it up without import
	// cycling through package main.
	buildinfo.SetVersion(versionTag)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	rest := os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(rest)
	case "daemon":
		err = cmdDaemon(rest)
	case "beacon":
		err = cmdBeacon(rest)
	case "doctor":
		err = cmdDoctor(rest)
	case "repair":
		err = cmdRepair(rest)
	case "service":
		err = cmdService(rest)
	case "stop":
		err = serviceStop(rest)
	case "restart":
		err = serviceRestart(rest)
	case "wallet":
		err = cmdWallet(rest)
	case "chain":
		err = cmdChain(rest)
	case "state":
		err = cmdState(rest)
	case "info":
		err = cmdInfo(rest)
	case "auth":
		err = cmdAuth(rest)
	case "version", "--version", "-v":
		fmt.Printf("lantern %s Lantern+%s (Phase 11 — installer + quorum bootstrap)\n",
			buildinfo.BuildVersion(), buildinfo.Network())
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `lantern — Filecoin light node (Phase 4)

USAGE
  lantern <command> [args...]

COMMANDS
  init [--bootstrap-quorum N] [--peer URL]...   Setup wizard with multi-source quorum bootstrap
  daemon [--gateway <url>]                      Run RPC server (default 127.0.0.1:1234)
  beacon [--cache-dir <p>]                      Run a Lantern beacon (Bitswap-only, no RPC)
  doctor [--bootstrap-quorum N]                 Re-run the quorum probe (read-only)
  repair [--bootstrap-quorum N]                 Re-anchor from a fresh quorum (overwrites bootstrap-anchor.json)
  service {install|uninstall|start|stop|restart|status}
                                                Manage the OS service (launchd / systemd user)
  stop / restart                                Aliases for 'service stop' / 'service restart'
  wallet new --type={bls|secp|delegated}
  wallet list
  wallet balance <addr>
  wallet send <to> <amount-FIL>                 (DRY-RUN — message preview)
  chain head
  state get-actor <addr>
  info                                          Show daemon status + FULLNODE_API_INFO
  version
  help

ENVIRONMENT
  LANTERN_HOME    Data directory (default: ~/.lantern)
  LANTERN_PASS    Keystore passphrase. When unset and stdin is a TTY,
                  Lantern prompts interactively. When unset on a non-TTY
                  process (e.g. a systemd service) Lantern refuses to
                  start; set LANTERN_PASS via an EnvironmentFile=. To
                  deliberately run with an unencrypted keystore, set
                  LANTERN_PASS='' (explicit empty).`)
}

// --- helpers ---

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dataDir() string {
	if h := os.Getenv("LANTERN_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lantern")
}

// resolvePassphrase decides what passphrase to use to unlock (or create)
// the keystore at dir. Resolution order:
//
//  1. LANTERN_PASS env var, when set and non-empty.
//  2. Explicit LANTERN_PASS="" (set but empty) -> unencrypted keystore,
//     with a stderr warning so misconfig is loud.
//  3. Interactive TTY prompt. When the keystore directory already exists
//     with key files, prompts once; when initializing a fresh keystore,
//     prompts twice (confirm matches).
//  4. No TTY and no env var -> hard error pointing at the EnvironmentFile
//     pattern for systemd / launchd.
//
// Issue #2: previously fell back silently to the empty string. That left
// keystores AES-GCM-encrypted with a known empty passphrase, effectively
// unencrypted to anyone with file read access on the box.
func resolvePassphrase(dir string) (string, error) {
	env, envSet := os.LookupEnv("LANTERN_PASS")
	if envSet && env != "" {
		return env, nil
	}
	if envSet && env == "" {
		// Operator explicitly opted out of encryption.
		fmt.Fprintln(os.Stderr, "\033[33m  warning: LANTERN_PASS is set but empty - keystore will be unencrypted\033[0m")
		return "", nil
	}

	// No env var. Need either a TTY or a clear error.
	if !isInteractive() {
		return "", fmt.Errorf("LANTERN_PASS is not set and stdin is not a TTY. " +
			"Set LANTERN_PASS for non-interactive runs. For systemd: use " +
			"EnvironmentFile=/etc/lantern/passphrase (chmod 600). " +
			"To deliberately use an unencrypted keystore set LANTERN_PASS='' (explicit empty)")
	}

	hasKeys := keystoreHasKeys(dir)
	if hasKeys {
		fmt.Fprint(os.Stderr, "Lantern keystore passphrase: ")
		p, err := readPassword()
		if err != nil {
			return "", fmt.Errorf("read passphrase: %w", err)
		}
		return p, nil
	}

	// Fresh keystore. Prompt twice and require a match.
	fmt.Fprintln(os.Stderr, "No existing Lantern keystore at", dir)
	fmt.Fprintln(os.Stderr, "Set a passphrase to encrypt local keys. Press enter without a value to opt out (NOT recommended).")
	fmt.Fprint(os.Stderr, "New passphrase: ")
	p1, err := readPassword()
	if err != nil {
		return "", fmt.Errorf("read passphrase: %w", err)
	}
	if p1 == "" {
		fmt.Fprintln(os.Stderr, "\033[33m  warning: keystore will be unencrypted\033[0m")
		return "", nil
	}
	fmt.Fprint(os.Stderr, "Confirm passphrase: ")
	p2, err := readPassword()
	if err != nil {
		return "", fmt.Errorf("read passphrase: %w", err)
	}
	if p1 != p2 {
		return "", errors.New("passphrases did not match")
	}
	return p1, nil
}

func openWallet() (*wallet.Wallet, error) {
	dir := filepath.Join(dataDir(), "keystore")
	p, err := resolvePassphrase(dir)
	if err != nil {
		return nil, err
	}
	return wallet.New(context.Background(), dir, p)
}

// gatewayClient builds the (cache + gateway + glif fallback) BlockGetter
// chain that's used by every state read in the CLI.
func gatewayClient(gw string) (hamt.BlockGetter, *combined.Fetcher) {
	cache := hamt.NewMemBlockStore()
	httpc := hsync.NewClient([]string{gw}, 20*time.Second)
	glifC := glif.New("", 20*time.Second)
	f := combined.New(cache,
		combined.Source{Name: "gateway", Getter: httpc, Timeout: 5 * time.Second, Race: true},
		combined.Source{Name: "glif", Getter: glifC, Timeout: 20 * time.Second},
	)
	return f, f
}

// fetchTrustedHead probes the primary gateway's /state/root endpoint,
// falling back to Glif's Filecoin.ChainHead when the gateway is down.
// Both responses are CID-verified before becoming a TrustedRoot.
//
// Issue #12: AcceptedAt is stamped on every TrustedRoot we hand out so
// the dashboard's "Anchor age" stat actually populates. This is the
// honest meaning of anchor age for a daemon that anchors on every boot:
// "we accepted this chain head at daemon start." The genuine
// long-lived anchor lives in bootstrap-anchor.json (written by 'lantern
// init') and carries its own AcceptedAt; this fallback is for daemons
// running on the embedded anchor without a separate init step.
//
// We also attempt a best-effort F3 latest-cert probe so F3Instance is
// populated when the dashboard renders. Failure is non-fatal: F3 is
// observability, not consensus, at this layer.
func fetchTrustedHead(ctx context.Context, gw string) (*trustedroot.TrustedRoot, error) {
	now := time.Now().UTC()
	hc := hsync.NewClient([]string{gw}, 5*time.Second)
	head, err := hc.GetStateHead(ctx)
	if err == nil {
		stateRoot, e := cid.Parse(head.StateRoot)
		if e == nil {
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
	// Fallback to Glif.
	fmt.Fprintln(os.Stderr, "(gateway unavailable; falling back to Glif RPC)")
	gc := glif.New("", 10*time.Second)
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

// attachF3Latest does a best-effort Filecoin.F3GetLatestCertificate probe
// against Glif so the dashboard can render F3 instance. Failures are
// silent; this is observability, not chain consensus.
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

// --- daemon ---

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	gw := fs.String("gateway", defaultGateway, "Lantern gateway base URL")
	listen := fs.String("listen", defaultListen, "RPC listen address")
	noHS := fs.Bool("no-header-store", false, "Disable the persistent header store (legacy synthetic-head mode)")
	hsPath := fs.String("header-store", filepath.Join(dataDir(), "headerstore"), "Header store BadgerDB path")
	syncInterval := fs.Duration("sync-interval", 6*time.Second, "Header-store sync poll interval")
	notifyBufSize := fs.Int("notify-buf", headnotify.DefaultBufferSize, "ChainNotify per-subscriber buffer size")
	p2pListen := fs.String("p2p-listen", "/ip4/0.0.0.0/tcp/0,/ip4/0.0.0.0/udp/0/quic-v1", "libp2p listen multiaddrs (comma-separated). Empty disables the libp2p host.")
	noLibp2p := fs.Bool("no-libp2p", false, "Skip starting the libp2p host (RPC stays up; Net* stats return zero).")
	bitswapEnabled := fs.Bool("bitswap", true, "Use Bitswap as primary fetch source (HTTP gateway falls to last resort).")
	bitswapFastDL := fs.Duration("bitswap-fast", 1500*time.Millisecond, "Bitswap fast-stage deadline for preferred peers.")
	bitswapFullDL := fs.Duration("bitswap-full", 5*time.Second, "Bitswap full-stage deadline for swarm broadcast.")
	preferredPeersStr := fs.String("bitswap-peers", "", "Comma-separated multiaddrs to use as preferred Bitswap peers (e.g. lantern beacon nodes).")
	metricsListen := fs.String("metrics", "", "Optional listen address for /metrics (Prometheus text exposition). Empty disables.")

	// Issue #4: VM bridge for block production state-root computation.
	//
	// Lantern's native VM is a gas-accurate Send-only shell. For
	// MinerCreateBlock + AllowBlockSubmit=true, the post-execution
	// ParentStateRoot must come from a real FVM. When --vm-bridge-rpc is
	// set, the daemon delegates that one computation to an upstream
	// Forest/Lotus node (typically the operator's own primary). This is
	// the SP failover backup story: when Lotus dies but Forest stays up
	// (or vice versa), Lantern can still produce blocks for Curio.
	//
	// AllowBlockSubmit refuses to start without a bridge configured.
	vmBridgeRPC := fs.String("vm-bridge-rpc", "", "Upstream Forest/Lotus JSON-RPC URL for VM bridge (e.g. http://127.0.0.1:1234/rpc/v1). Required when --allow-block-submit is set.")
	vmBridgeToken := fs.String("vm-bridge-token", "", "Optional Bearer token for the VM bridge upstream (defaults to env LANTERN_VM_BRIDGE_TOKEN when empty).")
	vmBridgeTimeout := fs.Duration("vm-bridge-timeout", 30*time.Second, "Per-request timeout for VM bridge RPC calls.")
	allowBlockSubmit := fs.Bool("allow-block-submit", false, "Allow SyncSubmitBlock to publish to gossipsub. Requires --vm-bridge-rpc.")
	fs.Parse(args)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("Lantern daemon — Lotus-compatible RPC")
	fmt.Println("Fetching trusted head from", *gw)
	tr, err := fetchTrustedHead(ctx, *gw)
	if err != nil {
		return err
	}
	fmt.Printf("  head epoch:  %d\n  state root:  %s\n", tr.Epoch, tr.StateRoot)

	w, err := openWallet()
	if err != nil {
		return fmt.Errorf("open wallet: %w", err)
	}

	// gatewayBG + fetcher is the cache+http chain. Bitswap, when enabled,
	// is inserted between cache and HTTP gateway later in this function
	// once the libp2p host is up.
	cache := hamt.NewMemBlockStore()
	fetcher := combined.New(cache,
		combined.Source{Name: "gateway", Getter: hsync.NewClient([]string{*gw}, 20*time.Second), Timeout: 5 * time.Second, Race: true},
		combined.Source{Name: "glif", Getter: glif.New("", 20*time.Second), Timeout: 20 * time.Second},
	)
	chainAPI := handlers.New(tr, fetcher, w, nil, "mainnet")

	// Issue #4: wire optional VM bridge for block production. Refuse to
	// start when AllowBlockSubmit is on but no bridge is configured;
	// silently publishing blocks with the parent stateRoot copied
	// verbatim would be rejected by the network and would consume the
	// miner's winning ticket. Failing loud here protects the SP.
	if *allowBlockSubmit && *vmBridgeRPC == "" {
		return fmt.Errorf("--allow-block-submit requires --vm-bridge-rpc to be set (see issue #4 in repo)")
	}
	if *vmBridgeRPC != "" {
		token := *vmBridgeToken
		if token == "" {
			token = os.Getenv("LANTERN_VM_BRIDGE_TOKEN")
		}
		vmBr := bridge.NewForestBridge(*vmBridgeRPC, token, *vmBridgeTimeout)
		chainAPI.WithBridge(vmBr)
		chainAPI.AllowBlockSubmit = *allowBlockSubmit
		fmt.Printf("  vm-bridge:    %s", vmBr.Provenance())
		if *allowBlockSubmit {
			fmt.Printf("  (allow-block-submit=true)")
		}
		fmt.Println()
	}

	// Phase 9: wire the persistent header store + sync agent + head-change
	// distributor so ChainNotify, ChainGetTipSetByHeight, ChainGetBlock,
	// StateGetBeaconEntry et al. become live.
	var store *hstore.Store
	var sync *hstore.Sync
	var dist *headnotify.Distributor
	if !*noHS {
		if err := os.MkdirAll(*hsPath, 0o700); err != nil {
			return fmt.Errorf("create header store dir: %w", err)
		}
		store, err = hstore.Open(*hsPath, hstore.Options{})
		if err != nil {
			return fmt.Errorf("open header store: %w", err)
		}
		defer store.Close()
		chainAPI.HeaderStore = store

		dist = headnotify.New(store, *notifyBufSize)
		dist.Start()
		chainAPI.HeadNotify = dist

		// Sync source: a Glif client. The combined fetcher in gatewayClient
		// is hamt-shaped (only Get), but Sync needs RPC-shaped
		// HeadEpoch/TipsetCIDsByHeight/FetchBlock — that's exactly what
		// glif.Client exposes.
		src := glif.New("", 8*time.Second)
		sync = hstore.NewSync(store, src, hstore.SyncOptions{
			Interval:       *syncInterval,
			MaxBacktrack:   60,
			BootstrapDepth: 3, // small cold start; ongoing polls catch up
		})
		if err := sync.Start(ctx); err != nil {
			return fmt.Errorf("start header sync: %w", err)
		}
		defer sync.Stop()
		fmt.Printf("  header store: %s (sync every %s, buf=%d)\n",
			*hsPath, syncInterval.String(), *notifyBufSize)
	}

	// Phase 10 Part A: bring up the live libp2p host so the Net* RPC
	// methods Curio's webui consumes return real data. The host dials the
	// Filecoin mainnet bootstrap peers in the background; bandwidth +
	// AutoNAT state are captured by the BandwidthCounter and the
	// AmbientAutoNAT subsystem on the host respectively.
	var p2pHost *llibp2p.Host
	var gossipIngestor *gossipBlockIngestor
	var helloSvc *hello.Service
	_ = helloSvc // kept around for future stats wiring; lifecycle is goroutine-managed
	if !*noLibp2p && *p2pListen != "" {
		listeners := splitCSV(*p2pListen)
		p2pHost, err = llibp2p.New(ctx, llibp2p.HostConfig{
			ListenAddrs:    listeners,
			BootstrapPeers: build.MainnetBootstrapPeers,
			MinPeers:       50,
			MaxPeers:       200,
		})
		if err != nil {
			return fmt.Errorf("start libp2p host: %w", err)
		}
		defer p2pHost.Close()
		chainAPI.NetInfoSource = p2pHost.NetInfo()
		fmt.Printf("  libp2p: id=%s listen=%v\n", p2pHost.ID(), p2pHost.ListenAddrs())

		// V1.2.1: enable Kademlia DHT in client mode plus the
		// closest-walk + dial-walk discovery loops so the peer count
		// climbs past the 3-5 bootstrap floor. See
		// PHASE11-PEER-COUNT-ASK.md for context.
		if err := p2pHost.EnableDHT(ctx, llibp2p.DHTOptions{
			BootstrapPeers: build.MainnetBootstrapPeers,
		}); err != nil {
			fmt.Printf("  libp2p: EnableDHT failed: %v (continuing without DHT discovery)\n", err)
		} else {
			fmt.Printf("  libp2p: DHT discovery on (target peers >= 50, hwm 200)\n")
		}

		// Issue #16: speak /fil/hello/1.0.0 so remote Filecoin peers'
		// connmgr scores us positively and stops trimming us within 30s.
		// Also tags inbound peers as "fcpeer" in our own connmgr so we
		// don't trim them.
		if genCID, perr := cid.Parse(build.MainnetGenesisCID); perr == nil {
			head := func() ([]cid.Cid, int64, string) {
				if store != nil {
					if ts := store.Head(); ts != nil {
						return ts.Cids(), int64(ts.Height()), ts.ParentWeight().String()
					}
				}
				// Fallback: trusted root's tipset key.
				if tr != nil {
					return tr.TipSetKey.Cids(), int64(tr.Epoch), tr.ParentWeight.String()
				}
				return nil, 0, "0"
			}
			helloSvc = hello.NewService(p2pHost.H, genCID, head)
			helloSvc.Register()
			go helloSvc.WatchNewConns(ctx)
			fmt.Printf("  hello:    /fil/hello/1.0.0 active (genesis %s)\n", build.MainnetGenesisCID[:18]+"…")
		} else {
			fmt.Printf("  hello:    DISABLED (genesis CID parse: %v)\n", perr)
		}

		// Issue #1: subscribe to /fil/blocks/<network> on gossipsub so
		// new heads land in the header store within ~1-3s of network
		// propagation, instead of waiting for the next 6-30s poll cycle.
		// The polling Sync above stays as the catch-up fallback for
		// blocks that gossipsub missed (connectivity blips, late join,
		// etc.) and for the first install on a cold start.
		//
		// We pass the same glif client the polling Sync uses so the
		// ingestor can do bounded inline backfill when a gossipsub
		// arrival lands at head+N>1 (rather than skipping and waiting
		// the full poll cycle).
		if store != nil && p2pHost.PubSub != nil {
			gossipSrc := glif.New("", 8*time.Second)
			if ing, _, gerr := startGossipBlocks(ctx, p2pHost.PubSub, store, gossipSrc); gerr != nil {
				fmt.Printf("  gossipsub-blocks: failed to start: %v (continuing without)\n", gerr)
			} else {
				gossipIngestor = ing
				fmt.Printf("  gossipsub-blocks: subscribed to %s (ingestor active, inline backfill on)\n", build.MainnetGossipTopicBlocks)
			}
		}
	}

	// Phase 10 Part B: real Bitswap as primary fetch path. We rebuild
	// the combined fetcher with Bitswap inserted between cache and HTTP
	// gateway, so the order is: cache → bitswap (fast deadline) →
	// gateway → glif.
	var bsClient *lbitswap.Client
	if *bitswapEnabled && p2pHost != nil {
		preferred, perr := parsePreferredPeers(*preferredPeersStr)
		if perr != nil {
			return fmt.Errorf("parse --bitswap-peers: %w", perr)
		}
		bsClient, err = lbitswap.New(ctx, lbitswap.Config{
			Host:           p2pHost.H,
			PreferredPeers: preferred,
			FastDeadline:   *bitswapFastDL,
			FullDeadline:   *bitswapFullDL,
		})
		if err != nil {
			return fmt.Errorf("start bitswap: %w", err)
		}
		defer bsClient.Close()
		// Issue #3 fix: gateway + Bitswap race in parallel for cold
		// blocks. State-tree walks that previously timed out at 30s
		// (because every cold block hit the 5s Bitswap timeout before
		// falling through to the gateway) now complete in low seconds.
		// Glif stays as the sequential last-resort fallback (different
		// shape, slower, public-service rate-limited).
		fetcher = combined.New(cache,
			combined.Source{Name: "bitswap", Getter: bsClient, Timeout: *bitswapFullDL, Race: true},
			combined.Source{Name: "gateway", Getter: hsync.NewClient([]string{*gw}, 20*time.Second), Timeout: 5 * time.Second, Race: true},
			combined.Source{Name: "glif", Getter: glif.New("", 20*time.Second), Timeout: 20 * time.Second},
		)
		rebindBlockGetter(chainAPI, fetcher)
		fmt.Printf("  bitswap:  enabled (preferred=%d, fast=%s, full=%s)\n",
			len(preferred), bitswapFastDL.String(), bitswapFullDL.String())
	}

	// Phase 10 Part B: optional /metrics endpoint exposes per-source hit
	// counts so operators can see Bitswap actually carrying load.
	//
	// Issue #5: when --metrics is set, also serve the operator dashboard
	// at /dashboard on the same listener.
	if *metricsListen != "" {
		bridgeTag := ""
		if chainAPI.Bridge != nil {
			bridgeTag = chainAPI.Bridge.Provenance()
		}
		dash := &dashboardDeps{
			tr:           tr,
			store:        store,
			sync:         sync,
			host:         p2pHost,
			bsClient:     bsClient,
			fetcher:      fetcher,
			ingestor:     gossipIngestor,
			vmBridgeTag:  bridgeTag,
			allowSubmit:  chainAPI.AllowBlockSubmit,
			network:      "Lantern+mainnet",
			rpcAddr:      *listen,
			startedAt:    time.Now(),
			headDelaySec: uint64(build.BlockDelaySecs),
			dataDirPath:  dataDir(),
			gatewayURL:   *gw,
			hello:        helloSvc,
		}
		go serveMetrics(ctx, *metricsListen, fetcher, bsClient, p2pHost, dash)
		fmt.Printf("  metrics:  http://%s/metrics\n", *metricsListen)
		fmt.Printf("  dashboard: http://%s/dashboard/\n", *metricsListen)
	}

	srv, err := server.New(server.Config{
		ListenAddress: *listen,
		DataDir:       dataDir(),
	}, chainAPI)
	if err != nil {
		return err
	}
	chainAPI.AuthIssuer = srv.Auth()

	if err := srv.Start(); err != nil {
		return err
	}
	apiInfo, _ := srv.FullNodeAPIInfo()
	fmt.Printf("\nRPC ready at http://%s/rpc/v1\n", srv.Addr())
	fmt.Printf("FULLNODE_API_INFO=%s\n", apiInfo)
	if dist != nil {
		go logSyncStats(ctx, sync, dist)
	}
	fmt.Println("\nReady. Ctrl-C to stop.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down...")
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	return srv.Stop(sctx)
}

// logSyncStats periodically prints sync + notify counters so operators can
// confirm the head store is advancing without spelunking through Badger.
func logSyncStats(ctx context.Context, s *hstore.Sync, d *headnotify.Distributor) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st := s.Stats()
			fmt.Fprintf(os.Stderr,
				"  [sync] polls=%d advances=%d reorgs=%d headers=%d head=%d subs=%d lastErr=%q\n",
				st.Polls, st.HeadAdvances, st.Reorgs, st.HeadersAdded,
				st.LastHeadEpoch, d.SubscriberCount(), st.LastError)
		}
	}
}

// --- wallet subcommands ---

func cmdWallet(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("wallet: subcommand required (new|list|balance|send)")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "new":
		return walletNew(rest)
	case "list":
		return walletList()
	case "balance":
		return walletBalance(rest)
	case "send":
		return walletSend(rest)
	case "default":
		return walletDefault(rest)
	}
	return fmt.Errorf("wallet: unknown subcommand %q", sub)
}

func walletNew(args []string) error {
	fs := flag.NewFlagSet("wallet new", flag.ExitOnError)
	kt := fs.String("type", "bls", "Key type: bls, secp, delegated")
	fs.Parse(args)
	w, err := openWallet()
	if err != nil {
		return err
	}
	var kkt wallet.KeyType
	switch strings.ToLower(*kt) {
	case "bls":
		kkt = wallet.KTBLS
	case "secp", "secp256k1":
		kkt = wallet.KTSecp256k1
	case "delegated", "f4":
		kkt = wallet.KTDelegated
	default:
		return fmt.Errorf("unknown key type %q", *kt)
	}
	a, err := w.NewAddress(context.Background(), kkt)
	if err != nil {
		return err
	}
	fmt.Println(a.String())
	return nil
}

func walletList() error {
	w, err := openWallet()
	if err != nil {
		return err
	}
	addrs, err := w.List(context.Background())
	if err != nil {
		return err
	}
	def, _ := w.Default(context.Background())
	for _, a := range addrs {
		marker := "  "
		if a == def {
			marker = "* "
		}
		fmt.Println(marker + a.String())
	}
	if len(addrs) == 0 {
		fmt.Println("(no wallet keys; try `lantern wallet new --type=bls`)")
	}
	return nil
}

func walletDefault(args []string) error {
	w, err := openWallet()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		def, err := w.Default(context.Background())
		if err != nil {
			return err
		}
		fmt.Println(def)
		return nil
	}
	a, err := addr.NewFromString(args[0])
	if err != nil {
		return err
	}
	return w.SetDefault(context.Background(), a)
}

func walletBalance(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lantern wallet balance <addr>")
	}
	a, err := addr.NewFromString(args[0])
	if err != nil {
		return err
	}
	ctx := context.Background()
	tr, err := fetchTrustedHead(ctx, defaultGateway)
	if err != nil {
		return err
	}
	bg, _ := gatewayClient(defaultGateway)
	chainAPI := handlers.New(tr, bg, nil, nil, "mainnet")
	bal, err := chainAPI.WalletBalance(ctx, a)
	if err != nil {
		return err
	}
	fmt.Println(types.FIL(bal).String())
	return nil
}

func walletSend(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: lantern wallet send <to> <amount-FIL>")
	}
	to, err := addr.NewFromString(args[0])
	if err != nil {
		return err
	}
	fil, err := types.ParseFIL(args[1])
	if err != nil {
		return fmt.Errorf("parse amount: %w", err)
	}
	w, err := openWallet()
	if err != nil {
		return err
	}
	from, err := w.Default(context.Background())
	if err != nil {
		return err
	}
	tr, err := fetchTrustedHead(context.Background(), defaultGateway)
	if err != nil {
		return err
	}
	bg, _ := gatewayClient(defaultGateway)
	chainAPI := handlers.New(tr, bg, w, nil, "mainnet")
	nonce, _ := chainAPI.MpoolGetNonce(context.Background(), from)

	msg := &types.Message{
		From:       from,
		To:         to,
		Value:      big.Int(fil),
		Method:     0,
		Nonce:      nonce,
		GasLimit:   10_000_000,
		GasFeeCap:  big.NewInt(100_000_000),
		GasPremium: big.NewInt(100_000),
	}

	fmt.Println("--- DRY RUN ---")
	fmt.Println("Would send the following message:")
	b, _ := json.MarshalIndent(msg, "", "  ")
	fmt.Println(string(b))
	fmt.Println()
	fmt.Println("Type 'send' to broadcast (or anything else to abort):")
	rdr := bufio.NewReader(os.Stdin)
	line, _ := rdr.ReadString('\n')
	if strings.TrimSpace(line) != "send" {
		fmt.Println("aborted")
		return nil
	}

	sm, err := chainAPI.WalletSignMessage(context.Background(), from, msg)
	if err != nil {
		return err
	}
	cid, err := chainAPI.MpoolPush(context.Background(), sm)
	if err != nil {
		fmt.Println("WARN:", err)
		fmt.Println("Signed message CID (would-be):", sm.Cid())
		return nil
	}
	fmt.Println("sent:", cid)
	return nil
}

// --- chain ---

func cmdChain(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("chain: subcommand required (head)")
	}
	switch args[0] {
	case "head":
		ctx := context.Background()
		tr, err := fetchTrustedHead(ctx, defaultGateway)
		if err != nil {
			return err
		}
		bg, _ := gatewayClient(defaultGateway)
		chainAPI := handlers.New(tr, bg, nil, nil, "mainnet")
		ts, err := chainAPI.ChainHead(ctx)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(ts, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	return fmt.Errorf("chain: unknown subcommand %q", args[0])
}

// --- state ---

func cmdState(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("state: subcommand required (get-actor)")
	}
	switch args[0] {
	case "get-actor":
		if len(args) < 2 {
			return fmt.Errorf("usage: lantern state get-actor <addr>")
		}
		a, err := addr.NewFromString(args[1])
		if err != nil {
			return err
		}
		ctx := context.Background()
		tr, err := fetchTrustedHead(ctx, defaultGateway)
		if err != nil {
			return err
		}
		bg, _ := gatewayClient(defaultGateway)
		chainAPI := handlers.New(tr, bg, nil, nil, "mainnet")
		act, err := chainAPI.StateGetActor(ctx, a, types.TipSetKey{})
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(act, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	return fmt.Errorf("state: unknown subcommand %q", args[0])
}

// --- info ---

func cmdInfo(_ []string) error {
	dir := dataDir()
	fmt.Println("Lantern info")
	fmt.Println("============")
	fmt.Println("Data dir:", dir)

	// Read the persisted admin token, if any.
	tok, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		fmt.Println("Admin token: (not initialised — run `lantern init`)")
	} else {
		s := strings.TrimSpace(string(tok))
		short := s
		if len(short) > 20 {
			short = short[:10] + "..." + short[len(short)-6:]
		}
		fmt.Printf("Admin token: %s\n", short)
		fmt.Printf("FULLNODE_API_INFO (assuming daemon on 127.0.0.1:1234):\n  %s:/ip4/127.0.0.1/tcp/1234/http\n", s)
	}

	// Probe local daemon.
	hc := &http.Client{Timeout: 1 * time.Second}
	resp, err := hc.Get("http://127.0.0.1:1234/healthz")
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("Daemon healthz: %s", string(body))
	} else {
		fmt.Println("Daemon: not running (no listener on 127.0.0.1:1234)")
	}
	return nil
}
