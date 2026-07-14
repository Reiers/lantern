// Package glif is a fallback JSON-RPC client for Glif's public Filecoin
// endpoint. We use it for two things:
//
//  1. Fetching the current chain head when the primary Lantern gateway is
//     unavailable (PHASE2-BLOCKERS.md B7 pattern).
//  2. Fetching raw IPLD blocks via `Filecoin.ChainReadObj` — Glif's
//     blockstore has every recent state CID in the finality window, so
//     this works as a block-getter for Lantern's state walker.
//
// Every fetched block is CID-verified locally; Glif is treated as a
// dumb-pipe block server, not a trust anchor.

package glif

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/hamt"
)

// DefaultURL is the canonical Glif node RPC.
const DefaultURL = "https://api.node.glif.io/rpc/v1"

// Client is a Glif JSON-RPC client. Goroutine-safe.
type Client struct {
	url string
	hc  *http.Client
}

// New returns a Client. Empty url defaults to DefaultURL.
func New(url string, timeout time.Duration) *Client {
	if url == "" {
		url = DefaultURL
	}
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	return &Client{url: url, hc: &http.Client{Timeout: timeout}}
}

// rpcCall posts a JSON-RPC method+params and decodes the result into out.
func (c *Client) rpcCall(ctx context.Context, method string, params []any, out any) error {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(all))
	}
	var rr struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// Use a streaming decoder rather than Unmarshal so we tolerate stray
	// bytes after the JSON document. Glif's edge occasionally returns
	// the body doubled or appends a stray newline under load — the
	// envelope itself is always the FIRST JSON document, so we stop
	// after the first Decode call.
	dec := json.NewDecoder(bytes.NewReader(all))
	if err := dec.Decode(&rr); err != nil {
		return fmt.Errorf("decode envelope: %w (len=%d body=%s tail=%s)", err, len(all), truncate(all), tailOf(all, 200))
	}
	if rr.Error != nil {
		return fmt.Errorf("glif: %s (code=%d)", rr.Error.Message, rr.Error.Code)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rr.Result, out)
}

