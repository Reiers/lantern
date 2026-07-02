// ChainExchange CLIENT (issue #91, Stage A of the full-node epic #87).
//
// The responder half of this package (chainxchg.go, issue #17) made us
// REACHABLE on /fil/chain/xchg/0.0.1. This file adds the requesting
// half: fetch tipset header chains from real Filecoin peers over
// libp2p, so a bridge-off Lantern (#76, --no-fallback-rpc) can seed its
// header store from the quorum anchor and backfill gossip gaps with
// ZERO upstream RPC (no Glif, no gateway).
//
// Scope (per the #91 survey): client-only, headers-only.
//   - Request{Head []cid, Length, Options} — Options bit 0x1 = Headers,
//     0x2 = Messages. We only ever set Headers; the response's per-
//     tipset Messages slot is then null and is skipped with a Deferred.
//   - Response{Status, ErrorMessage, Chain []BSTipSet} where
//     BSTipSet{Blocks []BlockHeader, Messages}. Chain[0] is the tipset
//     identified by Head; entries walk BACKWARD (newest -> oldest).
//   - Status 101 (Partial) is legal: a peer may return fewer tipsets
//     than asked. The caller loops if it needs more.
//
// Trust model: every returned header is CID-verified. Chain[0]'s block
// CIDs must equal the requested Head CIDs exactly (set equality), and
// each deeper level must equal the previous level's Parents. The caller
// always derives Head from something already verified (a gossip block's
// Parents, or the multi-source quorum anchor), so a malicious peer
// cannot splice us onto a fabricated chain — it can only refuse.
//
// Wire format cross-checked against Lotus chain/exchange/protocol.go +
// cbor_gen.go (handwritten here to keep the dependency surface at zero,
// same policy as the responder).

package chainxchg

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	cbg "github.com/whyrusleeping/cbor-gen"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// Options bits (Lotus chain/exchange/protocol.go).
const (
	optHeaders uint64 = 1 << iota
	optMessages
)

// Additional status codes the client must understand (the responder
// side above only ever emits statusNotFound).
const (
	statusPartial uint64 = 101
	statusGoAway  uint64 = 202
)

// MaxRequestLength is the protocol cap on tipsets per request.
const MaxRequestLength = 900

var (
	clientWriteDeadline = 10 * time.Second
	clientReadDeadline  = 45 * time.Second
	// maxResponseBytes bounds a headers-only response read. Worst
	// legitimate case (900 tipsets x ~10 blocks x ~2 KiB/header) is
	// ~18 MiB; 32 MiB leaves headroom without letting a malicious
	// peer feed us unbounded data.
	maxResponseBytes int64 = 32 << 20
	// maxPeerTries bounds how many peers one FetchTipsetChain call
	// will rotate through before giving up.
	maxPeerTries = 8
	// maxErrMsgLen bounds the ErrorMessage string we will read.
	maxErrMsgLen uint64 = 1024
	// maxBlocksPerTipset bounds decoded blocks per tipset (Filecoin
	// max is 10 wincount-weighted; 32 is generous).
	maxBlocksPerTipset uint64 = 32
)

// Client is a ChainExchange requester bound to a libp2p host.
type Client struct {
	h host.Host

	// preferredPeers, when set, returns peers to try FIRST (e.g. the
	// trusted floor / bootstrap quorum peers). Others follow shuffled.
	preferredPeers func() []peer.ID

	requests  atomic.Uint64 // FetchTipsetChain calls
	succeeded atomic.Uint64 // calls that returned >=1 verified tipset
	failed    atomic.Uint64 // calls that exhausted all peers
	peerTries atomic.Uint64 // per-peer attempts (success + failure)
}

// NewClient constructs a ChainExchange client on h. Register() is NOT
// required; the client only opens outbound streams.
func NewClient(h host.Host) *Client {
	return &Client{h: h}
}

// SetPreferredPeers wires a provider of first-choice peers (trusted
// floor / quorum peers). Optional.
func (c *Client) SetPreferredPeers(f func() []peer.ID) { c.preferredPeers = f }

// ClientStats reports observable activity for the dashboard.
type ClientStats struct {
	Requests  uint64 `json:"requests"`
	Succeeded uint64 `json:"succeeded"`
	Failed    uint64 `json:"failed"`
	PeerTries uint64 `json:"peer_tries"`
}

// ClientStats returns a snapshot.
func (c *Client) ClientStats() ClientStats {
	return ClientStats{
		Requests:  c.requests.Load(),
		Succeeded: c.succeeded.Load(),
		Failed:    c.failed.Load(),
		PeerTries: c.peerTries.Load(),
	}
}

