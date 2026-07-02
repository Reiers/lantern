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
//	lantern wallet export <addr>    — Lotus-hex KeyInfo
//	lantern wallet import [hex|-]
//	lantern wallet import-lotus ~/.lotus
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
	"net"
	"net/http"
	"net/url"
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
	"github.com/Reiers/lantern/chain/anchorverify"
	"github.com/Reiers/lantern/chain/crosscheck"
	"github.com/Reiers/lantern/chain/f3/subscriber"
	"github.com/Reiers/lantern/chain/fullvalidate"
	hstore "github.com/Reiers/lantern/chain/header/store"
	headnotify "github.com/Reiers/lantern/chain/headnotify"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/internal/buildinfo"
	lbitswap "github.com/Reiers/lantern/net/bitswap"
	"github.com/Reiers/lantern/net/blockpub"
	"github.com/Reiers/lantern/net/chainxchg"
	"github.com/Reiers/lantern/net/combined"
	"github.com/Reiers/lantern/net/glif"
	"github.com/Reiers/lantern/net/hello"
	"github.com/Reiers/lantern/net/hsync"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/pkg/nodeprofile"
	"github.com/Reiers/lantern/rpc/handlers"
	"github.com/Reiers/lantern/rpc/server"
	statecache "github.com/Reiers/lantern/state/cache"
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
	setupLogging()
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
	case "reset":
		err = cmdReset(rest)
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
	case "node-type":
		err = cmdNodeType(rest)
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
  reset --chain-state                           Clear rebuildable chain state (headerstore + anchor) so a
                                                long-stopped node re-syncs from live head. NEVER touches keys.
  service {install|uninstall|start|stop|restart|status}
                                                Manage the OS service (launchd / systemd user)
  stop / restart                                Aliases for 'service stop' / 'service restart'
  wallet new --type={bls|secp|delegated}
  wallet list
  wallet balance <addr>
  wallet send <to> <amount-FIL>                 (DRY-RUN — message preview)
  wallet export <addr>                          (Lotus-hex KeyInfo to stdout)
  wallet import [hex|-]                         (Lotus-hex KeyInfo from arg or stdin)
  wallet import-lotus <repo-path>               (bulk import from a Lotus repo keystore)
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

// dataDir returns the BASE Lantern data directory. It is shared across
// networks; per-network state (anchor, header store, keystore, JWT)
// lives under networkDataDir() below.
//
// Service files (launchd plist, systemd unit) live at this level since
// the service manages the whole install, not a specific network.
func dataDir() string {
	if h := os.Getenv("LANTERN_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lantern")
}

// networkDataDir returns the per-network subdirectory under dataDir().
// All network-scoped state (bootstrap-anchor.json, headerstore/,
// keystore/, jwt-secret, API tokens) lives here.
//
// This isolation matters because: chain heads differ between mainnet
// and calibration; signing keys for one chain are meaningless on the
// other; the JWT secret + HMAC'd API tokens are tied to the chain that
// minted them. Sharing the same directory between two networks was a
// silent corruption hazard (lantern#27, discovered 2026-05-23 by
// overwriting the local mainnet anchor with a calibration init).
//
// Caller must pass a valid build.Network. Empty/invalid values default
// to build.DefaultNetwork (mainnet) for backward compatibility with
// callers that haven't been network-converted yet (info, service).
func networkDataDir(n build.Network) string {
	if !n.Valid() {
		n = build.DefaultNetwork
	}
	return filepath.Join(dataDir(), string(n))
}

// migrateLegacyDataDir moves pre-V1.3 top-level state files into the
// per-network subdirectory. Idempotent: if the per-network dir already
// contains state OR the legacy files are gone, this is a no-op.
//
// We migrate INTO the specified network because pre-V1.3 Lantern only
// ran on mainnet. Calibration installs are new and don't have legacy
// state to migrate; mainnet installs get their old state lifted
// automatically.
//
// Migration target is dataDir()/<network>/. Files migrated:
//
//	bootstrap-anchor.json, headerstore/, keystore/, jwt-secret,
//	token, token-read, token-sign, token-write
//
// Anything else at dataDir()/ is left in place (service files, the
// 'lantern' binary if anyone manually dropped one, future top-level
// files).
func migrateLegacyDataDir(n build.Network) error {
	if n != build.Mainnet {
		return nil // pre-V1.3 Lantern was mainnet-only, nothing to migrate elsewhere
	}
	base := dataDir()
	net := networkDataDir(n)

	// If the per-network dir already has an anchor, migration is done.
	if _, err := os.Stat(filepath.Join(net, "bootstrap-anchor.json")); err == nil {
		return nil
	}

	// If NONE of the legacy markers exist at the base level, this is a
	// fresh install — nothing to migrate.
	legacyMarkers := []string{
		"bootstrap-anchor.json", "jwt-secret", "keystore", "headerstore",
	}
	anyLegacy := false
	for _, m := range legacyMarkers {
		if _, err := os.Stat(filepath.Join(base, m)); err == nil {
			anyLegacy = true
			break
		}
	}
	if !anyLegacy {
		return nil
	}

	if err := os.MkdirAll(net, 0o700); err != nil {
		return fmt.Errorf("create network data dir %s: %w", net, err)
	}

	names := []string{
		"bootstrap-anchor.json", "jwt-secret",
		"token", "token-read", "token-sign", "token-write",
		"headerstore", "keystore",
	}
	moved := 0
	for _, name := range names {
		src := filepath.Join(base, name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("stat %s: %w", src, err)
		}
		dst := filepath.Join(net, name)
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("migrate %s -> %s: %w", src, dst, err)
		}
		moved++
	}
	if moved > 0 {
		fmt.Fprintf(os.Stderr, "migrated %d legacy data files from %s to %s\n", moved, base, net)
	}
	return nil
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
// passphraseErrW is where resolvePassphrase writes its prompts/warnings.
// Defaults to os.Stderr; tests override it locally so they don't have to
// swap the global os.Stderr (which races with any concurrent logging,
// e.g. leaked libp2p goroutines under -race).
var passphraseErrW io.Writer = os.Stderr

