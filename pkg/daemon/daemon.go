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

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/trustedroot"
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

	// VMBridgeRPC is an upstream Forest/Lotus JSON-RPC URL for the VM
	// bridge (needed for AllowBlockSubmit=true). Empty disables bridge.
	VMBridgeRPC string

	// VMBridgeToken is the optional Bearer token for the bridge.
	VMBridgeToken string

	// VMBridgeTimeout caps each bridge call. Default 30s.
	VMBridgeTimeout time.Duration

	// AllowBlockSubmit lifts the SyncSubmitBlock gate. Requires VMBridgeRPC.
	AllowBlockSubmit bool

	// Network is a label baked into version strings + dashboards.
	// Default "mainnet".
	Network string

	// EmbeddedMode is set by callers (e.g. Curio Core) that want to
	// suppress some CLI-shaped stdout/stderr noise. Functionally no-op
	// otherwise.
	EmbeddedMode bool
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
		c.Network = "mainnet"
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

// Start brings the daemon up: fetches the trusted head, opens the
// header store, brings up libp2p + gossipsub, mounts the JSON-RPC server,
// and (optionally) the metrics + dashboard endpoints. Blocks until ctx
// is cancelled or a fatal startup error occurs.
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
