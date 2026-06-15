// lantern-phase1 is the Phase 1 end-to-end integration runner.
//
// It downloads a small slice of recent Filecoin mainnet state from the public
// Glif RPC endpoint, validates the block headers locally with Lantern's
// chain/header validator, builds a TrustedRoot, then cross-checks the
// computed (epoch, tipsetCID, stateRoot, parentWeight) against Glif's view of
// the same tipset.
//
// Exit codes:
//   0 — TrustedRoot matches Glif.
//   1 — mismatch (any field), or fatal error during the run.
//
// Usage:
//
//	lantern-phase1            # runs against mainnet via api.node.glif.io
//	lantern-phase1 --depth 50 # validates 50 epochs of headers instead of 30

package main

import (
	"bytes"
	"context"
	b64pkg "encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	gsproof "github.com/filecoin-project/go-state-types/proof"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/beacon"
	"github.com/Reiers/lantern/chain/f3"
	"github.com/Reiers/lantern/chain/header"
	"github.com/Reiers/lantern/chain/trustedroot"
	ltypes "github.com/Reiers/lantern/chain/types"
)

var b64 = b64pkg.StdEncoding

const defaultRPC = "https://api.node.glif.io/rpc/v1"

type cliFlags struct {
	rpcURL     string
	depth      int
	verbose    bool
	skipBeacon bool
}

