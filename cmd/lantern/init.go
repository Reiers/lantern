// Phase 11 Part A — multi-source bootstrap quorum.
//
// This file replaces the minimal `lantern init` wizard with the full
// V1.2 GA shape: before touching the wallet or RPC tokens, run a quorum
// probe across N independent finality sources. If ≥ quorum agree on the
// same finalized tipset, write the result as the trust anchor and
// continue with wallet+JWT setup. If they don't agree, refuse to
// continue and print per-source debug output so the operator can
// diagnose.
//
// See INSTALLER-SPEC.md §3 for the spec this implements.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/filecoin-project/go-f3/gpbft"
	libp2phost "github.com/libp2p/go-libp2p/core/host"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
	"github.com/Reiers/lantern/chain/bootstrap/sources"
	"github.com/Reiers/lantern/chain/trustedroot"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/rpc/handlers"
	"github.com/Reiers/lantern/rpc/server"
	"github.com/Reiers/lantern/wallet"
)

// peerList is a repeatable flag accumulator for --peer.
type peerList []string

func (p *peerList) String() string     { return strings.Join(*p, ",") }
func (p *peerList) Set(v string) error { *p = append(*p, v); return nil }

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	noWallet := fs.Bool("no-wallet", false, "Skip creating a wallet")
	quorum := fs.Int("bootstrap-quorum", 0, "Number of agreeing sources required before writing the trust anchor (≥1). Set to 0 to use the network-default (mainnet=5, calibration=3) or skip the quorum check with --bootstrap-quorum=-1 (NOT recommended outside testing).")
	timeout := fs.Duration("bootstrap-timeout", 60*time.Second, "Total wall-clock budget for the bootstrap quorum")
	gateway := fs.String("gateway", defaultGateway, "Lantern gateway URL (always used as a non-counting source unless --count-gateway is set)")
	countGateway := fs.Bool("count-gateway", false, "Count the Lantern gateway in the quorum tally (default false; not recommended)")
	noLibp2p := fs.Bool("no-libp2p", false, "Skip libp2p sources (use only HTTP RPC sources). Useful for environments without inbound networking.")
	libp2pSettle := fs.Duration("libp2p-settle", 15*time.Second, "Wait this long for libp2p bootstrap connections to settle before running the quorum probe (higher = more reliable first-try quorum on cold boot)")
	network := fs.String("network", "filecoin", "F3 network name. DEPRECATED: prefer --filecoin-network which selects the F3 manifest automatically.")
	filNetwork := fs.String("filecoin-network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration. Drives bootstrap peers, public RPC sources, and F3 manifest selection.")
	var peers peerList
	fs.Var(&peers, "peer", "Additional finality source URL (repeatable). Format: URL or URL|TOKEN")
	fs.Parse(args)

	filNet := build.Network(*filNetwork)
	if !filNet.Valid() {
		return fmt.Errorf("invalid --filecoin-network %q: want one of mainnet, calibration", *filNetwork)
	}
	// Auto-resolve F3 NetworkName from the selected filecoin-network if
	// the caller didn't override --network. Mainnet F3 NetworkName is
	// 'filecoin'; calibration's is 'calibrationnet2' (per the embedded
	// f3manifest_*.json files).
	if *network == "filecoin" && filNet == build.Calibration {
		*network = "calibrationnet2"
	}
	// Resolve the quorum default based on the selected network. Mainnet
	// has 5+ independent public sources; calibration today has 1
	// (Glif calibration). We drop to 3-of-N for calibration to allow
	// libp2p sources to make up the quorum.
	if *quorum == 0 {
		if filNet == build.Calibration {
			*quorum = 3
		} else {
			*quorum = 5
		}
	} else if *quorum < 0 {
		*quorum = 0 // explicit 'skip' signal
	}

	// V1.3 per-network data dir: migrate pre-V1.3 mainnet state to
	// dataDir()/mainnet/ before continuing.
	if err := migrateLegacyDataDir(filNet); err != nil {
		return fmt.Errorf("migrate legacy data dir: %w", err)
	}
	// Stage 2 (#51): use the secrets/ layout for fresh installs too.
	if err := migrateSecretsLayout(filNet); err != nil {
		return fmt.Errorf("migrate secrets layout: %w", err)
	}
	dir := networkDataDir(filNet)
	sdir := secretsDir(filNet)
	printBanner(dir)
	fmt.Printf("▸ Filecoin network: %s  (F3 NetworkName: %s)\n", filNet, *network)
	fmt.Printf("  data dir:        %s\n", dir)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(keystorePath(filNet), 0o700); err != nil {
		return err
	}

	// ---------- Bootstrap quorum ----------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *quorum > 0 {
		fmt.Println("▸ Bootstrap quorum — establishing chain head from independent sources")
		fmt.Printf("    Required agreement: %d sources, timeout %s\n", *quorum, *timeout)

		fin, err := runBootstrapQuorum(ctx, bootstrapParams{
			Quorum:       *quorum,
			Timeout:      *timeout,
			Gateway:      *gateway,
			CountGateway: *countGateway,
			NoLibp2p:     *noLibp2p,
			Libp2pSettle: *libp2pSettle,
			NetworkName:  *network,
			Network:      filNet,
			UserPeers:    []string(peers),
			Progress:     prettyProgress,
		})
		if err != nil {
			fmt.Println()
			fmt.Println("✗ Bootstrap quorum FAILED — refusing to write trust anchor.")
			fmt.Println("  Run `lantern doctor` for a detailed per-source report.")
			return err
		}
		if err := writeBootstrapAnchor(dir, fin, filNet); err != nil {
			return fmt.Errorf("persist bootstrap anchor: %w", err)
		}
		fmt.Println()
	} else {
		fmt.Println("▸ Bootstrap quorum skipped (--bootstrap-quorum=0)")
	}

	// ---------- JWT + wallet setup ----------
	fmt.Println("▸ Wallet + JWT setup")
	tr := &trustedroot.TrustedRoot{Epoch: 0}
	dummy := handlers.New(tr, nil, nil, nil, "mainnet")
	srv, err := server.New(server.Config{ListenAddress: "127.0.0.1:0", DataDir: sdir}, dummy)
	if err != nil {
		return fmt.Errorf("init rpc server: %w", err)
	}
	_ = srv
	fmt.Println("    ✓ JWT secret + auth tokens minted under", sdir)
	fmt.Println("      (admin: ./token, sign: ./token-sign, write: ./token-write, read: ./token-read)")

	if *noWallet {
		fmt.Println("    · Skipping wallet creation (--no-wallet).")
	} else {
		w, err := openWallet(filNet)
		if err != nil {
			return err
		}
		a, err := w.NewAddress(context.Background(), wallet.KTBLS)
		if err != nil {
			return err
		}
		fmt.Printf("    ✓ BLS wallet created: %s (default)\n", a)
	}

	fmt.Println()
	fmt.Println("✓ Lantern initialised.")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  lantern daemon                          # start RPC server")
	fmt.Println("  lantern info                            # FULLNODE_API_INFO")
	fmt.Println("  lantern doctor                          # re-run quorum probe")
	return nil
}