func tailOf(b []byte, n int) string {
	if len(b) > n {
		return "..." + string(b[len(b)-n:])
	}
	return string(b)
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// Head returns the current tipset's (epoch, key, stateRoot, parentWeight).
// The shape mirrors Lotus' api.TipSet so we can build a Lantern
// TrustedRoot from it.
type Head struct {
	Epoch                 abi.ChainEpoch
	TipSetKey             types.TipSetKey
	StateRoot             cid.Cid
	ParentWeight          big.Int
	ParentMessageReceipts cid.Cid
}

// FetchHead queries Filecoin.ChainHead.
func (c *Client) FetchHead(ctx context.Context) (*Head, error) {
	var raw struct {
		Cids []struct {
			Slash string `json:"/"`
		} `json:"Cids"`
		Blocks []struct {
			ParentStateRoot struct {
				Slash string `json:"/"`
			} `json:"ParentStateRoot"`
			ParentMessageReceipts struct {
				Slash string `json:"/"`
			} `json:"ParentMessageReceipts"`
			ParentWeight string         `json:"ParentWeight"`
			Height       abi.ChainEpoch `json:"Height"`
		} `json:"Blocks"`
		Height abi.ChainEpoch `json:"Height"`
	}
	if err := c.rpcCall(ctx, "Filecoin.ChainHead", []any{}, &raw); err != nil {
		return nil, err
	}
	if len(raw.Blocks) == 0 {
		return nil, errors.New("glif returned empty head")
	}
	first := raw.Blocks[0]
	sr, err := cid.Parse(first.ParentStateRoot.Slash)
	if err != nil {
		return nil, fmt.Errorf("parse stateRoot: %w", err)
	}
	pmr, _ := cid.Parse(first.ParentMessageReceipts.Slash)
	pw, _ := big.FromString(first.ParentWeight)
	cids := make([]cid.Cid, 0, len(raw.Cids))
	for _, c := range raw.Cids {
		cc, _ := cid.Parse(c.Slash)
		cids = append(cids, cc)
	}
	return &Head{
		Epoch:                 raw.Height,
		TipSetKey:             types.NewTipSetKey(cids...),
		StateRoot:             sr,
		ParentWeight:          pw,
		ParentMessageReceipts: pmr,
	}, nil
}

// FetchTipsetByHeight queries Filecoin.ChainGetTipSetByHeight at the given
// epoch, returning the block CIDs and (parent-state-root, parent-weight) of
// the first block in the tipset. Caller is responsible for fetching each
// block CID via Get(...) and verifying.
func (c *Client) FetchTipsetByHeight(ctx context.Context, h abi.ChainEpoch) (*Head, error) {
	var raw struct {
		Cids []struct {
			Slash string `json:"/"`
		} `json:"Cids"`
		Blocks []struct {
			ParentStateRoot struct {
				Slash string `json:"/"`
			} `json:"ParentStateRoot"`
			ParentMessageReceipts struct {
				Slash string `json:"/"`
			} `json:"ParentMessageReceipts"`
			ParentWeight string         `json:"ParentWeight"`
			Height       abi.ChainEpoch `json:"Height"`
		} `json:"Blocks"`
		Height abi.ChainEpoch `json:"Height"`
	}
	params := []any{int64(h), []any{}}
	if err := c.rpcCall(ctx, "Filecoin.ChainGetTipSetByHeight", params, &raw); err != nil {
		return nil, err
	}
	if len(raw.Blocks) == 0 {
		return nil, fmt.Errorf("glif returned empty tipset at height %d", h)
	}
	first := raw.Blocks[0]
	sr, err := cid.Parse(first.ParentStateRoot.Slash)
	if err != nil {
		return nil, fmt.Errorf("parse stateRoot: %w", err)
	}
	pmr, _ := cid.Parse(first.ParentMessageReceipts.Slash)
	pw, _ := big.FromString(first.ParentWeight)
	cids := make([]cid.Cid, 0, len(raw.Cids))
	for _, c := range raw.Cids {
		cc, _ := cid.Parse(c.Slash)
		cids = append(cids, cc)
	}
	return &Head{
		Epoch:                 raw.Height,
		TipSetKey:             types.NewTipSetKey(cids...),
		StateRoot:             sr,
		ParentWeight:          pw,
		ParentMessageReceipts: pmr,
	}, nil
}

// FetchBlock fetches a single BlockHeader by CID. We use ChainReadObj
// (which returns the raw CBOR bytes) and decode locally. This avoids the
// JSON serialisation roundtrip in ChainGetBlock and is byte-stable.
func (c *Client) FetchBlock(ctx context.Context, k cid.Cid) (*types.BlockHeader, error) {
	raw, err := c.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	bh, err := types.DecodeBlock(raw)
	if err != nil {
		return nil, fmt.Errorf("decode block %s: %w", k, err)
	}
	return bh, nil
}

// HeadEpoch satisfies header/store.RPCSource.
func (c *Client) HeadEpoch(ctx context.Context) (abi.ChainEpoch, error) {
	h, err := c.FetchHead(ctx)
	if err != nil {
		return 0, err
	}
	return h.Epoch, nil
}

// TipsetCIDsByHeight satisfies header/store.RPCSource.
func (c *Client) TipsetCIDsByHeight(ctx context.Context, h abi.ChainEpoch) ([]cid.Cid, error) {
	ts, err := c.FetchTipsetByHeight(ctx, h)
	if err != nil {
		return nil, err
	}
	return ts.TipSetKey.Cids(), nil
}

// Get implements state/hamt.BlockGetter via Filecoin.ChainReadObj.
func (c *Client) Get(ctx context.Context, k cid.Cid) ([]byte, error) {
	var raw string
	cidParam := map[string]any{"/": k.String()}
	if err := c.rpcCall(ctx, "Filecoin.ChainReadObj", []any{cidParam}, &raw); err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	if err := hamt.VerifyBlockCID(k, b); err != nil {
		return nil, err
	}
	return b, nil
}

// Compile check.
var _ hamt.BlockGetter = (*Client)(nil)

// StateNetworkName queries Filecoin.StateNetworkName so a Lantern client
// pointed at an unknown network (devnet, forkline, ...) can discover the
// wire-name string libp2p protocols expect (gossipsub topics, DHT prefix).
func (c *Client) StateNetworkName(ctx context.Context) (string, error) {
	var name string
	if err := c.rpcCall(ctx, "Filecoin.StateNetworkName", []any{}, &name); err != nil {
		return "", err
	}
	return name, nil
}

// FetchGenesis queries Filecoin.ChainGetGenesis and returns block 0's CID.
// The devnet-init path uses this to bind the local devnet's genesis into
// the Lantern config so /fil/hello/1.0.0 identifies the correct chain.
func (c *Client) FetchGenesis(ctx context.Context) (cid.Cid, error) {
	var raw struct {
		Cids []struct {
			Slash string `json:"/"`
		} `json:"Cids"`
	}
	if err := c.rpcCall(ctx, "Filecoin.ChainGetGenesis", []any{}, &raw); err != nil {
		return cid.Undef, err
	}
	if len(raw.Cids) == 0 {
		return cid.Undef, errors.New("ChainGetGenesis returned empty tipset")
	}
	return cid.Parse(raw.Cids[0].Slash)
}

// StateNetworkVersion queries Filecoin.StateNetworkVersion at the current
// head and returns the network version. The devnet-init path records it so
// the daemon can map the devnet's actor code CIDs to the right go-state-
// types actor-version decoders.
func (c *Client) StateNetworkVersion(ctx context.Context) (uint64, error) {
	var nv uint64
	// StateNetworkVersion takes a TipSetKey; an empty array selects head.
	if err := c.rpcCall(ctx, "Filecoin.StateNetworkVersion", []any{[]any{}}, &nv); err != nil {
		return 0, err
	}
	return nv, nil
}

// StateActorCodeCIDs queries Filecoin.StateActorCodeCIDs for the given
// network version and returns the actor-name -> code-CID map. A custom
// devnet (debug-compiled builtin-actors) ships code CIDs that are in no
// released bundle; recording them lets Lantern's actor registry decode
// devnet state that would otherwise be "unknown code CID".
func (c *Client) StateActorCodeCIDs(ctx context.Context, networkVersion uint64) (map[string]cid.Cid, error) {
	var raw map[string]struct {
		Slash string `json:"/"`
	}
	if err := c.rpcCall(ctx, "Filecoin.StateActorCodeCIDs", []any{networkVersion}, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]cid.Cid, len(raw))
	for name, v := range raw {
		c, err := cid.Parse(v.Slash)
		if err != nil {
			return nil, fmt.Errorf("parse code cid for %q (%s): %w", name, v.Slash, err)
		}
		out[name] = c
	}
	return out, nil
}

