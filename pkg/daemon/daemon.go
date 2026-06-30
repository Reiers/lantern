// Package daemon is Lantern's embeddable daemon library.
//
// The Lantern CLI in cmd/lantern is one consumer. Other Go programs that
// want a full Lantern node inside their own process (notably Curio Core,
// see Reiers/lantern#11) are the other consumers. The boundary:
//
//   - cmd/lantern owns user-facing wiring: flag parsing, wallet
//     passphrase prompts, signal handling, the install + service flows.
//   - pkg/daemon owns the actual node: TrustedRoot capture, header
//     store, libp2p host, gossipsub block ingestor, RPC server,
//     metrics endpoint, dashboard.
//
// Embedding example:
//
//	d, err := daemon.New(daemon.Config{
//	    Gateway:     "https://gateway.lantern.reiers.io",
//	    DataDir:     "/var/lib/lantern",
//	    RPCListen:   "127.0.0.1:1234",
//	    Wallet:      myWallet,
//	    Passphrase:  "..."  // optional, for keystore decryption on Start
//	})
//	if err != nil { ... }
//	go d.Start(ctx)
//	defer d.Stop(context.Background())
//	// d.RPCAddr(), d.Host(), d.TrustedRoot() now available
//
// Concurrency: Start blocks until ctx is cancelled or a fatal error
// occurs. Stop is idempotent and safe to call concurrently with Start.

package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/filecoin-project/go-jsonrpc/auth"
	"github.com/filecoin-project/go-state-types/abi"

	lapi "github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/headcheck"
	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/headnotify"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/blockingest"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/net/mpool"
	"github.com/Reiers/lantern/rpc/handlers"
	rpcserver "github.com/Reiers/lantern/rpc/server"
	"github.com/Reiers/lantern/state/prefetch"
	"github.com/Reiers/lantern/wallet"
)

