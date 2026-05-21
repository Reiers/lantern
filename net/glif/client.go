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
	if err := json.Unmarshal(all, &rr); err != nil {
		return fmt.Errorf("decode envelope: %w (body %s)", err, truncate(all))
	}
	if rr.Error != nil {
		return fmt.Errorf("glif: %s (code=%d)", rr.Error.Message, rr.Error.Code)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rr.Result, out)
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
	Epoch        abi.ChainEpoch
	TipSetKey    types.TipSetKey
	StateRoot    cid.Cid
	ParentWeight big.Int
	ParentMessageReceipts cid.Cid
}

// FetchHead queries Filecoin.ChainHead.
func (c *Client) FetchHead(ctx context.Context) (*Head, error) {
	var raw struct {
		Cids   []struct{ Slash string `json:"/"` } `json:"Cids"`
		Blocks []struct {
			ParentStateRoot       struct{ Slash string `json:"/"` } `json:"ParentStateRoot"`
			ParentMessageReceipts struct{ Slash string `json:"/"` } `json:"ParentMessageReceipts"`
			ParentWeight          string                              `json:"ParentWeight"`
			Height                abi.ChainEpoch                      `json:"Height"`
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
		Epoch:        raw.Height,
		TipSetKey:    types.NewTipSetKey(cids...),
		StateRoot:    sr,
		ParentWeight: pw,
		ParentMessageReceipts: pmr,
	}, nil
}

// FetchTipsetByHeight queries Filecoin.ChainGetTipSetByHeight at the given
// epoch, returning the block CIDs and (parent-state-root, parent-weight) of
// the first block in the tipset. Caller is responsible for fetching each
// block CID via Get(...) and verifying.
func (c *Client) FetchTipsetByHeight(ctx context.Context, h abi.ChainEpoch) (*Head, error) {
	var raw struct {
		Cids   []struct {
			Slash string `json:"/"`
		} `json:"Cids"`
		Blocks []struct {
			ParentStateRoot       struct{ Slash string `json:"/"` } `json:"ParentStateRoot"`
			ParentMessageReceipts struct{ Slash string `json:"/"` } `json:"ParentMessageReceipts"`
			ParentWeight          string                              `json:"ParentWeight"`
			Height                abi.ChainEpoch                      `json:"Height"`
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