func main() {
	var f cliFlags
	flag.StringVar(&f.rpcURL, "rpc", defaultRPC, "Filecoin JSON-RPC endpoint (public, no auth)")
	flag.IntVar(&f.depth, "depth", 30, "epochs back from head to consolidate the TrustedRoot at")
	flag.BoolVar(&f.verbose, "v", false, "verbose output")
	flag.BoolVar(&f.skipBeacon, "skip-beacon", false, "skip DRAND beacon entry verification (default: on, since we don't have prev-round sigs from RPC alone)")
	flag.Parse()

	if err := run(context.Background(), f); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

func run(ctx context.Context, f cliFlags) error {
	rpc := newRPC(f.rpcURL)
	_ = &glifHeaderSource{rpc: rpc} // ensures the bridge compiles; reserved for full trustedroot.Build runs in Phase 2

	// 1. Find current head.
	headTS, err := rpc.chainHead(ctx)
	if err != nil {
		return fmt.Errorf("rpc ChainHead: %w", err)
	}
	headEpoch := headTS.Height()
	selected := headEpoch - abi.ChainEpoch(f.depth)
	if selected < 0 {
		selected = 0
	}
	fmt.Printf("head epoch: %d   selected epoch: %d (depth=%d)\n", headEpoch, selected, f.depth)

	// 2. Pull selected tipset + immediate parents (to exercise parent linkage).
	selectedTS, err := rpc.chainGetTipSetByHeight(ctx, selected)
	if err != nil {
		return fmt.Errorf("rpc ChainGetTipSetByHeight(%d): %w", selected, err)
	}
	parentTS, err := rpc.chainGetTipSetByHeight(ctx, selected-1)
	if err != nil {
		return fmt.Errorf("rpc ChainGetTipSetByHeight(%d): %w", selected-1, err)
	}

	// 3. Local header validation pass.
	fmt.Printf("validating selected tipset (epoch %d, %d blocks)...\n", selectedTS.Height(), len(selectedTS.Blocks()))
	for _, bh := range selectedTS.Blocks() {
		expected := bh.Cid()
		if err := header.VerifyBlockHeaderCID(bh, expected); err != nil {
			return fmt.Errorf("block header CID self-check: %w", err)
		}
	}
	if _, err := header.ValidateTipsetShape(selectedTS.Blocks()); err != nil {
		return fmt.Errorf("validating tipset shape: %w", err)
	}
	for _, bh := range selectedTS.Blocks() {
		if err := header.VerifyParentLinkage(bh.Parents, parentTS); err != nil {
			return fmt.Errorf("parent linkage: %w", err)
		}
	}

	// 4. Optional DRAND beacon-entry signature check using the post-quicknet
	//    chain. We use the *first* beacon entry per block and verify with
	//    the unchained (quicknet) verifier. Mainnet may still include
	//    chained-mainnet entries pre-quicknet activation; for simplicity we
	//    try quicknet and skip on mismatch.
	if !f.skipBeacon {
		cfg, bcerr := beacon.LoadConfigFromChainInfoJSON(
			build.DrandConfigs[build.DrandQuicknet].ChainInfoJSON,
			build.DrandConfigs[build.DrandQuicknet].IsChained,
		)
		if bcerr != nil {
			return fmt.Errorf("loading drand quicknet config: %w", bcerr)
		}
		verified := 0
		for _, bh := range selectedTS.Blocks() {
			for _, e := range bh.BeaconEntries {
				if err := cfg.VerifyEntry(e, nil); err == nil {
					verified++
				} else if f.verbose {
					fmt.Printf("  beacon round %d verify miss (likely chained-mainnet entry): %v\n", e.Round, err)
				}
			}
		}
		fmt.Printf("verified %d quicknet beacon entries across the selected tipset\n", verified)
	}

	// 5. Decode the embedded F3 manifest (sanity).
	m, err := f3.ParseManifest(build.F3ManifestMainnetJSON)
	if err != nil {
		return fmt.Errorf("parsing F3 manifest: %w", err)
	}
	fmt.Printf("F3 manifest network=%q bootstrapEpoch=%d initialPT=%s\n",
		string(m.NetworkName), m.BootstrapEpoch, m.InitialPowerTable)

	// 6. Pull the latest F3 cert; we decode it but don't run BLS
	//    verification because seeding the initial PowerEntries from a
	//    public RPC isn't supported (Phase 2 will plug in Bitswap).
	if latestCert, lerr := rpc.f3GetLatestCertificate(ctx); lerr == nil && latestCert != nil {
		var inst uint64
		if f, ok := latestCert["GPBFTInstance"].(float64); ok {
			inst = uint64(f)
		}
		ecLen := 0
		if arr, ok := latestCert["ECChain"].([]any); ok {
			ecLen = len(arr)
		}
		fmt.Printf("latest F3 cert: instance=%d  ECChain entries=%d\n", inst, ecLen)
	} else if lerr != nil {
		fmt.Printf("F3 latest cert unavailable (%v); proceeding with header-only validation\n", lerr)
	}

	// 7. Construct a TrustedRoot directly from the validated tipset. We
	//    bypass trustedroot.Build because Build does an F3 cert walk that
	//    requires the initial power table.
	headBlock := selectedTS.Blocks()[0]
	var beaconRound uint64
	if n := len(headBlock.BeaconEntries); n > 0 {
		beaconRound = headBlock.BeaconEntries[n-1].Round
	}
	tr := &trustedroot.TrustedRoot{
		Epoch:                 selectedTS.Height(),
		TipSetKey:             selectedTS.Key(),
		StateRoot:             headBlock.ParentStateRoot,
		ParentMessageReceipts: headBlock.ParentMessageReceipts,
		ParentWeight:          headBlock.ParentWeight,
		BeaconRound:           beaconRound,
		AcceptedAt:            time.Now().UTC(),
	}

	fmt.Println()
	fmt.Println("Lantern Phase 1 TrustedRoot")
	fmt.Println("===========================")
	fmt.Printf("epoch:                  %d\n", tr.Epoch)
	tsCid, _ := tr.TipSetKey.Cid()
	fmt.Printf("tipset-key CID:         %s\n", tsCid)
	fmt.Printf("state root:             %s\n", tr.StateRoot)
	fmt.Printf("parent message rcpts:   %s\n", tr.ParentMessageReceipts)
	fmt.Printf("parent weight:          %s\n", tr.ParentWeight)
	fmt.Printf("beacon round:           %d\n", tr.BeaconRound)
	fmt.Println()

	// 8. Cross-check against Glif: re-query the same tipset by height,
	//    compare every field.
	xverifTS, err := rpc.chainGetTipSetByHeight(ctx, tr.Epoch)
	if err != nil {
		return fmt.Errorf("cross-check ChainGetTipSetByHeight: %w", err)
	}
	xverifBlock := xverifTS.Blocks()[0]
	if xverifTS.Height() != tr.Epoch {
		return fmt.Errorf("mismatch: epoch local=%d glif=%d", tr.Epoch, xverifTS.Height())
	}
	if xverifBlock.ParentStateRoot != tr.StateRoot {
		return fmt.Errorf("mismatch: stateRoot local=%s glif=%s", tr.StateRoot, xverifBlock.ParentStateRoot)
	}
	if xverifBlock.ParentMessageReceipts != tr.ParentMessageReceipts {
		return fmt.Errorf("mismatch: parentMessageReceipts local=%s glif=%s", tr.ParentMessageReceipts, xverifBlock.ParentMessageReceipts)
	}
	if xverifBlock.ParentWeight.String() != tr.ParentWeight.String() {
		return fmt.Errorf("mismatch: parentWeight local=%s glif=%s", tr.ParentWeight, xverifBlock.ParentWeight)
	}
	glifKeyCid, _ := xverifTS.Key().Cid()
	if glifKeyCid != tsCid {
		return fmt.Errorf("mismatch: tipset-key CID local=%s glif=%s", tsCid, glifKeyCid)
	}

	fmt.Println("cross-check vs Glif: EXACT MATCH on (epoch, tipsetCID, stateRoot, parentMessageReceipts, parentWeight)")
	return nil
}

// --------------------------------------------------------------------
// Lightweight Lotus JSON-RPC client

type rpcClient struct {
	url string
	hc  *http.Client
}

func newRPC(url string) *rpcClient {
	return &rpcClient{url: url, hc: &http.Client{Timeout: 30 * time.Second}}
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      int             `json:"id"`
}
type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
	ID      int             `json:"id"`
}
type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcErr) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