// Config holds every knob the embedded daemon exposes. All fields are
// optional except DataDir + Wallet. Zero values get the same defaults
// the CLI would use.
type Config struct {
	// Gateway is the Lantern gateway URL used for state-tree backfill +
	// chain-head bootstrap. Defaults to https://gateway.lantern.reiers.io.
	Gateway string

	// DataDir is the directory the daemon reads + writes (header store,
	// JWT secret, tokens). REQUIRED.
	DataDir string

	// Wallet is the keystore-backed wallet used to sign messages. REQUIRED.
	// Callers that don't need signing (read-only embedding) can pass a
	// wallet with no keys.
	Wallet *wallet.Wallet

	// Passphrase is the keystore decryption passphrase. Optional when
	// Wallet was constructed already-unlocked.
	Passphrase string

	// RPCListen is the JSON-RPC server bind address. Default 127.0.0.1:1234.
	RPCListen string

	// MetricsListen is the optional Prometheus + dashboard bind address.
	// Default empty (no metrics endpoint).
	MetricsListen string

	// P2PListen is comma-separated libp2p listen multiaddrs. Default
	// "/ip4/0.0.0.0/tcp/0,/ip4/0.0.0.0/udp/0/quic-v1". Set to "" to
	// disable the libp2p host entirely (RPC stays up, Net* RPCs return
	// empty data).
	P2PListen string

	// NoHeaderStore disables the persistent header store. Useful for
	// short-lived embedded uses where ChainNotify history isn't needed.
	NoHeaderStore bool

	// HeaderStorePath overrides the default Badger location ($DataDir/headerstore).
	HeaderStorePath string

	// SyncInterval is the header-store poll cadence. Default 6s.
	SyncInterval time.Duration

	// NotifyBufSize is the ChainNotify per-subscriber buffer. Default 64.
	NotifyBufSize int

	// NoLibp2p disables libp2p host startup. Equivalent to P2PListen="".
	NoLibp2p bool

	// BitswapEnabled inserts Bitswap between cache and HTTP gateway.
	// Default true.
	BitswapEnabled bool

	// BitswapFastDeadline is the preferred-peer Bitswap stage deadline.
	// Default 1.5s.
	BitswapFastDeadline time.Duration

	// BitswapFullDeadline is the swarm-broadcast Bitswap stage deadline.
	// Default 5s.
	BitswapFullDeadline time.Duration

	// BitswapPeers is a comma-separated multiaddr list of always-keep-
	// connected Bitswap providers (beacon nodes typically).
	BitswapPeers string

	// FallbackRPC overrides the Lotus-compatible RPC URL used as the
	// polling Sync head source and the cold state-block fallback. Empty
	// uses the built-in Glif URL for the active network (the historical
	// default). Point this at your own Forest/Lotus node to remove the
	// Glif dependency without going fully bridge-off (lantern#50 part 3).
	FallbackRPC string

	// HeadCheckRPCs is an optional list of Lotus-compatible JSON-RPC URLs
	// used by the running-head divergence monitor (chain/headcheck,
	// snadrus#85) to CORROBORATE the gossip-derived head. These are never
	// the source of truth for the head - they only raise an eclipse/fork
	// alarm when a diversity of independent observers disagrees with our
	// head beyond the 3-block lookback. Empty disables the monitor.
	HeadCheckRPCs []string

	// NoFallbackRPC, when true, wires NO upstream RPC as the Sync head
	// source or cold-block fallback - the node relies purely on gossipsub
	// for the head and Bitswap for cold blocks (lantern#50 part 3). This
	// makes the bridge-off trust posture EXPLICIT: under the old default a
	// bridge-off node silently fell back to Glif whenever gossip stalled,
	// a hidden third-party dependency. With this set, a gossip stall
	// surfaces as a stalled head (observable) instead of a silent Glif
	// fetch. Intended for operators who have a healthy swarm/beacon set
	// and want a provably-Glif-free node. Overrides FallbackRPC.
	NoFallbackRPC bool

	// VMBridgeRPC is an upstream Forest/Lotus JSON-RPC URL for the VM
	// bridge (needed for AllowBlockSubmit=true). Empty disables bridge.
	VMBridgeRPC string

	// VMBridgeToken is the optional Bearer token for the bridge.
	VMBridgeToken string

	// VMBridgeTimeout caps each bridge call. Default 30s.
	VMBridgeTimeout time.Duration

	// AllowBlockSubmit lifts the SyncSubmitBlock gate. Requires VMBridgeRPC.
	AllowBlockSubmit bool

	// Network selects the Filecoin network the embedded daemon syncs.
	// Accepted values: "mainnet" (default), "calibration". Drives the
	// gateway URL, Glif fallback URL, bootstrap peers, network name,
	// genesis CID, F3 manifest, and the Filecoin.Version label.
	Network string

	// EmbeddedMode is set by callers (e.g. Curio Core) that want to
	// suppress some CLI-shaped stdout/stderr noise. Functionally no-op
	// otherwise.
	EmbeddedMode bool

	// FEVMPrefetchAddrs is the optional list of EVM contract addresses
	// (20-byte hex, 0x-prefixed or bare) whose state subtrees should be
	// warmed into the local blockstore cache on every head advance, so
	// later eth_calls hit the cache instead of falling back to the VM
	// bridge or returning "block not found" (lantern#44).
	//
	// Embedded callers should set this to the proxy + impl addresses of
	// the contracts they read (PDPVerifier, FWSS, ServiceProviderRegistry,
	// USDFC, ...). The standalone daemon leaves this empty by default;
	// curio-core will wire defaults from its pdp/contract/addresses.go.
	FEVMPrefetchAddrs []string

	// FEVMPrefetchMaxBlocksPerAddr caps the BFS node-count per address
	// per head advance. Default 256.
	FEVMPrefetchMaxBlocksPerAddr int

	// FEVMPrefetchPerAddrTimeout bounds one address's walk. Default 20s.
	FEVMPrefetchPerAddrTimeout time.Duration

	// FEVMPrefetchMinInterval coalesces rapid head advances: each
	// address is walked at most once per MinInterval. Default 60s.
	FEVMPrefetchMinInterval time.Duration

	// FEVMFetchRetries / FEVMFetchTimeout control the retry-on-miss
	// wrapper used by the eth_call backend for bytecode + KAMT storage
	// reads (lantern#44). Zero values pick sensible defaults
	// (2 retries / 8s total). Set FEVMFetchRetries < 0 to disable.
	FEVMFetchRetries int
	FEVMFetchTimeout time.Duration
}