func printBanner(dir string) {
	fmt.Println()
	fmt.Println("  🪔  Lantern setup")
	fmt.Println("      Pure-Go Filecoin light node")
	fmt.Println()
	fmt.Println("  Data directory:", dir)
	fmt.Println()
}

// ---------- bootstrap quorum runner (shared by init, doctor, repair) ----------

type bootstrapParams struct {
	Quorum       int
	Timeout      time.Duration
	Gateway      string
	CountGateway bool
	NoLibp2p     bool
	Libp2pSettle time.Duration
	NetworkName  string        // F3 network name (e.g. filecoin, calibrationnet2)
	Network      build.Network // mainnet | calibration; drives source-set selection
	UserPeers    []string
	// Progress is called once per completed source. If nil, no progress
	// output is printed.
	Progress func(bootstrap.SourceResult)
}

// runBootstrapQuorum spins up an ephemeral libp2p host (unless disabled),
// runs the multi-source quorum probe, and returns the winning finality.
func runBootstrapQuorum(ctx context.Context, p bootstrapParams) (bootstrap.Finality, error) {
	if p.Quorum <= 0 {
		return bootstrap.Finality{}, errors.New("quorum must be > 0")
	}
	if p.Timeout <= 0 {
		p.Timeout = 60 * time.Second
	}
	if p.Libp2pSettle <= 0 {
		p.Libp2pSettle = 15 * time.Second
	}

	// 1. Optional libp2p host for cert-exchange sources.
	var host *llibp2p.Host
	if !p.NoLibp2p {
		fmt.Println("    libp2p host: starting...")
		bootPeers := p.Network.BootstrapPeers()
		if len(bootPeers) == 0 {
			bootPeers = build.MainnetBootstrapPeers
		}
		hcfg := llibp2p.HostConfig{
			BootstrapPeers: bootPeers,
			MinPeers:       20,
			MaxPeers:       100,
			UserAgent:      "lantern-bootstrap/0.1",
		}
		h, err := llibp2p.New(ctx, hcfg)
		if err != nil {
			fmt.Printf("    libp2p host: %v (falling back to HTTP sources only)\n", err)
		} else {
			host = h
			defer host.Close()
			settleCtx, settleCancel := context.WithTimeout(ctx, p.Libp2pSettle)
			sources.WaitForLibp2pPeers(settleCtx, host.H, 3, 500*time.Millisecond)
			settleCancel()
			fmt.Printf("    libp2p host: peer=%s (%d connections)\n",
				host.ID().String()[:16]+"...", host.PeerCount())
		}
	}

	// 2. Assemble source set. Calibration uses CalibnetPublicForestURLs
	// (single endpoint today: Glif calibration); mainnet uses
	// MainnetPublicForestURLs (Glif + chain.love).
	publicForest := sources.MainnetPublicForestURLs
	bootPeers := build.MainnetBootstrapPeers
	if p.Network == build.Calibration {
		publicForest = sources.CalibnetPublicForestURLs
		bootPeers = build.CalibnetBootstrapPeers
	}
	srcs := sources.BuildDefaultSources(sources.SourceSetConfig{
		Host:                  hostHandle(host),
		MainnetBootstrapPeers: bootPeers,
		PublicForestURLs:      publicForest,
		LanternGatewayURL:     p.Gateway,
		IncludeGatewayProbe:   p.CountGateway,
		UserPeerURLs:          p.UserPeers,
		NetworkName:           gpbftNetworkName(p.NetworkName),
		SourceTimeout:         min(p.Timeout, 25*time.Second),
	})

	fmt.Printf("    %d sources assembled (libp2p=%d, forest=%d, user=%d, gateway=%d)\n",
		len(srcs), countKind(srcs, bootstrap.KindLibp2p),
		countKind(srcs, bootstrap.KindForest),
		countKind(srcs, bootstrap.KindUser),
		countKind(srcs, bootstrap.KindLanternGateway))

	// 3. Run the quorum. Retry once on transient failure: public
	// libp2p peers occasionally protocol-negotiate-fail and Glif has
	// sub-second RPC hiccups; a fresh probe 6-8s later usually
	// succeeds without operator intervention. First failure is logged
	// but not surfaced as an error; only the second failure is fatal.
	const maxAttempts = 2
	var res *bootstrap.QuorumResult
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return bootstrap.Finality{}, ctx.Err()
			case <-time.After(6 * time.Second):
			}
			fmt.Println()
			fmt.Printf("    retry %d/%d: transient peer flake on first pass, probing again...\n", attempt, maxAttempts)
		}
		res, err = bootstrap.Quorum(ctx, srcs, bootstrap.QuorumOptions{
			Quorum:       p.Quorum,
			Timeout:      p.Timeout,
			CountGateway: p.CountGateway,
			Progress:     p.Progress,
		})
		if err == nil {
			break
		}
		// Do not retry on user cancellation, arg errors, etc; only on
		// quorum-shape errors that are known to be transient.
		if !errors.Is(err, bootstrap.ErrQuorumNotReached) && !errors.Is(err, bootstrap.ErrInsufficientSources) {
			break
		}
	}
	fmt.Println()
	fmt.Print(bootstrap.FormatReport(res))
	if err != nil {
		return bootstrap.Finality{}, err
	}
	return res.Winning, nil
}

