// Package msgsearch finds a message CID in the recent chain and returns the
// resulting MsgLookup (receipt + tipset of inclusion).
//
// A Filecoin block carries a `Messages` CID that points to a `MsgMeta`
// struct: { BlsMessages: AMT[cid.Cid], SecpkMessages: AMT[cid.Cid] }. The
// receipts for the tipset's execution live in the NEXT tipset's
// `ParentMessageReceipts` AMT, indexed in the application order
// (BLS messages first, then secp).
//
// Search walks backward from the requested tipset until either:
//  - the message CID appears in any block's BLS or secp AMT, or
//  - the lookback budget is exhausted.
//
// On a hit, we look up the receipt at the matched index from the
// child-tipset's ParentMessageReceipts.

package msgsearch

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/amt"
	"github.com/Reiers/lantern/state/hamt"
)

// ErrNotFound is returned when the message wasn't found within lookback.
var ErrNotFound = errors.New("msgsearch: message not found within lookback")

// Result is a found-message tuple.
type Result struct {
	TipSet  *ltypes.TipSet
	Height  abi.ChainEpoch
	Receipt ltypes.MessageReceipt
	// MessageIndex is the index in the tipset's combined message order
	// (BLS messages in block order then secp). Useful for diagnostics.
	MessageIndex uint64
}

// Searcher walks the chain backward looking for a message CID.
type Searcher struct {
	Store *hstore.Store
	Block hamt.BlockGetter
}

// New returns a Searcher.
func New(s *hstore.Store, bg hamt.BlockGetter) *Searcher {
	return &Searcher{Store: s, Block: bg}
}

// Find walks back at most `lookback` epochs from `fromEpoch` looking for
// `msgCID`. fromEpoch=-1 means "current head".
func (s *Searcher) Find(ctx context.Context, fromEpoch abi.ChainEpoch, msgCID cid.Cid, lookback abi.ChainEpoch) (*Result, error) {
	if s.Store == nil {
		return nil, errors.New("msgsearch: header store not configured")
	}
	if !msgCID.Defined() {
		return nil, errors.New("msgsearch: undefined message CID")
	}
	head := s.Store.HeadEpoch()
	if fromEpoch < 0 || fromEpoch > head {
		fromEpoch = head
	}
	if lookback <= 0 {
		lookback = 30
	}
	minEpoch := fromEpoch - lookback
	if minEpoch < 0 {
		minEpoch = 0
	}

	// We need the child tipset (epoch+1 canonical) to fetch
	// ParentMessageReceipts for the matched tipset.
	for ep := fromEpoch; ep >= minEpoch; ep-- {
		ts, err := s.Store.Tipset(ep)
		if err != nil || ts == nil {
			if ep == 0 {
				break
			}
			continue
		}
		idx, ok, err := s.findInTipset(ctx, ts, msgCID)
		if err != nil {
			return nil, err
		}
		if !ok {
			if ep == 0 {
				break
			}
			continue
		}
		// Found at index `idx` of ts. Receipt lives in child tipset's
		// ParentMessageReceipts. The child is the canonical tipset at
		// ep+1 (its blocks declare ts as parent).
		child, err := s.findChild(ts)
		if err != nil {
			// No child yet: message is included but not yet executed.
			// Lotus returns "not yet executed" in this state; we mirror
			// by returning ErrNotFound for now (StateWaitMsg will retry).
			return nil, ErrNotFound
		}
		receiptsRoot := child.Blocks()[0].ParentMessageReceipts
		// ParentMessageReceipts is a legacy (v2) AMT: 3-field root, implicit
		// width 8. Loading it with the v4 loader fails with "wrong number of
		// fields". Use the v2 path.
		recRaw, _, err := amt.LookupV2(ctx, receiptsRoot, idx, s.Block)
		if err != nil {
			return nil, fmt.Errorf("fetch receipt %d from %s: %w", idx, receiptsRoot, err)
		}
		var rec ltypes.MessageReceipt
		if err := rec.UnmarshalCBOR(bytes.NewReader(recRaw)); err != nil {
			return nil, fmt.Errorf("decode receipt: %w", err)
		}
		return &Result{TipSet: ts, Height: ts.Height(), Receipt: rec, MessageIndex: idx}, nil
	}
	return nil, ErrNotFound
}

// BlockMsgCIDs is one block's message CIDs as stored in its MsgMeta.
type BlockMsgCIDs struct {
	Bls  []cid.Cid
	Secp []cid.Cid
}

// BlockMessages returns the per-block (BLS, secp) message CID lists for
// every block of ts, in tipset block order.
func (s *Searcher) BlockMessages(ctx context.Context, ts *ltypes.TipSet) ([]BlockMsgCIDs, error) {
	out := make([]BlockMsgCIDs, len(ts.Blocks()))
	for bi, b := range ts.Blocks() {
		bls, secp, err := s.fetchMsgMeta(ctx, b.Messages)
		if err != nil {
			return nil, fmt.Errorf("block %s msgmeta: %w", b.Cid(), err)
		}
		out[bi] = BlockMsgCIDs{Bls: bls, Secp: secp}
	}
	return out, nil
}