// applyDefaults populates zero-value fields with the same defaults
// cmd/lantern's CLI uses.
func (c *Config) applyDefaults() {
	if c.Gateway == "" {
		c.Gateway = "https://gateway.lantern.reiers.io"
	}
	if c.RPCListen == "" {
		c.RPCListen = "127.0.0.1:1234"
	}
	if c.P2PListen == "" && !c.NoLibp2p {
		c.P2PListen = "/ip4/0.0.0.0/tcp/0,/ip4/0.0.0.0/udp/0/quic-v1"
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = 6 * time.Second
	}
	if c.NotifyBufSize <= 0 {
		c.NotifyBufSize = 64
	}
	if c.BitswapFastDeadline <= 0 {
		c.BitswapFastDeadline = 1500 * time.Millisecond
	}
	if c.BitswapFullDeadline <= 0 {
		c.BitswapFullDeadline = 5 * time.Second
	}
	if c.VMBridgeTimeout <= 0 {
		c.VMBridgeTimeout = 30 * time.Second
	}
	if c.Network == "" {
		c.Network = string(build.DefaultNetwork)
	}

	// Pick a per-network default gateway when the caller didn't set one
	// explicitly. Mainnet uses the Lantern gateway; calibration falls back
	// to Glif calibration HTTP because there's no Lantern gateway running
	// against calibration yet.
	if c.Gateway == "https://gateway.lantern.reiers.io" && build.Network(c.Network) == build.Calibration {
		c.Gateway = "https://api.calibration.node.glif.io/rpc/v1"
	}
}

func (c *Config) validate() error {
	if c.DataDir == "" {
		return errors.New("daemon.Config: DataDir is required")
	}
	if c.Wallet == nil {
		return errors.New("daemon.Config: Wallet is required (pass an empty wallet for read-only embedding)")
	}
	if c.AllowBlockSubmit && c.VMBridgeRPC == "" {
		return errors.New("daemon.Config: AllowBlockSubmit requires VMBridgeRPC")
	}
	if !build.Network(c.Network).Valid() {
		return errors.New("daemon.Config: Network must be \"mainnet\" or \"calibration\"")
	}
	return nil
}

// Daemon is a running Lantern node. Construct via New, then Start.
type Daemon struct {
	cfg Config

	mu       sync.Mutex
	started  bool
	stopping bool
	stopped  chan struct{}
	stopErr  error

	// runtime state, populated by Start (read-only after Start returns nil).
	tr      *trustedroot.TrustedRoot
	rpcAddr string

	// Optional runtime subsystems. nil when the relevant Config field
	// disabled the subsystem or it hasn't been wired yet.
	rpcServer *rpcserver.Server
	auth      *rpcserver.Auth

	// Header store + head-change distributor + sync agent. Populated
	// when !cfg.NoHeaderStore. The distributor backs ChainNotify
	// (HTTP-RPC subscribers via the chainAPI handler) AND the in-
	// process HeadChanges() accessor that embedded consumers use to
	// bypass the HTTP/JSON-RPC out-channel restriction.
	headerStore *hstore.Store
	headerSync  *hstore.Sync
	headNotify  *headnotify.Distributor

	// fevmPrefetch is the on-head-advance state-block warmer
	// (lantern#44). Populated when Config.FEVMPrefetchAddrs is set.
	// Nil-able; Stats() on a nil-receiver returns zero values.
	fevmPrefetch *prefetch.Prefetcher

	// chainAPI is the live JSON-RPC handler. Populated by startInternal
	// after the header store + bridge are wired. Exposed via accessors
	// like LocalEthCallStats (lantern#44) so embedded callers can read
	// counters without touching the RPC server.
	chainAPI *handlers.ChainAPI

	// libp2p host + gossipsub block ingestor. Populated when libp2p is
	// enabled (P2PListen != "" && !NoLibp2p) and a header store is wired.
	// When present, gossipsub is the primary head source (0-1 epoch
	// latency) and headerSync runs at a relaxed cadence as the catch-up
	// fallback. See lantern#40.
	p2pHost   *llibp2p.Host
	ingestor  *blockingest.Ingestor
	mpool     *mpool.Pool        // gossipsub mempool publisher (#45 Stage 4)
	headcheck *headcheck.Monitor // running-head divergence monitor (#85)
	bitswap   *bitswap.Client    // libp2p block source on the embedded fetcher (#50)

	// sendWarmer pre-warms a sent tx's message/receipt blocks into the
	// Bitswap cache in the background so the receipt poll resolves locally
	// (lantern#50 prefetch-on-send). Wired to chainAPI.OnSentTx in
	// startInternal; best-effort, read-only.
	sendWarmer *sendWarmer

	// Internal cancellation: derived from caller's ctx in Start.
	cancel context.CancelFunc
}

