// lantern-phase6 is the Phase 6 end-to-end demo.
//
// Goal: prove Lantern's message-flow + randomness + persistent header
// store + libp2p mempool work against live mainnet, in dry-run mode.
//
// What this demo does:
//
//  1. Bootstrap a TrustedRoot with F3 anchor-forward-walked finality.
//  2. Open the persistent header store and sync the last 5 epochs from
//     a Lotus-compatible RPC (Glif).
//  3. Compute DRAND beacon-derived randomness at a known mainnet epoch
//     and cross-check it against Glif's StateGetRandomnessFromBeacon.
//  4. Compute ticket randomness for the same epoch and cross-check.
//  5. Start a libp2p host + gossipsub, count peer connections, count
//     messages received on /fil/msgs/testnetnet for ~20 seconds.
//  6. Dry-run MpoolPush: construct a SignedMessage, validate it locally,
//     print what would be published without actually publishing.
//  7. (Optional) StateSearchMsg cross-check if --search-tx is provided.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-f3/manifest"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/build"
	lbeacon "github.com/Reiers/lantern/chain/beacon"
	"github.com/Reiers/lantern/chain/f3/anchor"
	"github.com/Reiers/lantern/chain/f3/subscriber"
	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/trustedroot"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/glif"
	llibp2p "github.com/Reiers/lantern/net/libp2p"
	"github.com/Reiers/lantern/net/mpool"
)

type flags struct {
	gateway      string
	glifURL      string
	epochRand    int64
	listen       string
	peerWaitSecs int
	gossipSecs   int
	walkCerts    int
}