// EthChainID queries eth_chainId and returns the decimal chain identifier.
// The devnet-init path uses this to bind the devnet's EIP-155 chain ID
// into the Lantern config so `eth_chainId` + `net_version` on the daemon
// return the same value clients pass to `docker compose up` (Curio's
// devnet defaults to 31415926 / 0x1df5e76).
func (c *Client) EthChainID(ctx context.Context) (uint64, error) {
	var hex string
	if err := c.rpcCall(ctx, "eth_chainId", []any{}, &hex); err != nil {
		return 0, err
	}
	s := hex
	if len(s) >= 2 && s[:2] == "0x" {
		s = s[2:]
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse eth_chainId %q: %w", hex, err)
	}
	return v, nil
}

// BlockDelaySecs queries Filecoin.Version and returns the BlockDelay
// field (block cadence in seconds). Mainnet = 30, calibration = 30,
// curio-fork docker devnet = 4. Devnet setups may customize this via
// the `//go:build 2k` variant, so we bind it at devnet-init time.
func (c *Client) BlockDelaySecs(ctx context.Context) (uint64, error) {
	var v struct {
		BlockDelay uint64 `json:"BlockDelay"`
	}
	if err := c.rpcCall(ctx, "Filecoin.Version", []any{}, &v); err != nil {
		return 0, err
	}
	return v.BlockDelay, nil
}

// MpoolPush pushes a signed message via the upstream lotus RPC's
// Filecoin.MpoolPush method. Used by devnet mode (`--network devnet`)
// as the send-path sink for the local mpool: a single-node docker devnet
// can't form a gossipsub mesh, so the pubsub topic never propagates.
// The devnet's lotus accepts signed messages directly via this JSON-RPC
// method and does its own inclusion.
//
// Trust posture: unchanged from #122. Devnet is single-source by design
// (operator owns the devnet). This method is NOT used on mainnet/
// calibration; the gossipsub publisher path continues to be the only
// producer of network traffic there.
func (c *Client) MpoolPush(ctx context.Context, sm *types.SignedMessage) (cid.Cid, error) {
	if sm == nil {
		return cid.Undef, errors.New("glif: MpoolPush nil signed message")
	}
	var out struct {
		Slash string `json:"/"`
	}
	if err := c.rpcCall(ctx, "Filecoin.MpoolPush", []any{sm}, &out); err != nil {
		return cid.Undef, err
	}
	if out.Slash == "" {
		return cid.Undef, fmt.Errorf("glif: MpoolPush returned empty CID; local CID would have been %s", sm.Cid())
	}
	return cid.Parse(out.Slash)
}