// FetchTipsetChain requests up to length tipsets of HEADERS starting at
// the tipset whose block CIDs are head, walking parent-ward. The result
// is ordered as received: index 0 = the head tipset, increasing index =
// older. Every header is CID-verified and every level is verified to be
// the parent set of the level above; the first level must match head
// exactly. Returns at least one tipset on success (partial responses
// are legal and returned as-is).
func (c *Client) FetchTipsetChain(ctx context.Context, head []cid.Cid, length int) ([][]*ltypes.BlockHeader, error) {
	if len(head) == 0 {
		return nil, fmt.Errorf("chainxchg client: empty head")
	}
	if length < 1 {
		length = 1
	}
	if length > MaxRequestLength {
		length = MaxRequestLength
	}
	c.requests.Add(1)

	peers := c.candidatePeers()
	if len(peers) == 0 {
		c.failed.Add(1)
		return nil, fmt.Errorf("chainxchg client: no connected peers support %s", ProtocolID)
	}
	if len(peers) > maxPeerTries {
		peers = peers[:maxPeerTries]
	}

	var lastErr error
	for _, p := range peers {
		if ctx.Err() != nil {
			c.failed.Add(1)
			return nil, ctx.Err()
		}
		c.peerTries.Add(1)
		chain, err := c.fetchFromPeer(ctx, p, head, length)
		if err != nil {
			lastErr = err
			log.Debugw("chainxchg client: peer failed", "peer", p, "err", err)
			continue
		}
		c.succeeded.Add(1)
		return chain, nil
	}
	c.failed.Add(1)
	return nil, fmt.Errorf("chainxchg client: all %d peers failed, last: %w", len(peers), lastErr)
}