// ExecutionOrder flattens per-block message lists into the order Lotus
// applies them (chain/consensus ApplyBlocks + ChainStore.MessagesForTipset):
// for each block in tipset order, that block's BLS messages followed by
// that block's secp messages. A CID already applied by an earlier block is
// skipped. isSecp[i] reports which list out[i] came from.
//
// Note: Lotus additionally drops a message whose (sender, nonce) does not
// follow the sender's previously applied nonce. That only matters when two
// blocks carry conflicting messages; callers that need exactness compare
// len(out) with the receipt count and apply the nonce rule on mismatch.
func ExecutionOrder(per []BlockMsgCIDs) (out []cid.Cid, isSecp []bool) {
	seen := make(map[cid.Cid]bool)
	for _, b := range per {
		for _, mc := range b.Bls {
			if seen[mc] {
				continue
			}
			seen[mc] = true
			out = append(out, mc)
			isSecp = append(isSecp, false)
		}
		for _, mc := range b.Secp {
			if seen[mc] {
				continue
			}
			seen[mc] = true
			out = append(out, mc)
			isSecp = append(isSecp, true)
		}
	}
	return out, isSecp
}

// OrderedMessageCIDs returns ts's messages in the canonical Filecoin
// application order (see ExecutionOrder). This is the order the child
// tipset's ParentMessageReceipts AMT is indexed by, so receipt[i]
// corresponds to OrderedMessageCIDs[i] (lantern#73, used by eth_getLogs to
// pair events with tx hashes).
func (s *Searcher) OrderedMessageCIDs(ctx context.Context, ts *ltypes.TipSet) ([]cid.Cid, error) {
	per, err := s.BlockMessages(ctx, ts)
	if err != nil {
		return nil, err
	}
	out, _ := ExecutionOrder(per)
	return out, nil
}

// FindChild exposes the canonical child-tipset lookup so callers that need
// a tipset's ParentMessageReceipts (which live in the child) can resolve
// it. Returns ErrNotFound-style error when no child is available yet.
func (s *Searcher) FindChild(ts *ltypes.TipSet) (*ltypes.TipSet, error) {
	return s.findChild(ts)
}

// findChild returns the canonical tipset at ts.Height()+1 if its parent
// matches ts.Key(). Returns an error otherwise.
func (s *Searcher) findChild(ts *ltypes.TipSet) (*ltypes.TipSet, error) {
	for ep := ts.Height() + 1; ep <= s.Store.HeadEpoch(); ep++ {
		next, err := s.Store.Tipset(ep)
		if err != nil || next == nil {
			continue
		}
		// Check parent linkage.
		if matchesParent(next, ts) {
			return next, nil
		}
		// If we hit a non-matching parent, the search-target was on a
		// fork — return error.
		return nil, fmt.Errorf("child at %d doesn't reference target tipset", ep)
	}
	return nil, errors.New("no child tipset available")
}

func matchesParent(child, parent *ltypes.TipSet) bool {
	if len(child.Blocks()) == 0 {
		return false
	}
	pcids := child.Blocks()[0].Parents
	tcids := parent.Cids()
	if len(pcids) != len(tcids) {
		return false
	}
	for i := range pcids {
		if pcids[i] != tcids[i] {
			return false
		}
	}
	return true
}

// findInTipset returns (index, true, nil) when msgCID appears in one of
// ts's blocks' message AMTs. The index is the message's position in the
// execution order (per block: BLS then secp; see ExecutionOrder) used by
// Filecoin's ParentMessageReceipts AMT.
func (s *Searcher) findInTipset(ctx context.Context, ts *ltypes.TipSet, msgCID cid.Cid) (uint64, bool, error) {
	per, err := s.BlockMessages(ctx, ts)
	if err != nil {
		return 0, false, err
	}
	ordered, _ := ExecutionOrder(per)
	for i, mc := range ordered {
		if mc == msgCID {
			return uint64(i), true, nil
		}
	}
	return 0, false, nil
}

// fetchMsgMeta fetches a block's MsgMeta CID, then enumerates the AMTs to
// return the ordered (bls, secp) message CIDs.
func (s *Searcher) fetchMsgMeta(ctx context.Context, metaCID cid.Cid) ([]cid.Cid, []cid.Cid, error) {
	raw, err := s.Block.Get(ctx, metaCID)
	if err != nil {
		return nil, nil, err
	}
	if err := hamt.VerifyBlockCID(metaCID, raw); err != nil {
		return nil, nil, err
	}
	var meta ltypes.MsgMeta
	if err := meta.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
		return nil, nil, fmt.Errorf("decode msgmeta: %w", err)
	}
	bls, err := s.enumerateAMTCIDs(ctx, meta.BlsMessages)
	if err != nil {
		return nil, nil, fmt.Errorf("bls amt: %w", err)
	}
	secp, err := s.enumerateAMTCIDs(ctx, meta.SecpkMessages)
	if err != nil {
		return nil, nil, fmt.Errorf("secp amt: %w", err)
	}
	return bls, secp, nil
}

// enumerateAMTCIDs walks a block's message AMT-of-CIDs in index order.
// Block message AMTs (BlsMessages / SecpkMessages) are legacy (v2) AMTs, so
// we enumerate via the v2 ForEach path.
func (s *Searcher) enumerateAMTCIDs(ctx context.Context, root cid.Cid) ([]cid.Cid, error) {
	return amt.ForEachV2CIDs(ctx, root, s.Block)
}