// New constructs a Daemon from a Config. Validates required fields.
// Does NOT start the network; call Start.
func New(cfg Config) (*Daemon, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Daemon{
		cfg:     cfg,
		stopped: make(chan struct{}),
	}, nil
}

// Config returns the effective configuration (with defaults applied).
func (d *Daemon) Config() Config { return d.cfg }

// HeadChanges returns a channel of head-change events from the
// in-process distributor. The first event is always {Type:"current"}
// with the current head (or nil if the store hasn't observed a head
// yet). Cancelling ctx unsubscribes and closes the channel.
//
// Returns nil when NoHeaderStore=true (no distributor wired) OR when
// Start has not yet completed.
//
// Embedded consumers (curio-core) call this to drive chain-sched
// event loops without going through the JSON-RPC ChainNotify path
// (which can't carry channels over HTTP POST). External consumers
// reach the same distributor via the standard JSON-RPC ChainNotify
// when the RPC server is upgraded to WebSocket transport.
func (d *Daemon) HeadChanges(ctx context.Context) <-chan []lapi.HeadChange {
	d.mu.Lock()
	dist := d.headNotify
	d.mu.Unlock()
	if dist == nil {
		return nil
	}
	return dist.Subscribe(ctx)
}

// RPCAddr returns the resolved RPC listen address. Only valid after
// Start has returned nil (or after Started() returns true).
func (d *Daemon) RPCAddr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rpcAddr
}

// TrustedRoot returns the daemon's captured anchor. Only valid after
// Start has returned nil. Returns nil before that.
func (d *Daemon) TrustedRoot() *trustedroot.TrustedRoot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tr
}

// Started reports whether Start has finished bringing the daemon up.
func (d *Daemon) Started() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.started
}

// HeadEpoch returns the current head epoch from the captured TrustedRoot.
// Returns 0 if Start hasn't completed.
func (d *Daemon) HeadEpoch() abi.ChainEpoch {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tr == nil {
		return 0
	}
	return d.tr.Epoch
}

// Host returns the libp2p host, or nil when libp2p is disabled
// (NoLibp2p / empty P2PListen) or Start hasn't completed. See #40.
func (d *Daemon) Host() *llibp2p.Host {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.p2pHost
}

// FEVMPrefetchStats returns a snapshot of the FEVM state-block
// prefetcher's counters and true when the prefetcher is wired.
// Returns (zero, false) when the merged warm-set (built-in + consumer)
// was empty, so the prefetcher never started (lantern#44, #69).
func (d *Daemon) FEVMPrefetchStats() (prefetch.Stats, bool) {
	d.mu.Lock()
	pf := d.fevmPrefetch
	d.mu.Unlock()
	if pf == nil {
		return prefetch.Stats{}, false
	}
	return pf.Stats(), true
}