// candidatePeers returns connected peers that advertise the protocol,
// preferred peers first, the rest shuffled.
func (c *Client) candidatePeers() []peer.ID {
	connected := c.h.Network().Peers()
	supports := make(map[peer.ID]bool, len(connected))
	var pool []peer.ID
	for _, p := range connected {
		if p == c.h.ID() {
			continue
		}
		protos, err := c.h.Peerstore().SupportsProtocols(p, ProtocolID)
		if err != nil || len(protos) == 0 {
			continue
		}
		supports[p] = true
		pool = append(pool, p)
	}
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	if c.preferredPeers == nil {
		return pool
	}
	seen := make(map[peer.ID]bool, len(pool))
	var out []peer.ID
	for _, p := range c.preferredPeers() {
		if supports[p] && !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	for _, p := range pool {
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out
}

// fetchFromPeer performs one request/response exchange with one peer
// and fully verifies the returned chain.
func (c *Client) fetchFromPeer(ctx context.Context, p peer.ID, head []cid.Cid, length int) ([][]*ltypes.BlockHeader, error) {
	str, err := c.h.NewStream(ctx, p, ProtocolID)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}
	defer func() { _ = str.Close() }()

	_ = str.SetWriteDeadline(time.Now().Add(clientWriteDeadline))
	if err := writeRequest(str, head, uint64(length), optHeaders); err != nil {
		_ = str.Reset()
		return nil, fmt.Errorf("write request: %w", err)
	}
	// Half-close so the remote sees EOF on its read (standard Lotus
	// client behaviour; our own responder relies on it too).
	_ = str.CloseWrite()
	_ = str.SetWriteDeadline(time.Time{})

	_ = str.SetReadDeadline(time.Now().Add(clientReadDeadline))
	defer func() { _ = str.SetReadDeadline(time.Time{}) }()

	chain, status, errMsg, err := readResponse(io.LimitReader(str, maxResponseBytes))
	if err != nil {
		_ = str.Reset()
		return nil, fmt.Errorf("read response: %w", err)
	}
	switch status {
	case statusOK, statusPartial:
		// fine
	case statusNotFound:
		return nil, fmt.Errorf("peer does not have tipset (NotFound): %s", errMsg)
	case statusGoAway:
		return nil, fmt.Errorf("peer sent GoAway: %s", errMsg)
	default:
		return nil, fmt.Errorf("peer returned status %d: %s", status, errMsg)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("peer returned OK with empty chain")
	}
	if err := verifyChain(head, chain); err != nil {
		return nil, fmt.Errorf("chain verification: %w", err)
	}
	return chain, nil
}

// verifyChain enforces the trust model: level 0 == requested head CIDs,
// level i+1 == level i's Parents, heights strictly decreasing, and all
// blocks within a level share the same Parents.
func verifyChain(head []cid.Cid, chain [][]*ltypes.BlockHeader) error {
	expected := cidSet(head)
	var prevHeight int64 = -1
	for i, blocks := range chain {
		if len(blocks) == 0 {
			return fmt.Errorf("level %d: empty tipset", i)
		}
		got := make(map[cid.Cid]bool, len(blocks))
		for _, b := range blocks {
			got[b.Cid()] = true
		}
		if len(got) != len(expected) {
			return fmt.Errorf("level %d: got %d distinct blocks, expected %d", i, len(got), len(expected))
		}
		for c := range expected {
			if !got[c] {
				return fmt.Errorf("level %d: missing expected block %s", i, c)
			}
		}
		h := int64(blocks[0].Height)
		if prevHeight >= 0 && h >= prevHeight {
			return fmt.Errorf("level %d: height %d not below previous %d", i, h, prevHeight)
		}
		prevHeight = h
		parents := cidSet(blocks[0].Parents)
		for _, b := range blocks[1:] {
			if !sameCidSet(parents, cidSet(b.Parents)) {
				return fmt.Errorf("level %d: blocks disagree on parents", i)
			}
		}
		expected = parents
	}
	return nil
}

func cidSet(cids []cid.Cid) map[cid.Cid]bool {
	m := make(map[cid.Cid]bool, len(cids))
	for _, c := range cids {
		m[c] = true
	}
	return m
}

func sameCidSet(a, b map[cid.Cid]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for c := range a {
		if !b[c] {
			return false
		}
	}
	return true
}

// writeRequest emits Request{Head, Length, Options} as CBOR array(3),
// matching Lotus exchange cbor_gen.go layout.
func writeRequest(w io.Writer, head []cid.Cid, length, options uint64) error {
	cw := cbg.NewCborWriter(w)
	if err := cw.WriteMajorTypeHeader(cbg.MajArray, 3); err != nil {
		return err
	}
	if err := cw.WriteMajorTypeHeader(cbg.MajArray, uint64(len(head))); err != nil {
		return err
	}
	for _, c := range head {
		if err := cbg.WriteCid(cw, c); err != nil {
			return err
		}
	}
	if err := cw.WriteMajorTypeHeader(cbg.MajUnsignedInt, length); err != nil {
		return err
	}
	return cw.WriteMajorTypeHeader(cbg.MajUnsignedInt, options)
}

// readResponse decodes Response{Status, ErrorMessage, Chain []BSTipSet}
// keeping only headers. Each BSTipSet is array(2){Blocks, Messages};
// Messages is consumed with a Deferred (null for headers-only, but a
// peer that sends messages anyway is tolerated and skipped).
func readResponse(r io.Reader) (chain [][]*ltypes.BlockHeader, status uint64, errMsg string, err error) {
	cr := cbg.NewCborReader(r)

	maj, n, err := cr.ReadHeader()
	if err != nil {
		return nil, 0, "", fmt.Errorf("response envelope: %w", err)
	}
	if maj != cbg.MajArray || n != 3 {
		return nil, 0, "", fmt.Errorf("response envelope: want array(3), got maj=%d n=%d", maj, n)
	}

	maj, status, err = cr.ReadHeader()
	if err != nil || maj != cbg.MajUnsignedInt {
		return nil, 0, "", fmt.Errorf("response status: maj=%d err=%v", maj, err)
	}

	maj, slen, err := cr.ReadHeader()
	if err != nil || maj != cbg.MajTextString {
		return nil, 0, "", fmt.Errorf("response errmsg header: maj=%d err=%v", maj, err)
	}
	if slen > maxErrMsgLen {
		return nil, 0, "", fmt.Errorf("response errmsg too long: %d", slen)
	}
	if slen > 0 {
		buf := make([]byte, slen)
		if _, err := io.ReadFull(cr, buf); err != nil {
			return nil, 0, "", fmt.Errorf("response errmsg body: %w", err)
		}
		errMsg = string(buf)
	}

	maj, tn, err := cr.ReadHeader()
	if err != nil || maj != cbg.MajArray {
		return nil, 0, "", fmt.Errorf("response chain header: maj=%d err=%v", maj, err)
	}
	if tn > MaxRequestLength {
		return nil, 0, "", fmt.Errorf("response chain too long: %d", tn)
	}
	chain = make([][]*ltypes.BlockHeader, 0, tn)
	for i := uint64(0); i < tn; i++ {
		maj, fn, err := cr.ReadHeader()
		if err != nil || maj != cbg.MajArray || fn != 2 {
			return nil, 0, "", fmt.Errorf("bstipset %d: want array(2), got maj=%d n=%d err=%v", i, maj, fn, err)
		}
		maj, bn, err := cr.ReadHeader()
		if err != nil || maj != cbg.MajArray {
			return nil, 0, "", fmt.Errorf("bstipset %d blocks header: maj=%d err=%v", i, maj, err)
		}
		if bn == 0 || bn > maxBlocksPerTipset {
			return nil, 0, "", fmt.Errorf("bstipset %d: bad block count %d", i, bn)
		}
		blocks := make([]*ltypes.BlockHeader, 0, bn)
		for j := uint64(0); j < bn; j++ {
			bh := new(ltypes.BlockHeader)
			if err := bh.UnmarshalCBOR(cr); err != nil {
				return nil, 0, "", fmt.Errorf("bstipset %d block %d: %w", i, j, err)
			}
			blocks = append(blocks, bh)
		}
		// Messages: null for headers-only; tolerate anything and skip.
		var d cbg.Deferred
		if err := d.UnmarshalCBOR(cr); err != nil {
			return nil, 0, "", fmt.Errorf("bstipset %d messages skip: %w", i, err)
		}
		chain = append(chain, blocks)
	}
	return chain, status, errMsg, nil
}
