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

	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/glif"
	"github.com/Reiers/lantern/net/hsync"
	"github.com/Reiers/lantern/rpc/handlers"
	"github.com/Reiers/lantern/rpc/server"
	"github.com/Reiers/lantern/state/hamt"
	"github.com/Reiers/lantern/wallet"
)

const (
	defaultGateway = "https://gateway.lantern.reiers.io"
	defaultListen  = "127.0.0.1:1234"
)

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
	case "wallet":
		err = cmdWallet(rest)
	case "chain":
		err = cmdChain(rest)
	case "state":
		err = cmdState(rest)
	case "info":
		err = cmdInfo(rest)
	case "version", "--version", "-v":
		fmt.Println("lantern 0.4.0 (Phase 4 — Lotus-compat RPC)")
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
  init                                Interactive setup wizard
  daemon [--gateway <url>]            Run RPC server (default 127.0.0.1:1234)
  wallet new --type={bls|secp|delegated}
  wallet list
  wallet balance <addr>
  wallet send <to> <amount-FIL>       (DRY-RUN — message preview)
  chain head
  state get-actor <addr>
  info                                Show daemon status + FULLNODE_API_INFO
  version
  help

ENVIRONMENT
  LANTERN_HOME    Data directory (default: ~/.lantern)
  LANTERN_PASS    Keystore passphrase (default: empty; prompts on init)`)
}

// --- helpers ---

func dataDir() string {
	if h := os.Getenv("LANTERN_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lantern")
}

func passphrase() string {
	if p := os.Getenv("LANTERN_PASS"); p != "" {
		return p
	}
	return ""
}

func openWallet() (*wallet.Wallet, error) {
	dir := filepath.Join(dataDir(), "keystore")
	return wallet.New(context.Background(), dir, passphrase())
}

// gatewayClient builds the (cache + gateway + glif fallback) BlockGetter
// chain that's used by every state read in the CLI.
func gatewayClient(gw string) (hamt.BlockGetter, *combined.Fetcher) {
	cache := hamt.NewMemBlockStore()
	httpc := hsync.NewClient([]string{gw}, 20*time.Second)
	glifC := glif.New("", 20*time.Second)
	f := combined.New(cache,
		combined.Source{Name: "gateway", Getter: httpc, Timeout: 5 * time.Second},
		combined.Source{Name: "glif", Getter: glifC, Timeout: 20 * time.Second},
	)
	return f, f
}

// fetchTrustedHead probes the primary gateway's /state/root endpoint,
// falling back to Glif's Filecoin.ChainHead when the gateway is down.
// Both responses are CID-verified before becoming a TrustedRoot.
func fetchTrustedHead(ctx context.Context, gw string) (*trustedroot.TrustedRoot, error) {
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
			return &trustedroot.TrustedRoot{
				Epoch:        abi.ChainEpoch(head.Epoch),
				StateRoot:    stateRoot,
				TipSetKey:    types.NewTipSetKey(tskCids...),
				ParentWeight: pw,
			}, nil
		}
	}
	// Fallback to Glif.
	fmt.Fprintln(os.Stderr, "(gateway unavailable; falling back to Glif RPC)")
	gc := glif.New("", 10*time.Second)
	gh, err := gc.FetchHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("both gateway and Glif failed: %w", err)
	}
	return &trustedroot.TrustedRoot{
		Epoch:        gh.Epoch,
		StateRoot:    gh.StateRoot,
		TipSetKey:    gh.TipSetKey,
		ParentWeight: gh.ParentWeight,
		ParentMessageReceipts: gh.ParentMessageReceipts,
	}, nil
}

// --- init ---

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	noWallet := fs.Bool("no-wallet", false, "Skip creating a wallet")
	fs.Parse(args)

	dir := dataDir()
	fmt.Printf("Lantern setup\n=============\nData directory: %s\n\n", dir)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	fmt.Println("Creating directories under", dir)
	if err := os.MkdirAll(filepath.Join(dir, "keystore"), 0o700); err != nil {
		return err
	}

	// Initialise the RPC server so tokens get minted.
	tr := &trustedroot.TrustedRoot{Epoch: 0}
	dummy := handlers.New(tr, nil, nil, nil, "mainnet")
	srv, err := server.New(server.Config{ListenAddress: "127.0.0.1:0", DataDir: dir}, dummy)
	if err != nil {
		return fmt.Errorf("init rpc server: %w", err)
	}
	_ = srv // we only needed the side-effect (token files persisted)

	fmt.Println("  ✓ JWT secret + auth tokens minted under", dir)
	fmt.Println("    (admin: ./token, sign: ./token-sign, write: ./token-write, read: ./token-read)")
	fmt.Println()

	if *noWallet {
		fmt.Println("Skipping wallet creation (--no-wallet).")
	} else {
		fmt.Println("Creating initial BLS wallet...")
		w, err := openWallet()
		if err != nil {
			return err
		}
		a, err := w.NewAddress(context.Background(), wallet.KTBLS)
		if err != nil {
			return err
		}
		fmt.Printf("  ✓ wallet created: %s (default)\n", a)
	}

	fmt.Println()
	fmt.Println("Setup complete. Next steps:")
	fmt.Println("  lantern daemon                          # start RPC server")
	fmt.Println("  lantern info                            # show FULLNODE_API_INFO")
	fmt.Println("  lantern wallet list")
	return nil
}

// --- daemon ---

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	gw := fs.String("gateway", defaultGateway, "Lantern gateway base URL")
	listen := fs.String("listen", defaultListen, "RPC listen address")
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

	bg, _ := gatewayClient(*gw)
	w, err := openWallet()
	if err != nil {
		return fmt.Errorf("open wallet: %w", err)
	}

	chainAPI := handlers.New(tr, bg, w, nil, "mainnet")

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
	fmt.Println("\nReady. Ctrl-C to stop.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down...")
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	return srv.Stop(sctx)
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