// LocalEthCallStats returns a snapshot of the local-eth_call counters
// (lantern#44). A healthy embedded daemon with state-block availability
// should approach Served/Total = 1.0. Returns (zero, false) when the
// daemon hasn't reached the point where ChainAPI is wired (i.e. before
// Start completes), so callers can poll safely.
func (d *Daemon) LocalEthCallStats() (handlers.LocalEthCallStatsView, bool) {
	d.mu.Lock()
	ch := d.chainAPI
	d.mu.Unlock()
	if ch == nil {
		return handlers.LocalEthCallStatsView{}, false
	}
	return ch.LocalEthCallStatsView(), true
}

// GossipStats returns a snapshot of gossipsub block-ingestor counters
// and true when gossipsub head-tracking is active. Returns (zero, false)
// when running on the polling Sync alone. Useful for verifying the
// 0-1 epoch latency soak in #40.
func (d *Daemon) GossipStats() (blockingest.Stats, bool) {
	d.mu.Lock()
	ing := d.ingestor
	d.mu.Unlock()
	if ing == nil {
		return blockingest.Stats{}, false
	}
	return ing.Stats(), true
}

// Start brings the daemon up: fetches the trusted head, opens the
// header store, brings up libp2p + gossipsub, mounts the JSON-RPC server,
// and (optionally) the metrics + dashboard endpoints. Blocks until ctx
// is cancelled or a fatal startup error occurs.
//
// AdminToken returns the pre-minted admin-scope JWT for the embedded
// RPC server. Only valid after Start has returned nil (or Started()
// returns true) and only when the RPC server was actually mounted
// (RPCListen != "" in Config). Returns "" otherwise.
//
// Embedded callers (notably curio-core) use this to self-issue a
// token against the in-process Lantern without going through disk
// files or environment variables.
func (d *Daemon) AdminToken() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.auth == nil {
		return ""
	}
	return d.auth.Token(lapi.PermAdmin)
}

// MintToken issues a fresh JWT with the requested permissions.
// Use AdminToken() when the pre-minted admin token suffices; use this
// only when a narrower scope is needed (e.g. minting a read-only token
// for a constrained consumer). Returns an error if the RPC server
// hasn't been mounted yet.
func (d *Daemon) MintToken(perms []auth.Permission) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.auth == nil {
		return nil, errors.New("daemon: rpc server not mounted; MintToken unavailable")
	}
	return d.auth.AuthNew(perms)
}

// FullNodeAPIInfo returns the Lotus-compatible FULLNODE_API_INFO string
// for the admin-scoped token, ready to be set into the environment by
// embedded callers running a Lotus-API client. Only valid after Start
// has mounted the RPC server.
func (d *Daemon) FullNodeAPIInfo() (string, error) {
	d.mu.Lock()
	srv := d.rpcServer
	d.mu.Unlock()
	if srv == nil {
		return "", errors.New("daemon: rpc server not mounted; FullNodeAPIInfo unavailable")
	}
	return srv.FullNodeAPIInfo()
}

// Original Start doc continues:
//
// On a clean ctx cancellation, Start returns nil. On startup failure,
// Start returns the error and the daemon is half-built (caller should
// still call Stop to clean up partial state).
//
// Concurrency: not safe to call twice on the same Daemon.
func (d *Daemon) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started || d.stopping {
		d.mu.Unlock()
		return errors.New("daemon: already started or stopping")
	}
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()

	// The actual wiring lives in start.go (built incrementally; the
	// initial version of pkg/daemon brings up just enough to be useful
	// to Curio Core's recon work).
	if err := d.startInternal(ctx); err != nil {
		return err
	}

	d.mu.Lock()
	d.started = true
	d.mu.Unlock()

	// Block until the caller cancels.
	<-ctx.Done()
	return d.stopInternal(context.Background())
}

// Stop signals the daemon to shut down. Returns nil if the daemon was
// not running. Otherwise blocks (with a 5s timeout) until shutdown is
// complete or ctx is cancelled.
func (d *Daemon) Stop(ctx context.Context) error {
	d.mu.Lock()
	if !d.started || d.stopping {
		d.mu.Unlock()
		return nil
	}
	d.stopping = true
	cancel := d.cancel
	d.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	select {
	case <-d.stopped:
		return d.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