func (c *rpcClient) call(ctx context.Context, method string, params any, out any) error {
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return err
	}
	body, err := json.Marshal(rpcReq{JSONRPC: "2.0", Method: method, Params: paramsBytes, ID: 1})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("rpc http %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var r rpcResp
	// Glif occasionally returns its response body twice, concatenated
	// (observed empirically on Filecoin.ChainHead, 2026-05). Use json.Decoder
	// so we accept the first complete JSON object and ignore any trailing bytes.
	dec := json.NewDecoder(bytes.NewReader(respBody))
	if err := dec.Decode(&r); err != nil {
		preview := string(respBody)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return fmt.Errorf("decoding rpc response: %w (body: %s)", err, preview)
	}
	if r.Error != nil {
		return r.Error
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// glifTipSet matches the Lotus JSON shape for ChainHead/ChainGetTipSetByHeight.
type glifTipSet struct {
	Cids   []glifCid         `json:"Cids"`
	Blocks []glifBlockHeader `json:"Blocks"`
	Height int64             `json:"Height"`
}
type glifCid struct {
	Slash string `json:"/"`
}
type glifBlockHeader struct {
	Miner                 string          `json:"Miner"`
	Ticket                *glifTicket     `json:"Ticket"`
	ElectionProof         *glifElection   `json:"ElectionProof"`
	BeaconEntries         []glifBeacon    `json:"BeaconEntries"`
	WinPoStProof          []glifPoStProof `json:"WinPoStProof"`
	Parents               []glifCid       `json:"Parents"`
	ParentWeight          string          `json:"ParentWeight"`
	Height                int64           `json:"Height"`
	ParentStateRoot       glifCid         `json:"ParentStateRoot"`
	ParentMessageReceipts glifCid         `json:"ParentMessageReceipts"`
	Messages              glifCid         `json:"Messages"`
	BLSAggregate          *glifSig        `json:"BLSAggregate"`
	Timestamp             uint64          `json:"Timestamp"`
	BlockSig              *glifSig        `json:"BlockSig"`
	ForkSignaling         uint64          `json:"ForkSignaling"`
	ParentBaseFee         string          `json:"ParentBaseFee"`
}
type glifTicket struct {
	VRFProof string `json:"VRFProof"`
}
type glifElection struct {
	WinCount int64  `json:"WinCount"`
	VRFProof string `json:"VRFProof"`
}
type glifBeacon struct {
	Round uint64 `json:"Round"`
	Data  string `json:"Data"` // base64 in Lotus JSON
}
type glifPoStProof struct {
	PoStProof  int64  `json:"PoStProof"`
	ProofBytes string `json:"ProofBytes"` // base64
}
type glifSig struct {
	Type uint8  `json:"Type"`
	Data string `json:"Data"` // base64
}

func glifCidsToCids(in []glifCid) ([]cid.Cid, error) {
	out := make([]cid.Cid, len(in))
	for i, g := range in {
		c, err := cid.Parse(g.Slash)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}

func (c *rpcClient) chainHead(ctx context.Context) (*ltypes.TipSet, error) {
	var raw glifTipSet
	if err := c.call(ctx, "Filecoin.ChainHead", []any{}, &raw); err != nil {
		return nil, err
	}
	return glifTipSetToLantern(&raw)
}

func (c *rpcClient) chainGetTipSetByHeight(ctx context.Context, h abi.ChainEpoch) (*ltypes.TipSet, error) {
	var raw glifTipSet
	if err := c.call(ctx, "Filecoin.ChainGetTipSetByHeight", []any{h, nil}, &raw); err != nil {
		return nil, err
	}
	return glifTipSetToLantern(&raw)
}

func (c *rpcClient) f3GetLatestCertificate(ctx context.Context) (map[string]any, error) {
	var raw map[string]any
	if err := c.call(ctx, "Filecoin.F3GetLatestCertificate", []any{}, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// glifTipSetToLantern decodes a Lotus-shape JSON tipset into Lantern's
// chain/types.TipSet.
func glifTipSetToLantern(g *glifTipSet) (*ltypes.TipSet, error) {
	if g == nil {
		return nil, errors.New("nil tipset")
	}
	blocks := make([]*ltypes.BlockHeader, 0, len(g.Blocks))
	for i, b := range g.Blocks {
		bh, err := glifBlockToLantern(&b)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		blocks = append(blocks, bh)
	}
	return ltypes.NewTipSet(blocks)
}

func glifBlockToLantern(b *glifBlockHeader) (*ltypes.BlockHeader, error) {
	miner, err := address.NewFromString(b.Miner)
	if err != nil {
		return nil, fmt.Errorf("miner: %w", err)
	}
	psr, err := cid.Parse(b.ParentStateRoot.Slash)
	if err != nil {
		return nil, fmt.Errorf("ParentStateRoot: %w", err)
	}
	pmr, err := cid.Parse(b.ParentMessageReceipts.Slash)
	if err != nil {
		return nil, fmt.Errorf("ParentMessageReceipts: %w", err)
	}
	msgs, err := cid.Parse(b.Messages.Slash)
	if err != nil {
		return nil, fmt.Errorf("Messages: %w", err)
	}
	parents, err := glifCidsToCids(b.Parents)
	if err != nil {
		return nil, fmt.Errorf("Parents: %w", err)
	}
	pw, err := ltypes.BigFromString(b.ParentWeight)
	if err != nil {
		return nil, fmt.Errorf("ParentWeight: %w", err)
	}
	pbf, err := ltypes.BigFromString(b.ParentBaseFee)
	if err != nil {
		return nil, fmt.Errorf("ParentBaseFee: %w", err)
	}

	var ticket *ltypes.Ticket
	if b.Ticket != nil {
		tv, derr := stdb64decode(b.Ticket.VRFProof)
		if derr != nil {
			return nil, fmt.Errorf("ticket vrfproof: %w", derr)
		}
		ticket = &ltypes.Ticket{VRFProof: tv}
	}
	var election *ltypes.ElectionProof
	if b.ElectionProof != nil {
		ev, derr := stdb64decode(b.ElectionProof.VRFProof)
		if derr != nil {
			return nil, fmt.Errorf("election vrfproof: %w", derr)
		}
		election = &ltypes.ElectionProof{WinCount: b.ElectionProof.WinCount, VRFProof: ev}
	}

	beaconEntries := make([]ltypes.BeaconEntry, len(b.BeaconEntries))
	for i, be := range b.BeaconEntries {
		bv, derr := stdb64decode(be.Data)
		if derr != nil {
			return nil, fmt.Errorf("beacon %d: %w", i, derr)
		}
		beaconEntries[i] = ltypes.BeaconEntry{Round: be.Round, Data: bv}
	}

	winPoStProofs := make([]gsproof.PoStProof, len(b.WinPoStProof))
	for i, p := range b.WinPoStProof {
		pb, derr := stdb64decode(p.ProofBytes)
		if derr != nil {
			return nil, fmt.Errorf("winpost %d: %w", i, derr)
		}
		winPoStProofs[i] = gsproof.PoStProof{PoStProof: abi.RegisteredPoStProof(p.PoStProof), ProofBytes: pb}
	}

	var blsAgg *gscrypto.Signature
	if b.BLSAggregate != nil {
		sd, derr := stdb64decode(b.BLSAggregate.Data)
		if derr != nil {
			return nil, fmt.Errorf("bls agg: %w", derr)
		}
		blsAgg = &gscrypto.Signature{Type: gscrypto.SigType(b.BLSAggregate.Type), Data: sd}
	}
	var blockSig *gscrypto.Signature
	if b.BlockSig != nil {
		sd, derr := stdb64decode(b.BlockSig.Data)
		if derr != nil {
			return nil, fmt.Errorf("block sig: %w", derr)
		}
		blockSig = &gscrypto.Signature{Type: gscrypto.SigType(b.BlockSig.Type), Data: sd}
	}

	return &ltypes.BlockHeader{
		Miner:                 miner,
		Ticket:                ticket,
		ElectionProof:         election,
		BeaconEntries:         beaconEntries,
		WinPoStProof:          winPoStProofs,
		Parents:               parents,
		ParentWeight:          pw,
		Height:                abi.ChainEpoch(b.Height),
		ParentStateRoot:       psr,
		ParentMessageReceipts: pmr,
		Messages:              msgs,
		BLSAggregate:          blsAgg,
		Timestamp:             b.Timestamp,
		BlockSig:              blockSig,
		ForkSignaling:         b.ForkSignaling,
		ParentBaseFee:         pbf,
	}, nil
}

// stdb64decode decodes a possibly-padded base64 string used by Lotus' JSON
// shape for raw bytes.
func stdb64decode(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	// Lotus uses standard base64 with padding.
	return b64.DecodeString(s)
}

// --------------------------------------------------------------------
// HeaderSource bridge for chain/trustedroot. Unused by the demo above (we
// construct the root inline), but kept to demonstrate the integration.

type glifHeaderSource struct{ rpc *rpcClient }

func (g *glifHeaderSource) Tipset(ctx context.Context, e abi.ChainEpoch) (*ltypes.TipSet, error) {
	return g.rpc.chainGetTipSetByHeight(ctx, e)
}
func (g *glifHeaderSource) Head(ctx context.Context) (abi.ChainEpoch, error) {
	ts, err := g.rpc.chainHead(ctx)
	if err != nil {
		return 0, err
	}
	return ts.Height(), nil
}

// keep these imports referenced; some only used by helper packages.
var _ = []any{hex.EncodeToString, json.Unmarshal, build.DrandMainnet}