func hostHandle(h *llibp2p.Host) libp2phost.Host {
	if h == nil {
		return nil
	}
	return h.H
}

func countKind(ss []bootstrap.Source, k bootstrap.Kind) int {
	n := 0
	for _, s := range ss {
		if s.Kind() == k {
			n++
		}
	}
	return n
}

// prettyProgress prints a per-source ✓/✗ line as each source finishes.
func prettyProgress(r bootstrap.SourceResult) {
	mark := "  ✓"
	tail := r.Finality.String()
	if r.Error != nil {
		mark = "  ✗"
		tail = truncateErr(r.Error.Error(), 80)
	} else if !r.Counted {
		mark = "  ·"
		tail = tail + " (not counted)"
	}
	fmt.Printf("%s [%s] %s — %s (%s)\n", mark, r.Kind, r.Name, tail, r.Duration.Round(time.Millisecond))
}

func truncateErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// gpbftNetworkName takes a string from --network and returns a
// gpbft.NetworkName for the cert-exchange protocol ID.
func gpbftNetworkName(s string) gpbft.NetworkName {
	return gpbft.NetworkName(s)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// ---------- on-disk trust anchor ----------

// BootstrapAnchor is the JSON document written to
// ~/.lantern/bootstrap-anchor.json after a successful quorum probe. It
// is *not* a substitute for the embedded F3 anchor (which seeds power-
// table verification); it records the validated chain head so
// subsequent runs can sanity-check that the swarm hasn't diverged.
type BootstrapAnchor struct {
	Instance   uint64    `json:"instance"`
	Epoch      int64     `json:"epoch"`
	TipSetKey  []string  `json:"tipsetKey"`
	StateRoot  string    `json:"stateRoot"`
	CapturedAt time.Time `json:"capturedAt"`
	Network    string    `json:"network"`
}

func writeBootstrapAnchor(dir string, f bootstrap.Finality, network build.Network) error {
	tsks := make([]string, len(f.TipSetKey))
	for i, c := range f.TipSetKey {
		tsks[i] = c.String()
	}
	netStr := string(network)
	if netStr == "" {
		netStr = string(build.DefaultNetwork)
	}
	a := BootstrapAnchor{
		Instance:   f.Instance,
		Epoch:      f.Epoch,
		TipSetKey:  tsks,
		StateRoot:  f.StateRoot.String(),
		CapturedAt: time.Now().UTC(),
		Network:    netStr,
	}
	raw, err := json.MarshalIndent(&a, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "bootstrap-anchor.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	fmt.Printf("    ✓ Trust anchor written: %s\n", path)
	return nil
}

// ReadBootstrapAnchor reads a previously-written anchor. Returns
// (nil, nil) if no anchor file exists yet.
func ReadBootstrapAnchor(dir string) (*BootstrapAnchor, error) {
	path := filepath.Join(dir, "bootstrap-anchor.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var a BootstrapAnchor
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}