// emptyPassAllowed reports whether the operator has explicitly acknowledged
// running an unencrypted keystore (#58). Set by LANTERN_ALLOW_EMPTY_PASS=1
// (env, works for systemd/CLI) or the daemon --allow-empty-passphrase flag
// (which sets the env before resolvePassphrase runs).
func emptyPassAllowed() bool {
	v := strings.TrimSpace(os.Getenv("LANTERN_ALLOW_EMPTY_PASS"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// guardEmptyPassphraseWithKeys enforces the #58 fail-loud rule: an empty
// passphrase is refused when the keystore already holds keys (which, on a
// node running the live write path, are funded signing keys) unless the
// operator explicitly opted in. Encrypting nothing is harmless; leaving
// funded keys at rest unencrypted silently is not.
func guardEmptyPassphraseWithKeys(dir string) error {
	if !keystoreHasKeys(dir) {
		return nil // fresh/empty keystore: nothing to protect yet
	}
	if emptyPassAllowed() {
		fmt.Fprintln(passphraseErrW, "\033[33m  warning: keystore holds keys and is UNENCRYPTED (LANTERN_ALLOW_EMPTY_PASS set)\033[0m")
		return nil
	}
	return fmt.Errorf("refusing empty passphrase: keystore at %s already holds keys "+
		"(#58: these may be funded signing keys; an empty passphrase stores them unencrypted). "+
		"Set a real LANTERN_PASS, or set LANTERN_ALLOW_EMPTY_PASS=1 (or --allow-empty-passphrase) to deliberately run unencrypted", dir)
}

func resolvePassphrase(dir string) (string, error) {
	env, envSet := os.LookupEnv("LANTERN_PASS")
	if envSet && env != "" {
		return env, nil
	}
	if envSet && env == "" {
		// Operator set LANTERN_PASS="" to opt out of encryption. Still
		// fail-loud when funded keys exist unless explicitly acknowledged.
		if err := guardEmptyPassphraseWithKeys(dir); err != nil {
			return "", err
		}
		fmt.Fprintln(passphraseErrW, "\033[33m  warning: LANTERN_PASS is set but empty - keystore will be unencrypted\033[0m")
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
		fmt.Fprint(passphraseErrW, "Lantern keystore passphrase: ")
		p, err := readPassword()
		if err != nil {
			return "", fmt.Errorf("read passphrase: %w", err)
		}
		return p, nil
	}

	// Fresh keystore. Prompt twice and require a match.
	fmt.Fprintln(passphraseErrW, "No existing Lantern keystore at", dir)
	fmt.Fprintln(passphraseErrW, "Set a passphrase to encrypt local keys. Press enter without a value to opt out (NOT recommended).")
	fmt.Fprint(passphraseErrW, "New passphrase: ")
	p1, err := readPassword()
	if err != nil {
		return "", fmt.Errorf("read passphrase: %w", err)
	}
	if p1 == "" {
		fmt.Fprintln(passphraseErrW, "\033[33m  warning: keystore will be unencrypted\033[0m")
		return "", nil
	}
	fmt.Fprint(passphraseErrW, "Confirm passphrase: ")
	p2, err := readPassword()
	if err != nil {
		return "", fmt.Errorf("read passphrase: %w", err)
	}
	if p1 != p2 {
		return "", errors.New("passphrases did not match")
	}
	return p1, nil
}

// openWallet opens (or creates) the keystore for the given network.
// Signing keys are per-chain: a key minted on mainnet does NOT sign
// calibration messages and vice versa.
//
// For CLI subcommands that don't yet thread network through, use
// openWalletDefault() which defaults to mainnet for backward compat.
func openWallet(network build.Network) (*wallet.Wallet, error) {
	// Stage 2 (#51): ensure secrets are in <net>/secrets/ before opening.
	// migrateLegacyDataDir is a prerequisite (lifts pre-V1.3 top-level
	// state into <net>/); callers that reach openWallet via the daemon
	// have already run it, but CLI wallet subcommands may not, so run
	// both here idempotently.
	if err := migrateLegacyDataDir(network); err != nil {
		return nil, fmt.Errorf("migrate legacy data dir: %w", err)
	}
	if err := migrateSecretsLayout(network); err != nil {
		return nil, fmt.Errorf("migrate secrets layout: %w", err)
	}
	dir := keystorePath(network)
	p, err := resolvePassphrase(dir)
	if err != nil {
		return nil, err
	}
	return wallet.New(context.Background(), dir, p)
}

// openWalletDefault opens the mainnet keystore. Used by CLI subcommands
// that haven't been network-converted yet (wallet new/list/balance/send,
// chain head, state get-actor). When those subcommands gain a --network
// flag, they should switch to openWallet(net).
func openWalletDefault() (*wallet.Wallet, error) {
	return openWallet(build.Mainnet)
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

// glifURLForNetwork returns the public Glif JSON-RPC endpoint for a
// given Filecoin network. Used by every Glif client constructor in the
// daemon path so a calibration daemon never accidentally pulls
// mainnet chain data (which would corrupt the header store, sync
// poller, gossipsub backfill, and combined-fetcher fallback).
func glifURLForNetwork(n build.Network) string {
	if n == build.Calibration {
		return "https://api.calibration.node.glif.io/rpc/v1"
	}
	return "" // empty -> glif.New uses its DefaultURL (mainnet)
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
func fetchTrustedHead(ctx context.Context, gw string, network build.Network) (*trustedroot.TrustedRoot, error) {
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
	// Fallback to Glif. Network-aware: calibration uses the calibration
	// Glif endpoint; mainnet uses the default.
	glifURL := "" // default mainnet
	if network == build.Calibration {
		glifURL = "https://api.calibration.node.glif.io/rpc/v1"
	}
	fmt.Fprintln(os.Stderr, "(gateway unavailable; falling back to Glif RPC)")
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

// f3RPCURLForNetwork returns the public Glif F3 JSON-RPC endpoint for a
// given network. Used for the best-effort latest-cert probe + the #54
// finality cross-check.
func f3RPCURLForNetwork(n build.Network) string {
	if n == build.Calibration {
		return "https://api.calibration.node.glif.io/rpc/v1"
	}
	return "https://api.node.glif.io/rpc/v1"
}

// attachF3Latest does a best-effort Filecoin.F3GetLatestCertificate probe
// so the dashboard can render F3 instance. Failures are silent; this is
// observability, not chain consensus.
func attachF3Latest(ctx context.Context, tr *trustedroot.TrustedRoot) {
	attachF3LatestForNetwork(ctx, tr, build.Mainnet)
}

func attachF3LatestForNetwork(ctx context.Context, tr *trustedroot.TrustedRoot, network build.Network) {
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	src := subscriber.NewJSONRPCSource(f3RPCURLForNetwork(network))
	cert, err := src.GetLatest(probeCtx)
	if err != nil || cert == nil {
		return
	}
	tr.F3Instance = cert.GPBFTInstance
	tr.F3Cert = cert
}

// --- #54: verified boot anchor ---
//
// gatewayHeadFetcher / glifHeadFetcher adapt the existing block-source
// clients to anchorverify.HeadFetcher so the boot path can gather >=2
// independent views of the head and require agreement.

type gatewayHeadFetcher struct{ gw string }

func (g gatewayHeadFetcher) FetchCandidate(ctx context.Context) (anchorverify.Candidate, error) {
	hc := hsync.NewClient([]string{g.gw}, 5*time.Second)
	head, err := hc.GetStateHead(ctx)
	if err != nil {
		return anchorverify.Candidate{}, fmt.Errorf("gateway head: %w", err)
	}
	sr, err := cid.Parse(head.StateRoot)
	if err != nil {
		return anchorverify.Candidate{}, fmt.Errorf("gateway state root parse: %w", err)
	}
	tskCids := make([]cid.Cid, 0, len(head.TipsetKey))
	for _, s := range head.TipsetKey {
		if c, e := cid.Parse(s); e == nil {
			tskCids = append(tskCids, c)
		}
	}
	pw, _ := big.FromString(head.ParentWeight)
	return anchorverify.Candidate{
		Source:       "gateway",
		Epoch:        abi.ChainEpoch(head.Epoch),
		StateRoot:    sr,
		TipSetKey:    types.NewTipSetKey(tskCids...),
		ParentWeight: pw,
	}, nil
}

type glifHeadFetcher struct{ url string }

func (g glifHeadFetcher) FetchCandidate(ctx context.Context) (anchorverify.Candidate, error) {
	gc := glif.New(g.url, 10*time.Second)
	gh, err := gc.FetchHead(ctx)
	if err != nil {
		return anchorverify.Candidate{}, fmt.Errorf("glif head: %w", err)
	}
	return anchorverify.Candidate{
		Source:       "glif",
		Epoch:        gh.Epoch,
		StateRoot:    gh.StateRoot,
		TipSetKey:    gh.TipSetKey,
		ParentWeight: gh.ParentWeight,
	}, nil
}

// fetchVerifiedTrustedHead is the hardened boot anchor selection (#54).
// It gathers the head from the gateway AND Glif (two independent operators),
// requires them to agree on (StateRoot, TipSetKey), and cross-checks the
// result against the latest F3 finality certificate when one is available.
// On disagreement it prefers the heavier ParentWeight (Filecoin fork choice)
// only when F3 proves no fork-below-finality, and otherwise refuses to boot.
//
// --insecure-anchor (insecure=true) restores the legacy single-source
// behaviour for localhost/dev against a trusted endpoint.
func fetchVerifiedTrustedHead(ctx context.Context, gw string, network build.Network, insecure bool) (*trustedroot.TrustedRoot, error) {
	now := time.Now().UTC()
	pol := anchorverify.Policy{
		MinAgreeingSources:        2,
		InsecureAllowSingleSource: insecure,
		Warnf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "  [anchor] "+format+"\n", args...)
		},
	}

	cands := anchorverify.Gather(ctx, pol,
		gatewayHeadFetcher{gw: gw},
		glifHeadFetcher{url: glifURLForNetwork(network)},
	)

	// Best-effort F3 latest cert for the finality cross-check.
	var f3 anchorverify.F3Finalized
	{
		probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		src := subscriber.NewJSONRPCSource(f3RPCURLForNetwork(network))
		if cert, err := src.GetLatest(probeCtx); err == nil && cert != nil {
			if fin, ferr := anchorverify.FinalizedFromCert(cert); ferr == nil {
				f3 = fin
			}
		}
		cancel()
	}

	res, err := anchorverify.Verify(cands, f3, pol)
	if err != nil {
		// Hard fail: do not silently fall back to trusting one source.
		return nil, fmt.Errorf("boot anchor verification failed (#54): %w "+
			"(set --insecure-anchor to override on a single trusted endpoint)", err)
	}
	fmt.Printf("  anchor:   verified via %s (epoch %d, agreeing sources=%d, f3-checked=%t)\n",
		res.Method, res.Chosen.Epoch, res.AgreeingSources, res.F3Checked)

	tr := &trustedroot.TrustedRoot{
		Epoch:        res.Chosen.Epoch,
		StateRoot:    res.Chosen.StateRoot,
		TipSetKey:    res.Chosen.TipSetKey,
		ParentWeight: res.Chosen.ParentWeight,
		AcceptedAt:   now,
	}
	attachF3LatestForNetwork(ctx, tr, network)
	return tr, nil
}

// --- daemon ---

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	networkFlag := fs.String("network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	gw := fs.String("gateway", defaultGateway, "Lantern gateway base URL")
	listen := fs.String("listen", defaultListen, "RPC listen address")
	noHS := fs.Bool("no-header-store", false, "Disable the persistent header store (legacy synthetic-head mode)")
	hsPath := fs.String("header-store", "", "Header store BadgerDB path (default: <data-dir>/<network>/headerstore)")
	syncInterval := fs.Duration("sync-interval", 6*time.Second, "Header-store sync poll interval")
	staleResetThreshold := fs.Int64("stale-reset-threshold", 2880, "Epochs behind live head past which a persisted header store re-anchors near live head instead of backfilling (#51). 0 disables. Chain state only; keys are never touched.")
	notifyBufSize := fs.Int("notify-buf", headnotify.DefaultBufferSize, "ChainNotify per-subscriber buffer size")
	p2pListen := fs.String("p2p-listen", "/ip4/0.0.0.0/tcp/0,/ip4/0.0.0.0/udp/0/quic-v1", "libp2p listen multiaddrs (comma-separated). Empty disables the libp2p host.")
	noLibp2p := fs.Bool("no-libp2p", false, "Skip starting the libp2p host (RPC stays up; Net* stats return zero).")
	insecureAnchor := fs.Bool("insecure-anchor", false, "SECURITY (#54): accept the boot trusted-root from a single source without multi-source agreement or F3 finality cross-check. Intended for localhost/dev against one trusted endpoint only.")
	allowRemoteRPC := fs.Bool("allow-remote-rpc", false, "SECURITY (#56): permit binding JSON-RPC --listen to a non-loopback address. The RPC holds wallet keys + a signing write path; only loopback is safe without a fronting auth proxy. Off by default — set explicitly to expose the port and rely on Bearer-token perms.")
	allowEmptyPass := fs.Bool("allow-empty-passphrase", false, "SECURITY (#58): deliberately run with an UNENCRYPTED keystore even when it holds keys. Equivalent to LANTERN_ALLOW_EMPTY_PASS=1. Off by default — the daemon refuses an empty passphrase on a keystore that already holds (possibly funded) signing keys.")
	insecureGateway := fs.Bool("insecure-gateway", false, "SECURITY (#55): allow a plain-http:// gateway URL. The boot anchor + cold-block fetches traverse the gateway; without TLS a network attacker can MITM them. Off by default — required only for localhost/dev against an http endpoint.")
	bitswapEnabled := fs.Bool("bitswap", true, "Use Bitswap as primary fetch source (HTTP gateway falls to last resort).")
	bitswapFastDL := fs.Duration("bitswap-fast", 1500*time.Millisecond, "Bitswap fast-stage deadline for preferred peers.")
	bitswapFullDL := fs.Duration("bitswap-full", 5*time.Second, "Bitswap full-stage deadline for swarm broadcast.")
	preferredPeersStr := fs.String("bitswap-peers", "", "Comma-separated multiaddrs to use as preferred Bitswap peers (e.g. lantern beacon nodes).")
	// --metrics: loopback listener that serves Prometheus /metrics AND the
	// embedded operator dashboard at /dashboard/. Default is 127.0.0.1:9092
	// (loopback only) so a fresh `lantern daemon` always has a webui without
	// the operator passing extra flags. Set to empty (`--metrics=`) to
	// disable, or `--no-dashboard` to skip dashboard wiring while keeping
	// /metrics.
	metricsListen := fs.String("metrics", "127.0.0.1:9092", "Listen address for /metrics + /dashboard. Empty string disables both.")
	noDashboard := fs.Bool("no-dashboard", false, "Skip serving /dashboard/; /metrics still served if --metrics is set.")
	allowRemoteDash := fs.Bool("allow-remote-dashboard", false, "SECURITY (#57): permit binding --metrics/dashboard to a non-loopback address. The dashboard exposes node internals + action buttons; only loopback is safe without auth. When bound non-loopback, a LANTERN_DASHBOARD_TOKEN is REQUIRED and enforced as a Bearer token.")

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
	// #80 part 2: head-source corroboration. -1 = auto (Full tier: 2,
	// Light/PDP: off). 0 = off. N>0 = require N distinct forwarding peers
	// (or one trusted floor peer) before adopting a gossip head.
	headCorroPeers := fs.Int("head-corroboration-peers", -1, "Distinct gossip peers required to corroborate a head advance before adoption. -1=auto (Full tier 2, others off), 0=off.")
	// #98: VM-bridge cross-check auditor. Observe-only: compares the local
	// canonical tipset at head-3 against the bridge node once a minute and
	// alarms on divergence. Requires --vm-bridge-rpc.
	vmCrossCheck := fs.Bool("vm-crosscheck", false, "Audit the local chain against the VM bridge node (observe-only divergence alarm). Requires --vm-bridge-rpc.")
	vmCrossCheckInterval := fs.Duration("vm-crosscheck-interval", crosscheck.DefaultInterval, "Interval between VM-bridge cross-checks.")
	// #76 bridge-off: wire NO upstream RPC (Glif) as a runtime fallback.
	// Head comes ONLY from gossipsub; cold blocks ONLY from gateway+Bitswap;
	// the polling Sync + gossip inline-backfill become no-ops. A gossip stall
	// then surfaces as an observable stalled head instead of a silent Glif
	// fetch. Requires libp2p/gossipsub (it is the sole head source). Mirrors
	// pkg/daemon Config.NoFallbackRPC for the standalone CLI. The one-time
	// boot anchor (#54) still uses multi-source agreement (gateway+Glif) and
	// is separate from runtime fetch counters.
	noFallbackRPC := fs.Bool("no-fallback-rpc", false, "#76 bridge-off: wire no upstream RPC (Glif) fallback. Head=gossipsub-only, cold-blocks=gateway+bitswap. Requires libp2p/gossipsub.")
	fs.Parse(args)

	// #58: --allow-empty-passphrase is sugar for the env the keystore guard
	// reads. Set it before any wallet open so the guard sees it.
	if *allowEmptyPass {
		_ = os.Setenv("LANTERN_ALLOW_EMPTY_PASS", "1")
	}

	// #55: reject a plain-http gateway unless explicitly allowed. The boot
	// anchor + cold-block fetches traverse this URL; HTTP has no transport
	// integrity, so a MITM could seed a bad anchor (the CID-verify backstop
	// protects state under a root, not the choice of root — see #54).
	if err := validateGatewayScheme(*gw, *insecureGateway); err != nil {
		return err
	}

	network := build.Network(*networkFlag)
	if !network.Valid() {
		return fmt.Errorf("invalid --network %q: want one of mainnet, calibration", *networkFlag)
	}

	// Propagate the active network into buildinfo so Filecoin.Version,
	// libp2p UserAgent, and other identity surfaces reflect the actual
	// network instead of the package default ('mainnet').
	buildinfo.SetNetwork(network.String())

	// #76 bridge-off guard: with no RPC fallback, gossipsub is the ONLY head
	// source. Refuse to start without libp2p rather than silently freeze the
	// head (mirrors pkg/daemon/start.go).
	if *noFallbackRPC && (*noLibp2p || *p2pListen == "") {
		return fmt.Errorf("--no-fallback-rpc requires libp2p/gossipsub enabled (it is the only head source); set --p2p-listen and do not pass --no-libp2p")
	}

	// V1.3 per-network data dir: migrate any legacy mainnet-only state
	// at dataDir() to dataDir()/mainnet/ on first boot. Idempotent for
	// fresh installs or already-migrated state.
	if err := migrateLegacyDataDir(network); err != nil {
		return fmt.Errorf("migrate legacy data dir: %w", err)
	}
	// Stage 2 (#51): relocate loose secrets into <net>/secrets/ before
	// anything reads them, so keystore + jwt + tokens are physically
	// separated from rebuildable chain state.
	if err := migrateSecretsLayout(network); err != nil {
		return fmt.Errorf("migrate secrets layout: %w", err)
	}
	netDir := networkDataDir(network)
	if err := os.MkdirAll(netDir, 0o700); err != nil {
		return fmt.Errorf("create network data dir: %w", err)
	}
	// Stage 2 (#51): rolling backup of secrets/ on every start. Best-effort
	// — a backup failure must never stop the daemon. Gives a same-machine
	// recovery path even against a hand `rm -rf` of the data dir.
	if bpath, berr := backupSecrets(network); berr != nil {
		fmt.Fprintf(os.Stderr, "  warn: secrets backup: %v\n", berr)
	} else if bpath != "" {
		fmt.Printf("  secrets backup: %s\n", bpath)
	}

	// Persist the RPC listen address so `lantern info` reports the port
	// this daemon actually bound, instead of assuming the 1234 default.
	// Best-effort: a write failure here must not stop the daemon.
	if err := writeRPCListen(netDir, *listen); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: persist rpc-listen: %v\n", err)
	}

	// Warn loudly when the default 1234 collides with an existing
	// listener (almost always a local Lotus). Lantern keeps 1234 as the
	// Lotus-compatible default, but a silent collision is what made
	// issue #34 confusing: the daemon failed to bind while `info` still
	// advertised 1234. Surface it with the fix (--listen) inline.
	if *listen == defaultListen && portInUse(*listen) {
		fmt.Printf("  ⚠ %s is already in use (Lotus also defaults to 1234).\n"+
			"    Pass --listen 127.0.0.1:2345 (or any free port) to run alongside it.\n", *listen)
	}

	// Resolve --header-store default now that network is known.
	if *hsPath == "" {
		*hsPath = filepath.Join(netDir, "headerstore")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("Lantern daemon — Lotus-compatible RPC (network: %s)\n", network)
	fmt.Printf("  data dir:    %s\n", netDir)
	fmt.Println("Fetching trusted head from", *gw)
	tr, err := fetchVerifiedTrustedHead(ctx, *gw, network, *insecureAnchor)
	if err != nil {
		return err
	}
	fmt.Printf("  head epoch:  %d\n  state root:  %s\n", tr.Epoch, tr.StateRoot)

	w, err := openWallet(network)
	if err != nil {
		return fmt.Errorf("open wallet: %w", err)
	}

	// Node tier (install-time choice, persisted by the installer). Light =
	// in-memory cache (small footprint). PDP/Full = a persistent, bounded
	// (2-5 GB) block cache so the warm contract set survives restart - the
	// mid/PDP node's footprint, provisioned only for the tier that chose it.
	profile, perr := nodeprofile.Load(dataDir(), network.String())
	if perr != nil {
		return fmt.Errorf("load node profile: %w", perr)
	}

	// gatewayBG + fetcher is the cache+http chain. Bitswap, when enabled,
	// is inserted between cache and HTTP gateway later in this function
	// once the libp2p host is up.
	var cache combined.Cache
	var persistentCache *statecache.Store
	if profile.UsesPersistentCache() {
		bcPath := filepath.Join(netDir, "blockcache")
		if err := os.MkdirAll(bcPath, 0o700); err != nil {
			return fmt.Errorf("create block cache dir: %w", err)
		}
		bc, err := statecache.Open(bcPath, statecache.Options{SoftCapBytes: profile.CacheBytes()})
		if err != nil {
			return fmt.Errorf("open persistent block cache: %w", err)
		}
		persistentCache = bc
		cache = bc
		fmt.Printf("  node tier:    %s  (persistent cache %s, soft cap %d MiB)\n",
			profile.Tier, bcPath, profile.CacheBytes()>>20)
	} else {
		cache = hamt.NewMemBlockStore()
		fmt.Printf("  node tier:    %s  (in-memory cache)\n", profile.Tier)
	}
	fetcherSources := []combined.Source{
		{Name: "gateway", Getter: hsync.NewClient([]string{*gw}, 20*time.Second), Timeout: 5 * time.Second, Race: true},
	}
	if !*noFallbackRPC {
		fetcherSources = append(fetcherSources,
			combined.Source{Name: "glif", Getter: glif.New(glifURLForNetwork(network), 20*time.Second), Timeout: 20 * time.Second})
	} else {
		fmt.Printf("  bridge-off:   no upstream RPC fallback wired (#76); head=gossipsub-only, cold-blocks=gateway+bitswap\n")
	}
	fetcher := combined.New(cache, fetcherSources...)
	if persistentCache != nil {
		defer persistentCache.Close()
	}
	chainAPI := handlers.New(tr, fetcher, w, nil, network.String())

	// Issue #4: wire optional VM bridge for block production. Refuse to
	// start when AllowBlockSubmit is on but no bridge is configured;
	// silently publishing blocks with the parent stateRoot copied
	// verbatim would be rejected by the network and would consume the
	// miner's winning ticket. Failing loud here protects the SP.
	// PDP/backup tier can opt into block submission at install time; the
	// runtime flag still overrides/forces it. Either way it needs a bridge.
	effAllowBlockSubmit := *allowBlockSubmit || profile.AllowBlockSubmit
	if effAllowBlockSubmit && *vmBridgeRPC == "" {
		return fmt.Errorf("block submission (tier=%s or --allow-block-submit) requires --vm-bridge-rpc to be set (see issue #4 in repo)", profile.Tier)
	}
	var vmBr *bridge.ForestBridge
	if *vmBridgeRPC != "" {
		token := *vmBridgeToken
		if token == "" {
			token = os.Getenv("LANTERN_VM_BRIDGE_TOKEN")
		}
		vmBr = bridge.NewForestBridge(*vmBridgeRPC, token, *vmBridgeTimeout)
		chainAPI.WithBridge(vmBr)
		chainAPI.AllowBlockSubmit = effAllowBlockSubmit
		fmt.Printf("  vm-bridge:    %s", vmBr.Provenance())
		if effAllowBlockSubmit {
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

		// Sync source: #53 — a bitswap-backed adapter. HeadEpoch /
		// TipsetCIDsByHeight stay on Glif (RPC-shaped), but the
		// content-addressed FetchBlock (parent backfill) is served from the
		// combined gateway+bitswap fetcher first, with Glif fallback. This
		// takes parent backfill off the Glif critical path, so a slow /
		// rate-limited Glif no longer stalls contiguous head advancement.
		// Network-aware: calibration daemon pulls from calibration Glif,
		// mainnet daemon pulls from mainnet Glif. Without the network split
		// the header store fills with the wrong chain's headers (silent
		// corruption).
		// #76: bridge-off => no Glif Sync source (truly-nil interface so the
		// hstore.Sync nil-guard fires and the polling Sync is a no-op; head is
		// gossipsub-only). Otherwise the bitswap-backed adapter (Glif head +
		// content-addressed backfill).
		src := nilRPCSource()
		if !*noFallbackRPC {
			src = newBitswapBackedSource(glif.New(glifURLForNetwork(network), 8*time.Second), func() blockGetter { return fetcher })
		}
		// #71: when libp2p/gossipsub is enabled it is the PRIMARY head source
		// (0-1 epoch latency, no Glif), so relax the polling-Sync cadence to
		// 30s as a catch-up fallback instead of polling Glif's ChainHead every
		// 6s. This matches the embedded daemon (pkg/daemon/start.go) and stops
		// a healthy node from getting Glif-rate-limited (429). An explicit
		// non-default -sync-interval is always honored. The gossip-aware skip
		// wired below (SetGossipFresh) drops Glif polls to ~zero when gossip is
		// live; this relaxed floor covers the brief windows it doesn't.
		effSyncInterval := *syncInterval
		gossipEnabled := !*noLibp2p && *p2pListen != ""
		if gossipEnabled && effSyncInterval == 6*time.Second {
			effSyncInterval = 30 * time.Second
		}
		sync = hstore.NewSync(store, src, hstore.SyncOptions{
			Interval:       effSyncInterval,
			MaxBacktrack:   60,
			BootstrapDepth: 3, // small cold start; ongoing polls catch up
			// #51 "down for a week" auto-heal: if the persisted store is
			// more than ~a day behind live head, re-anchor near live head
			// instead of trying (and failing) to backfill the gap. Rebuildable
			// chain state only — keys/wallets/tokens are untouched.
			StaleResetThreshold: abi.ChainEpoch(*staleResetThreshold),
			OnStaleReset: func(storeHead, liveHead abi.ChainEpoch) {
				fmt.Printf("  header store: persisted head %d is %d epochs (~%.1f days) behind live head %d — re-anchoring near live head (chain state only; keys untouched)\n",
					storeHead, liveHead-storeHead, float64(liveHead-storeHead)/2880.0, liveHead)
			},
		})
		// Full tier (#90): re-verify each ingested block's signature / VRF /
		// win-count against resident F3-anchored state. Observe-only (logs,
		// does not reject) so a Full node can be brought up + watched on
		// calibration before the pipeline gates ingest. nil on Light/PDP.
		if profile.FullValidation() && chainAPI != nil {
			sv := chainAPI.FullValidateView()
			hsForBeacon := store
			sync.SetBlockValidator(func(ctx context.Context, bh *types.BlockHeader) error {
				prevBeacon, _ := hsForBeacon.LatestBeaconEntry(bh)
				_, err := fullvalidate.ValidateBlockConsensus(ctx, bh, prevBeacon, sv)
				return err
			}, false)
			fmt.Printf("  node tier:    full block validation ON (observe mode, #90)\n")
		}
		if err := sync.Start(ctx); err != nil {
			return fmt.Errorf("start header sync: %w", err)
		}
		defer sync.Stop()
		fmt.Printf("  header store: %s (sync every %s, buf=%d)\n",
			*hsPath, effSyncInterval.String(), *notifyBufSize)
	}

	// Phase 10 Part A: bring up the live libp2p host so the Net* RPC
	// methods Curio's webui consumes return real data. The host dials the
	// Filecoin mainnet bootstrap peers in the background; bandwidth +
	// AutoNAT state are captured by the BandwidthCounter and the
	// AmbientAutoNAT subsystem on the host respectively.
	var p2pHost *llibp2p.Host
	var gossipIngestor *gossipBlockIngestor
	var helloSvc *hello.Service
	var xchgSvc *chainxchg.Service
	_ = helloSvc // kept around for future stats wiring; lifecycle is goroutine-managed
	_ = xchgSvc
	if !*noLibp2p && *p2pListen != "" {
		listeners := splitCSV(*p2pListen)
		// Peer count sizing for issue #16/#18 follow-up:
		// Lantern is a light client. Its load-bearing fetch paths are
		//   (a) HTTP gateway for chain head + state reads
		//   (b) Bitswap fast-stage against the preferred beacon peer
		//   (c) Bitswap full-stage against a handful of swarm peers
		// The gossipsub block topic needs the default mesh size (D_lo=4,
		// D=6), so ~6 peers in the mesh is the functional minimum for
		// real-time head propagation. Add a safety margin for churn:
		//
		//   MinPeers = 20
		//   MaxPeers = 200 (room for ad-hoc Bitswap providers)
		//
		// We previously ran MinPeers=50 inherited from Lotus. Real Lotus
		// nodes need 50+ because they serve chain to others, participate
		// in F3 voting, and make deals. Lantern does none of those. The
		// honest floor for a light client is ~20.
		// #80 part 2: resolve the corroboration requirement. Auto: the
		// Full tier follows head with full trust weight on it, so it
		// requires 2 distinct sources; Light/PDP stay off (their head
		// safety comes from the divergence monitor + fork choice).
		effCorroPeers := *headCorroPeers
		if effCorroPeers < 0 {
			if profile.FullValidation() {
				effCorroPeers = 2
			} else {
				effCorroPeers = 0
			}
		}
		var corroTracker *blockpub.CorroborationTracker
		if effCorroPeers > 0 {
			corroTracker = blockpub.NewCorroborationTracker(network.GossipTopicBlocks())
		}
		p2pHost, err = llibp2p.New(ctx, llibp2p.HostConfig{
			ListenAddrs:    listeners,
			BootstrapPeers: network.BootstrapPeers(),
			MinPeers:       20,
			MaxPeers:       200,
			PubSubTracer:   corroTracker.Tracer(),
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
			BootstrapPeers: network.BootstrapPeers(),
			NetworkName:    network.NetworkName(),
		}); err != nil {
			fmt.Printf("  libp2p: EnableDHT failed: %v (continuing without DHT discovery)\n", err)
		} else {
			fmt.Printf("  libp2p: DHT discovery on (target peers >= %d, hwm %d)\n", p2pHost.MinPeers(), p2pHost.MaxPeers())
		}

		// Issue #16: speak /fil/hello/1.0.0 so remote Filecoin peers'
		// connmgr scores us positively and stops trimming us within 30s.
		// Also tags inbound peers as "fcpeer" in our own connmgr so we
		// don't trim them.
		if genCID, perr := cid.Parse(network.GenesisCID()); perr == nil {
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
			fmt.Printf("  hello:    /fil/hello/1.0.0 active (genesis %s…)\n", network.GenesisCID()[:18])
		} else {
			fmt.Printf("  hello:    DISABLED (genesis CID parse: %v)\n", perr)
		}

		// Issue #17: ChainExchange responder. Minimum-viable: answers
		// 'NotFound' to every request. Being REACHABLE on the protocol
		// is what removes the 'dead node' signal that triggers remote
		// connmgr trim passes.
		xchgSvc = chainxchg.NewService(p2pHost.H)
		xchgSvc.Register()
		fmt.Printf("  chainxchg: /fil/chain/xchg/0.0.1 active (NotFound responder)\n")

		// Issue #1: subscribe to /fil/blocks/<network> on gossipsub so
		// new heads land in the header store within ~1-3s of network
		// propagation, instead of waiting for the next 6-30s poll cycle.
		// The polling Sync above stays as the catch-up fallback for
		// blocks that gossipsub missed (connectivity blips, late join,
		// etc.) and for the first install on a cold start.
		//
		// We pass the same bitswap-backed adapter the polling Sync uses so
		// the ingestor's inline backfill (when a gossipsub arrival lands at
		// head+N>1) serves parent blocks from the content-addressed fetcher
		// first and only falls back to Glif (#53). Previously this was a raw
		// Glif client, so a slow Glif drove backfillFail up and desynced the
		// node despite live blocks arriving fine over gossipsub.
		if store != nil && p2pHost.PubSub != nil {
			// #76: bridge-off => no Glif inline-backfill source (truly-nil
			// interface so the ingestor nil-guard fires; parent backfill is
			// served from the content-addressed fetcher / bitswap only).
			gossipSrc := nilBackfillSource()
			if !*noFallbackRPC {
				gossipSrc = newBitswapBackedSource(glif.New(glifURLForNetwork(network), 8*time.Second), func() blockGetter { return fetcher })
			}
			if ing, blockPub, gerr := startGossipBlocks(ctx, p2pHost.PubSub, store, gossipSrc, network.GossipTopicBlocks()); gerr != nil {
				fmt.Printf("  gossipsub-blocks: failed to start: %v (continuing without)\n", gerr)
			} else {
				gossipIngestor = ing
				// #80 part 2: head adoption requires corroboration from
				// distinct scored peers (trusted floor peers super-vote;
				// requirement clamps to connected-peer count so a small
				// node never wedges).
				if corroTracker != nil {
					hostRef := p2pHost
					ing.SetHeadCorroboration(blockpub.CorroborationGate(
						corroTracker, effCorroPeers,
						hostRef.IsTrustedPeer,
						func() int { return hostRef.PeerCount() }))
					fmt.Printf("  head-corroboration: on (min distinct sources: %d, trusted floor super-vote)\n", effCorroPeers)
				}
				// Wire the /fil/blocks publisher so SyncSubmitBlock can
				// actually publish (PDP/backup tier). SyncSubmitBlock still
				// gates on AllowBlockSubmit, so this is safe on any tier.
				if blockPub != nil {
					chainAPI.SetBlockPublisher(blockPub)
				}
				fmt.Printf("  gossipsub-blocks: subscribed to %s (ingestor active, inline backfill on)\n", network.GossipTopicBlocks())
				// #71: let the polling Sync skip its Glif HeadEpoch() poll while
				// gossip is keeping the store head fresh, so a healthy node stops
				// hammering (and getting 429'd by) Glif. Window = 60s (2x the
				// relaxed 30s cadence); when gossip goes quiet the Sync resumes.
				if sync != nil {
					sync.SetGossipFresh(func() bool { return ing.Fresh(60 * time.Second) })
					// #83: lag-aware skip - resume catch-up when gossip is
					// fresh-but-lagging instead of wedging behind the tip.
					sync.SetGossipObservedHead(func() abi.ChainEpoch { return ing.ObservedHead() }, 0)
				}
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
			// lantern#50: Filecoin nodes serve bitswap under the "/chain"
			// protocol prefix (/chain/ipfs/bitswap/...), NOT the boxo/IPFS
			// default (/ipfs/bitswap/...). Without this the standalone daemon
			// connects to mainnet peers but every stream negotiation fails
			// ("protocols not supported: /ipfs/bitswap/..."), so bitswap serves
			// zero blocks and the gateway carries the entire cold-block tail.
			// The embedded daemon (pkg/daemon/start.go) already set this; the
			// standalone CLI path was missing it.
			ProtocolPrefix: network.BitswapProtocolPrefix(),
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
		rebuiltSources := []combined.Source{
			{Name: "bitswap", Getter: bsClient, Timeout: *bitswapFullDL, Race: true},
			{Name: "gateway", Getter: hsync.NewClient([]string{*gw}, 20*time.Second), Timeout: 5 * time.Second, Race: true},
		}
		if !*noFallbackRPC {
			rebuiltSources = append(rebuiltSources,
				combined.Source{Name: "glif", Getter: glif.New(glifURLForNetwork(network), 20*time.Second), Timeout: 20 * time.Second})
		}
		fetcher = combined.New(cache, rebuiltSources...)
		rebindBlockGetter(chainAPI, fetcher)
		fmt.Printf("  bitswap:  enabled (preferred=%d, fast=%s, full=%s)\n",
			len(preferred), bitswapFastDL.String(), bitswapFullDL.String())
	}

	// Phase 10 Part B: /metrics endpoint exposes per-source hit counts so
	// operators can see Bitswap carrying load. Issue #5 added the dashboard
	// on the same listener. v1.5.5 enables both by default on 127.0.0.1:9092
	// so a fresh `lantern daemon` always has a webui without extra flags.
	// #98: VM-bridge cross-check auditor. Observe-only; needs both the
	// bridge and the header store. Divergences alarm via log + counters
	// (dashboard card below); reads are never answered by the bridge.
	var xchecker *crosscheck.Checker
	if *vmCrossCheck {
		switch {
		case vmBr == nil:
			return fmt.Errorf("--vm-crosscheck requires --vm-bridge-rpc")
		case store == nil:
			return fmt.Errorf("--vm-crosscheck requires the header store (remove --header-store \"\" overrides)")
		default:
			xc, xerr := crosscheck.New(crosscheck.Config{
				Bridge:   vmBr,
				Source:   store,
				Interval: *vmCrossCheckInterval,
			})
			if xerr != nil {
				return xerr
			}
			xc.Start(ctx)
			xchecker = xc
			fmt.Printf("  vm-crosscheck: on (auditing local chain vs %s every %s, observe-only)\n",
				vmBr.Provenance(), vmCrossCheckInterval.String())
		}
	}

	var dashboardURL string
	if *metricsListen != "" {
		// SECURITY #57: the dashboard/metrics listener exposes node internals
		// + action POST endpoints (guarded only by a same-origin header).
		// Refuse a non-loopback bind unless explicitly allowed, and when
		// allowed require a Bearer token so the surface isn't world-open.
		dashToken := strings.TrimSpace(os.Getenv("LANTERN_DASHBOARD_TOKEN"))
		if !isLoopbackListen(*metricsListen) {
			if !*allowRemoteDash {
				return fmt.Errorf("refusing to bind dashboard/metrics to non-loopback %q without --allow-remote-dashboard "+
					"(#57: exposes node internals + action buttons)", *metricsListen)
			}
			if dashToken == "" {
				return fmt.Errorf("dashboard bound to non-loopback %q requires LANTERN_DASHBOARD_TOKEN to be set "+
					"(#57: a Bearer token gates the exposed surface)", *metricsListen)
			}
			fmt.Fprintf(os.Stderr, "  [dashboard] WARNING: bound to non-loopback %q; Bearer-token auth enforced.\n", *metricsListen)
		}
		var dash *dashboardDeps
		if !*noDashboard {
			bridgeTag := ""
			if chainAPI.Bridge != nil {
				bridgeTag = chainAPI.Bridge.Provenance()
			}
			dash = &dashboardDeps{
				tr:           tr,
				store:        store,
				sync:         sync,
				host:         p2pHost,
				bsClient:     bsClient,
				fetcher:      fetcher,
				ingestor:     gossipIngestor,
				vmBridgeTag:  bridgeTag,
				allowSubmit:  chainAPI.AllowBlockSubmit,
				network:      "Lantern+" + network.String(),
				rpcAddr:      *listen,
				startedAt:    time.Now(),
				headDelaySec: uint64(build.BlockDelaySecs),
				dataDirPath:  netDir,
				gatewayURL:   *gw,
				hello:        helloSvc,
				xchg:         xchgSvc,
				// #96: observed-data EC finality (FRC-0089). Nil-safe in
				// the handler; store==nil (no header store) leaves it off.
				ecfin: newECFinality(store),
				// #98: VM-bridge cross-check auditor stats (nil when off).
				xcheck: xchecker,
			}
			dashboardURL = fmt.Sprintf("http://%s/dashboard/", *metricsListen)
		}
		go serveMetrics(ctx, *metricsListen, dashToken, fetcher, bsClient, p2pHost, dash)
		fmt.Printf("  metrics:  http://%s/metrics\n", *metricsListen)
		if dashboardURL != "" {
			fmt.Printf("  dashboard: %s\n", dashboardURL)
		}
	}

	// SECURITY #56: refuse to bind the key-holding, signing-capable RPC to a
	// non-loopback address unless the operator explicitly opted in. The
	// listener defends wallet keys + the eth_sendRawTransaction write path;
	// loopback is the only safe default without a fronting auth proxy.
	if !isLoopbackListen(*listen) && !*allowRemoteRPC {
		return fmt.Errorf("refusing to bind RPC to non-loopback address %q without --allow-remote-rpc "+
			"(#56: the RPC holds wallet keys + a signing write path; expose it only behind an auth proxy and set --allow-remote-rpc to acknowledge)", *listen)
	}
	if !isLoopbackListen(*listen) && *allowRemoteRPC {
		fmt.Fprintf(os.Stderr, "  [rpc] WARNING: RPC bound to non-loopback %q (--allow-remote-rpc). "+
			"Privileged methods require a Bearer token; ensure the port is firewalled / proxied.\n", *listen)
	}

	srv, err := server.New(server.Config{
		ListenAddress: *listen,
		// Stage 2 (#51): jwt-secret + scope tokens live under secrets/.
		DataDir: secretsDir(network),
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
	// Final summary banner: surface the two URLs the operator will actually
	// hit (RPC + dashboard) so they don't get lost in the daemon log.
	fmt.Println()
	if dashboardURL != "" {
		fmt.Println("\033[1mDashboard\033[0m  " + dashboardURL)
	}
	fmt.Println("\033[1mReady.\033[0m  Ctrl-C to stop.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down...")

	// Cancel the root daemon context so every long-running subsystem
	// (sync poller, libp2p connect loops, bitswap, gossipsub, beacon,
	// chainxchg, header store sync, log goroutines) starts winding
	// down promptly. Without this, only the HTTP server shuts down
	// and SIGTERM responsiveness degrades to '5s timeout then exit
	// while goroutines are still alive,' which makes back-to-back
	// daemon restarts fail to acquire the Badger headerstore lock.
	// See lantern#31.
	cancel()

	// Bound the HTTP server's graceful shutdown so a stuck connection
	// doesn't hang the process forever. 5s is the canonical Go timeout.
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := srv.Stop(sctx); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: HTTP shutdown: %v\n", err)
	}

	// Tiny grace period for in-flight handlers to observe ctx.Done
	// before main returns and the defers (Badger.Close, libp2p.Close)
	// fire. Without this, the Badger DB's WAL flush sometimes races
	// with the subsequent daemon start.
	time.Sleep(500 * time.Millisecond)
	return nil
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
		return fmt.Errorf("wallet: subcommand required (new|list|balance|send|export|import|import-lotus)")
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
	case "export":
		// #93: Lotus-hex KeyInfo to stdout.
		return walletExport(rest)
	case "import":
		// #93: Lotus-hex KeyInfo from arg or stdin.
		return walletImport(rest)
	case "import-lotus":
		// #93: bulk import wallet keys from a Lotus repo keystore.
		return walletImportLotus(rest)
	}
	return fmt.Errorf("wallet: unknown subcommand %q", sub)
}

func walletNew(args []string) error {
	fs := flag.NewFlagSet("wallet new", flag.ExitOnError)
	kt := fs.String("type", "bls", "Key type: bls, secp, delegated")
	fs.Parse(args)
	w, err := openWalletDefault()
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
	w, err := openWalletDefault()
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
	w, err := openWalletDefault()
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
	tr, err := fetchTrustedHead(ctx, defaultGateway, build.Mainnet)
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
	w, err := openWalletDefault()
	if err != nil {
		return err
	}
	from, err := w.Default(context.Background())
	if err != nil {
		return err
	}
	tr, err := fetchTrustedHead(context.Background(), defaultGateway, build.Mainnet)
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
		tr, err := fetchTrustedHead(ctx, defaultGateway, build.Mainnet)
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
		tr, err := fetchTrustedHead(ctx, defaultGateway, build.Mainnet)
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

// validateGatewayScheme enforces the #55 transport-integrity rule: the
// gateway URL must be https:// unless the operator explicitly opts into an
// insecure plain-http endpoint (localhost/dev). http://localhost and
// http://127.0.0.1 are allowed without the flag since loopback has no
// meaningful MITM surface.
func validateGatewayScheme(gw string, insecure bool) error {
	u, err := url.Parse(gw)
	if err != nil {
		return fmt.Errorf("invalid --gateway %q: %w", gw, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil // loopback: no MITM surface
		}
		if insecure {
			fmt.Fprintf(os.Stderr, "  [gateway] WARNING: using plain-http gateway %q (--insecure-gateway). Boot anchor + cold fetches are MITM-able.\n", gw)
			return nil
		}
		return fmt.Errorf("refusing plain-http gateway %q (#55: no transport integrity; "+
			"a MITM could seed a bad boot anchor). Use https://, or set --insecure-gateway for a trusted localhost/dev endpoint", gw)
	default:
		return fmt.Errorf("unsupported --gateway scheme %q in %q (want https:// or http://)", u.Scheme, gw)
	}
}

// isLoopbackListen reports whether a "host:port" (or ":port") RPC listen
// address binds only to the loopback interface. An empty/":port"/"0.0.0.0"
// host is treated as non-loopback (binds all interfaces). Used by the #56
// non-loopback bind guard. Hostnames that aren't literal IPs are treated as
// non-loopback (fail-safe) except the literal "localhost".
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port? Treat the whole string as host.
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// Non-literal hostname: fail safe.
	return false
}

// --- info ---

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	networkFlag := fs.String("network", string(build.DefaultNetwork), "Filecoin network: mainnet | calibration")
	listenFlag := fs.String("listen", "", "Override the RPC address to report/probe (default: the address the daemon persisted, else 127.0.0.1:1234)")
	tokenOnly := fs.Bool("token-only", false, "Print only the admin token (for scripting FULLNODE_API_INFO)")
	fs.Parse(args)

	network := build.Network(*networkFlag)
	if !network.Valid() {
		return fmt.Errorf("invalid --network %q: want one of mainnet, calibration", *networkFlag)
	}
	// Stage 2 (#51): make sure secrets are relocated so we read the token
	// from the right place. Idempotent + best-effort (info must not fail
	// just because migration couldn't run).
	_ = migrateLegacyDataDir(network)
	_ = migrateSecretsLayout(network)
	netDir := networkDataDir(network)

	// Resolve the admin token. Stage 2 location (secrets/) first, then the
	// V1.3 per-network dir, then the pre-V1.3 top-level location.
	token, tokenErr := readAdminToken(network)

	// --token-only: emit just the raw token (or fail) for scripting.
	if *tokenOnly {
		if tokenErr != nil {
			return fmt.Errorf("no admin token under %s (run `lantern init --filecoin-network %s`)", netDir, network)
		}
		fmt.Println(token)
		return nil
	}

	// Resolve the RPC address: explicit --listen wins, else the address
	// the daemon persisted, else the documented default.
	listen := *listenFlag
	if listen == "" {
		listen = readRPCListen(netDir)
	}

	fmt.Println("Lantern info")
	fmt.Println("============")
	fmt.Printf("Data dir: %s (network: %s)\n", netDir, network)

	if tokenErr != nil {
		fmt.Printf("Admin token: (not initialised — run `lantern init --filecoin-network %s`)\n", network)
	} else {
		short := token
		if len(short) > 20 {
			short = short[:10] + "..." + short[len(short)-6:]
		}
		fmt.Printf("Admin token: %s\n", short)
		fmt.Printf("FULLNODE_API_INFO (daemon on %s):\n  %s:%s\n",
			listen, token, apiMultiaddr(listen))
	}

	// Probe the resolved daemon address.
	hc := &http.Client{Timeout: 1 * time.Second}
	resp, err := hc.Get("http://" + listen + "/healthz")
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Printf("Daemon healthz: %s\n", strings.TrimSpace(string(body)))
		} else {
			fmt.Printf("Daemon healthz: HTTP %d at %s (is something other than Lantern on this port?)\n",
				resp.StatusCode, listen)
		}
	} else {
		fmt.Printf("Daemon: not running (no listener on %s)\n", listen)
	}
	return nil
}

// readAdminToken reads the admin token from the per-network data dir,
// falling back to the pre-V1.3 top-level location for un-migrated
// installs. Returns the trimmed token or an error if neither exists.
func readAdminToken(network build.Network) (string, error) {
	// Stage 2 layout: <net>/secrets/token.
	if b, err := os.ReadFile(secretPath(network, "token")); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	// V1.3 per-network location: <net>/token.
	if b, err := os.ReadFile(filepath.Join(networkDataDir(network), "token")); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	// Pre-V1.3 single-network installs kept the token at the top-level
	// data dir.
	if b, err := os.ReadFile(filepath.Join(dataDir(), "token")); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("no token file under %s", secretsDir(network))
}

// rpcListenFile is where the daemon records the RPC address it bound, so
// `lantern info` can report the real port instead of assuming 1234.
const rpcListenFile = "rpc-listen"

// writeRPCListen persists the daemon's RPC listen address under netDir.
func writeRPCListen(netDir, addr string) error {
	return os.WriteFile(filepath.Join(netDir, rpcListenFile), []byte(addr+"\n"), 0o600)
}

// readRPCListen returns the persisted RPC listen address for netDir, or
// the documented default when none was recorded.
func readRPCListen(netDir string) string {
	if b, err := os.ReadFile(filepath.Join(netDir, rpcListenFile)); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return defaultListen
}

// apiMultiaddr renders a host:port listen address as the /ip4/.../tcp/.../http
// multiaddr that FULLNODE_API_INFO expects. Falls back to a 127.0.0.1
// multiaddr on the resolved port when the host isn't a plain IPv4.
func apiMultiaddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "/ip4/127.0.0.1/tcp/1234/http"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		return fmt.Sprintf("/ip4/%s/tcp/%s/http", host, port)
	}
	// Non-IPv4 host (hostname or IPv6): report loopback IPv4 on the port,
	// which is what a local Curio/Lotus client expects.
	return fmt.Sprintf("/ip4/127.0.0.1/tcp/%s/http", port)
}

// portInUse reports whether addr already has a listener (best-effort).
// Used only to warn about the Lotus/1234 collision; never fatal.
func portInUse(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