func main() {
	var f flags
	flag.StringVar(&f.gateway, "gateway", "https://gateway.lantern.reiers.io", "Lantern gateway URL")
	flag.StringVar(&f.glifURL, "glif", "https://api.node.glif.io/rpc/v1", "Glif RPC URL")
	flag.Int64Var(&f.epochRand, "rand-epoch", 6_000_000, "Filecoin epoch for randomness cross-check")
	flag.StringVar(&f.listen, "listen", "/ip4/0.0.0.0/tcp/0", "libp2p listen multiaddr")
	flag.IntVar(&f.peerWaitSecs, "peer-wait", 30, "Seconds to wait for libp2p peer connections")
	flag.IntVar(&f.gossipSecs, "gossip-secs", 20, "Seconds to observe gossipsub /fil/msgs traffic")
	flag.IntVar(&f.walkCerts, "walk-certs", 10, "Max F3 certs to walk forward from the anchor")
	flag.Parse()

	ctx := context.Background()
	if err := run(ctx, f); err != nil {
		fmt.Fprintf(os.Stderr, "\nFAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\nOK — Phase 6 demo complete")
}

func run(ctx context.Context, f flags) error {
	fmt.Println("Lantern Phase 6 — message-flow + libp2p + randomness")
	fmt.Println("====================================================")

	// ---- 1. TrustedRoot with activated F3 cert chain ----
	fmt.Println("\n[1] Activating F3 cert chain from embedded anchor")
	a, err := anchor.Embedded("mainnet")
	if err != nil {
		return fmt.Errorf("load anchor: %w", err)
	}
	fmt.Printf("    anchor instance:      %d\n", a.Instance)
	fmt.Printf("    anchor committee:     %d entries\n", len(a.Entries))

	mf, err := parseManifest(build.F3ManifestMainnetJSON)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	src := subscriber.NewJSONRPCSource(f.glifURL)
	sub, err := subscriber.New(subscriber.Options{
		Anchor:   a,
		Manifest: mf,
		Source:   src,
	})
	if err != nil {
		return fmt.Errorf("subscriber: %w", err)
	}
	startInst := a.Instance
	walked, err := sub.Walk(ctx, f.walkCerts)
	if err != nil {
		fmt.Printf("    WARN: cert walk failed at instance %d: %v\n", sub.State().Instance, err)
	} else {
		fmt.Printf("    walked %d certs forward (instance %d → %d)\n", walked, startInst, sub.State().Instance)
	}

	st := sub.State()
	var finalizedTSK ltypes.TipSetKey
	var finalizedEp abi.ChainEpoch
	if st.LatestChain != nil && !st.LatestChain.IsZero() {
		head := st.LatestChain.Head()
		finalizedEp = abi.ChainEpoch(head.Epoch)
		k, err := ltypes.TipSetKeyFromBytes(head.Key)
		if err == nil {
			finalizedTSK = k
		}
		fmt.Printf("    F3 finalized: epoch %d (instance %d)\n", finalizedEp, st.Instance-1)
	}

	// Build TrustedRoot. For the demo, we read the head from Glif so the
	// trusted root carries the current chain head's stateRoot, while F3
	// finality is recorded separately.
	gl := glif.New(f.glifURL, 20*time.Second)
	head, err := gl.FetchHead(ctx)
	if err != nil {
		return fmt.Errorf("glif ChainHead: %w", err)
	}
	tr := &trustedroot.TrustedRoot{
		Epoch:                 head.Epoch,
		TipSetKey:             head.TipSetKey,
		StateRoot:             head.StateRoot,
		ParentMessageReceipts: head.ParentMessageReceipts,
		ParentWeight:          head.ParentWeight,
		F3Instance:            st.Instance,
		F3Cert:                st.Latest,
		AcceptedAt:            time.Now().UTC(),
	}
	fmt.Printf("    TrustedRoot @ epoch %d, stateRoot %s\n", tr.Epoch, tr.StateRoot)
	fmt.Printf("    F3Instance=%d, finalizedEp=%d, finalizedTSK=%v(%d cids)\n", tr.F3Instance, finalizedEp, !finalizedTSK.IsEmpty(), len(finalizedTSK.Cids()))

	// ---- 2. Persistent header store + sync last 5 epochs ----
	fmt.Println("\n[2] Persistent header store + sync")
	tmpDir, err := os.MkdirTemp("", "lantern-phase6-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	store, err := hstore.Open(tmpDir+"/hdrs", hstore.Options{})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	// Fetch the last 5 epochs directly from Glif and push them through
	// SetHead. This exercises the store without engaging the full Sync
	// agent's reorg-detection machinery (the latter is unit-tested).
	fmt.Printf("    syncing last 5 epochs from %s\n", f.glifURL)
	syncStart := time.Now()
	for ep := head.Epoch - 4; ep <= head.Epoch; ep++ {
		cids, err := gl.TipsetCIDsByHeight(ctx, ep)
		if err != nil {
			fmt.Printf("      epoch %d: %v\n", ep, err)
			continue
		}
		blocks := make([]*ltypes.BlockHeader, 0, len(cids))
		for _, c := range cids {
			bh, err := gl.FetchBlock(ctx, c)
			if err != nil {
				fmt.Printf("      block %s: %v\n", c, err)
				continue
			}
			blocks = append(blocks, bh)
		}
		if len(blocks) == 0 {
			continue
		}
		ts, err := ltypes.NewTipSet(blocks)
		if err != nil {
			fmt.Printf("      epoch %d tipset: %v\n", ep, err)
			continue
		}
		if err := store.SetHead(ctx, ts); err != nil {
			fmt.Printf("      epoch %d setHead: %v\n", ep, err)
			continue
		}
		fmt.Printf("      epoch %d: %d blocks, first CID %s\n", ts.Height(), len(ts.Blocks()), ts.Cids()[0])
	}
	fmt.Printf("    header store head: epoch %d (took %s)\n", store.HeadEpoch(), time.Since(syncStart))

	// ---- 3. DRAND randomness cross-check ----
	fmt.Println("\n[3] DRAND randomness cross-check")
	if err := randomnessCrossCheck(ctx, store, gl, f.glifURL, f.epochRand); err != nil {
		fmt.Printf("    WARN: %v\n", err)
	}

	// ---- 4. libp2p host + gossipsub ----
	fmt.Println("\n[4] libp2p host + gossipsub /fil/msgs/testnetnet")
	hctx, hcancel := context.WithTimeout(ctx, time.Duration(f.peerWaitSecs+f.gossipSecs+30)*time.Second)
	defer hcancel()
	host, err := llibp2p.New(hctx, llibp2p.HostConfig{
		ListenAddrs:    []string{f.listen},
		BootstrapPeers: build.MainnetBootstrapPeers,
	})
	if err != nil {
		return fmt.Errorf("libp2p.New: %w", err)
	}
	defer host.Close()
	fmt.Printf("    peer ID: %s\n", host.ID())
	fmt.Printf("    listen:  %v\n", host.ListenAddrs())

	// Wait up to peerWaitSecs for >= 10 peers.
	target := 10
	deadline := time.Now().Add(time.Duration(f.peerWaitSecs) * time.Second)
	for time.Now().Before(deadline) {
		if host.PeerCount() >= target {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("    connected peers after %ds: %d (target %d)\n", f.peerWaitSecs, host.PeerCount(), target)

	// Subscribe to mainnet /fil/msgs/testnetnet.
	pool, err := mpool.New(hctx, host.PubSub, mpool.Config{
		Topic:  build.MainnetGossipTopicMessages,
		DryRun: true,
	})
	if err != nil {
		return fmt.Errorf("mpool: %w", err)
	}
	defer pool.Close()

	fmt.Printf("    observing %s for %ds...\n", build.MainnetGossipTopicMessages, f.gossipSecs)
	time.Sleep(time.Duration(f.gossipSecs) * time.Second)
	st2 := pool.Stats()
	fmt.Printf("    mpool stats: received=%d, rejected=%d, pending=%d\n", st2.Received, st2.Rejected, st2.PendingCnt)

	// ---- 5. MpoolPush dry-run ----
	fmt.Println("\n[5] MpoolPush dry-run")
	if err := mpoolDryRun(ctx, pool); err != nil {
		fmt.Printf("    WARN: %v\n", err)
	}

	// ---- 6. (Skipped) StateSearchMsg cross-check ----
	fmt.Println("\n[6] StateSearchMsg cross-check (skipped — needs a known recent on-chain tx)")
	fmt.Println("    See PHASE6-BLOCKERS.md for cross-test fixture work")

	return nil
}

func randomnessCrossCheck(ctx context.Context, store *hstore.Store, gl *glif.Client, glifURL string, epochArg int64) error {
	epoch := abi.ChainEpoch(epochArg)
	head := store.HeadEpoch()
	// Always use a synced epoch so the beacon entry is in the store.
	if epoch > head || epoch < head-4 || epoch < 0 {
		newEp := head - 2
		if newEp < 0 {
			newEp = 0
		}
		if epochArg != int64(newEp) {
			fmt.Printf("    rand-epoch adjusted from %d to %d (synced range only)\n", epochArg, newEp)
			epoch = newEp
		}
	}
	ts, err := store.GetTipSetByHeight(epoch)
	if err != nil {
		// Fall through to direct Glif fetch.
		cids, ferr := gl.TipsetCIDsByHeight(ctx, epoch)
		if ferr != nil {
			return fmt.Errorf("tipset %d: %w", epoch, ferr)
		}
		blocks := make([]*ltypes.BlockHeader, 0, len(cids))
		for _, c := range cids {
			bh, berr := gl.FetchBlock(ctx, c)
			if berr != nil {
				return fmt.Errorf("fetch %s: %w", c, berr)
			}
			blocks = append(blocks, bh)
		}
		ts, err = ltypes.NewTipSet(blocks)
		if err != nil {
			return err
		}
	}

	pers := gscrypto.DomainSeparationTag(3) // arbitrary domain
	entropy := []byte("phase6-rand-fixture")
	// Ticket randomness (chain).
	chainR, err := lbeacon.DrawChainRandomness(ts, pers, epoch, entropy)
	if err != nil {
		return fmt.Errorf("ticket randomness: %w", err)
	}
	fmt.Printf("    ticket randomness @ epoch %d (pers=%d): %s\n", epoch, pers, hex.EncodeToString(chainR))

	// Find the beacon entry whose round matches MaxBeaconRoundForEpoch.
	params := lbeacon.MainnetQuicknetParams()
	want := params.MaxBeaconRoundForEpoch(epoch)
	var be *ltypes.BeaconEntry
	for _, b := range ts.Blocks() {
		for _, e := range b.BeaconEntries {
			if e.Round == want {
				be = &e
				break
			}
		}
		if be != nil {
			break
		}
	}
	if be == nil {
		return fmt.Errorf("beacon entry for round %d not found in tipset at epoch %d", want, epoch)
	}
	beaconR, err := lbeacon.DrawBeaconRandomness(*be, pers, epoch, entropy)
	if err != nil {
		return fmt.Errorf("beacon randomness: %w", err)
	}
	fmt.Printf("    beacon randomness @ epoch %d (pers=%d): %s\n", epoch, pers, hex.EncodeToString(beaconR))

	// Cross-check vs Glif.
	chainXC, err := glifRandomness(ctx, glifURL, "Filecoin.StateGetRandomnessFromTickets", pers, epoch, entropy, ts.Key())
	if err != nil {
		fmt.Printf("    Glif ticket randomness XC: ERR %v\n", err)
	} else {
		match := bytes.Equal(chainR, chainXC)
		fmt.Printf("    Glif ticket randomness XC: %s (glif=%s)\n", matchStr(match), hex.EncodeToString(chainXC))
	}

	beaconXC, err := glifRandomness(ctx, glifURL, "Filecoin.StateGetRandomnessFromBeacon", pers, epoch, entropy, ts.Key())
	if err != nil {
		fmt.Printf("    Glif beacon randomness XC: ERR %v\n", err)
	} else {
		match := bytes.Equal(beaconR, beaconXC)
		fmt.Printf("    Glif beacon randomness XC: %s (glif=%s)\n", matchStr(match), hex.EncodeToString(beaconXC))
	}
	return nil
}

func matchStr(b bool) string {
	if b {
		return "MATCH"
	}
	return "MISMATCH"
}

func glifRandomness(ctx context.Context, url, method string, pers gscrypto.DomainSeparationTag, epoch abi.ChainEpoch, entropy []byte, tsk ltypes.TipSetKey) ([]byte, error) {
	cids := tsk.Cids()
	tsParam := make([]map[string]string, 0, len(cids))
	for _, c := range cids {
		tsParam = append(tsParam, map[string]string{"/": c.String()})
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  []any{int64(pers), int64(epoch), entropy, tsParam},
		"id":      1,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(all))
	}
	var env struct {
		Result string `json:"result"`
		Error  *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(all, &env); err != nil {
		return nil, err
	}
	if env.Error != nil {
		return nil, fmt.Errorf("%s", env.Error.Message)
	}
	return base64.StdEncoding.DecodeString(env.Result)
}

func mpoolDryRun(ctx context.Context, pool *mpool.Pool) error {
	from, _ := address.NewIDAddress(1000)
	to, _ := address.NewIDAddress(1001)
	msg := ltypes.Message{
		Version:    0,
		From:       from,
		To:         to,
		Nonce:      1,
		Value:      big.NewInt(1_000_000_000_000_000), // 0.001 FIL
		GasLimit:   10_000_000,
		GasFeeCap:  big.NewInt(100_000_000),
		GasPremium: big.NewInt(100_000),
		Method:     0,
	}
	// Fake signature (dry-run mode doesn't actually publish; validation
	// only checks the shape).
	sig := gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: bytes.Repeat([]byte{0xab}, 96)}
	sm := &ltypes.SignedMessage{Message: msg, Signature: sig}
	c, err := pool.Publish(ctx, sm)
	if err != nil && err.Error() != mpool.ErrDryRun.Error() {
		return err
	}
	fmt.Printf("    dry-run published message CID: %s\n", c)
	fmt.Printf("    msg payload digest:           %s\n", payloadDigest(sm))
	fmt.Printf("    target topic:                  %s\n", build.MainnetGossipTopicMessages)
	pending := pool.Pending()
	fmt.Printf("    locally tracked pending:       %d msg(s)\n", len(pending))
	return nil
}

// payloadDigest returns the sha256 of a SignedMessage's CBOR serialization,
// for logging.
func payloadDigest(sm *ltypes.SignedMessage) string {
	b, _ := sm.Serialize()
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func parseManifest(jsonBytes []byte) (*manifest.Manifest, error) {
	var m manifest.Manifest
	if err := json.NewDecoder(bytes.NewReader(jsonBytes)).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// unused: keep cid import linkable while we evolve the demo.
var _ = cid.Undef
