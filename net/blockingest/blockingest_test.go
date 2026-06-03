// Unit tests for the gossipsub block ingestor.
//
// We cover the behaviours that don't require a live gossipsub topic:
//   - dedupe by header CID across repeat enqueue
//   - height fence: blocks at or behind current head are dropped
//   - parent fence: blocks whose parents aren't in the store are skipped
//   - happy path: block at head+1 with known parents gets installed
//
// The live gossipsub join + read loop is exercised in net/blockpub's own
// tests and on the live mainnet daemon; this file is the pure-logic
// coverage of the ingestor itself.

package blockingest

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
)

func mkCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

func mkAddr(t *testing.T, id uint64) address.Address {
	t.Helper()
	a, err := address.NewIDAddress(id)
	require.NoError(t, err)
	return a
}

// mkBlock builds a deterministic test block at the given height and
// parents. Tag distinguishes competing blocks at the same height.
func mkBlock(t *testing.T, h abi.ChainEpoch, parents []cid.Cid, miner uint64, tag string) *ltypes.BlockHeader {
	t.Helper()
	return &ltypes.BlockHeader{
		Miner:                 mkAddr(t, miner),
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t-" + tag)},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + tag)},
		BeaconEntries:         nil,
		Parents:               parents,
		ParentWeight:          ltypes.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       mkCID(t, "state-"+tag),
		ParentMessageReceipts: mkCID(t, "receipts-"+tag),
		Messages:              mkCID(t, "msgs-"+tag),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         ltypes.NewInt(100),
	}
}

// withStore opens a temp Badger-backed header store and seeds it with a
// chain of blocks rooted at the given genesis tag. Returns the store and
// the in-store tipset at headHeight.
func withStore(t *testing.T, headHeight abi.ChainEpoch) (*hstore.Store, *ltypes.TipSet) {
	t.Helper()
	dir := t.TempDir()
	s, err := hstore.Open(filepath.Join(dir, "hs"), hstore.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Build a single-block chain genesis -> headHeight.
	var prevCids []cid.Cid
	var prev *ltypes.BlockHeader
	for h := abi.ChainEpoch(0); h <= headHeight; h++ {
		b := mkBlock(t, h, prevCids, 1000, "main")
		ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{b})
		require.NoError(t, err)
		require.NoError(t, s.SetHead(context.Background(), ts))
		prevCids = []cid.Cid{b.Cid()}
		prev = b
	}
	require.NotNil(t, prev)
	headTS, err := ltypes.NewTipSet([]*ltypes.BlockHeader{prev})
	require.NoError(t, err)
	return s, headTS
}

func TestIngestor_DedupesRepeatEnqueue(t *testing.T) {
	s, head := withStore(t, 10)
	ing := New(s, nil)

	// Block at head+1 with the right parent.
	next := mkBlock(t, head.Height()+1, []cid.Cid{head.Blocks()[0].Cid()}, 1000, "next")
	blk := &ltypes.BlockMsg{Header: next}

	ctx := context.Background()

	// First process: installed.
	ing.process(ctx, blk)
	require.Equal(t, uint64(1), ing.installed.Load(), "first process should install")

	// Second process: deduped.
	ing.process(ctx, blk)
	require.Equal(t, uint64(1), ing.installed.Load(), "second process must not double-install")
	require.Equal(t, uint64(1), ing.dedup.Load(), "second process must hit dedupe")
}

func TestIngestor_HeightFence(t *testing.T) {
	s, head := withStore(t, 10)
	ing := New(s, nil)
	ctx := context.Background()

	// Block at the same height as head. Should be skipped silently
	// (no install, no rejection - height fence is a quiet drop).
	stale := mkBlock(t, head.Height(), nil, 9999, "stale")
	ing.process(ctx, &ltypes.BlockMsg{Header: stale})

	// Block below head.
	older := mkBlock(t, head.Height()-3, nil, 9999, "older")
	ing.process(ctx, &ltypes.BlockMsg{Header: older})

	require.Equal(t, uint64(0), ing.installed.Load())
	require.Equal(t, uint64(0), ing.rejected.Load())
}

func TestIngestor_SkipsWhenParentsMissing(t *testing.T) {
	s, head := withStore(t, 10)
	ing := New(s, nil)
	ctx := context.Background()

	// Block at head+5 (jump ahead). Its parent CID won't be in the store.
	unknownParent := mkCID(t, "phantom-parent")
	jump := mkBlock(t, head.Height()+5, []cid.Cid{unknownParent}, 1000, "jump")
	ing.process(ctx, &ltypes.BlockMsg{Header: jump})

	require.Equal(t, uint64(0), ing.installed.Load(), "should not install when parents missing")
	require.Equal(t, uint64(1), ing.skipped.Load(), "should record as skipped")
}

func TestIngestor_InstallsAtHeadPlusOne(t *testing.T) {
	s, head := withStore(t, 10)
	ing := New(s, nil)
	ctx := context.Background()

	next := mkBlock(t, head.Height()+1, []cid.Cid{head.Blocks()[0].Cid()}, 1000, "next")
	ing.process(ctx, &ltypes.BlockMsg{Header: next})

	require.Equal(t, uint64(1), ing.installed.Load())
	require.Equal(t, head.Height()+1, s.HeadEpoch(),
		"head should have advanced to new block's height")
	require.Equal(t, head.Height()+1, ing.Stats().LastInstallEpoch)
}

func TestIngestor_EnqueueDropsWhenChannelFull(t *testing.T) {
	s, _ := withStore(t, 1)
	ing := New(s, nil)

	// Don't start Run(); the channel buffer fills and Enqueue drops.
	// Send buffer+5 messages; the last 5 should be dropped.
	buf := cap(ing.incoming)
	for i := 0; i < buf+5; i++ {
		b := mkBlock(t, abi.ChainEpoch(100+i), nil, 1000, "drop")
		ing.Enqueue(&ltypes.BlockMsg{Header: b})
	}

	// Give the runtime a moment to settle.
	time.Sleep(10 * time.Millisecond)

	require.Equal(t, uint64(buf+5), ing.received.Load(), "all received")
	require.GreaterOrEqual(t, ing.skipped.Load(), uint64(5), "at least 5 dropped due to full channel")
}

func TestIngestor_EnqueueIgnoresNilHeader(t *testing.T) {
	s, _ := withStore(t, 1)
	ing := New(s, nil)

	ing.Enqueue(nil)
	ing.Enqueue(&ltypes.BlockMsg{Header: nil})

	require.Equal(t, uint64(0), ing.received.Load(), "nil messages must not be counted")
}

// fakeBackfillSource is an in-memory RPC source for testing inline backfill.
// It indexes blocks by height + CID; TipsetCIDsByHeight returns whatever was
// registered at that height.
type fakeBackfillSource struct {
	byHeight map[abi.ChainEpoch][]*ltypes.BlockHeader
	byCID    map[cid.Cid]*ltypes.BlockHeader
	calls    int
}

func newFakeBackfillSource() *fakeBackfillSource {
	return &fakeBackfillSource{
		byHeight: map[abi.ChainEpoch][]*ltypes.BlockHeader{},
		byCID:    map[cid.Cid]*ltypes.BlockHeader{},
	}
}

func (f *fakeBackfillSource) register(b *ltypes.BlockHeader) {
	f.byHeight[b.Height] = append(f.byHeight[b.Height], b)
	f.byCID[b.Cid()] = b
}

func (f *fakeBackfillSource) TipsetCIDsByHeight(_ context.Context, h abi.ChainEpoch) ([]cid.Cid, error) {
	f.calls++
	bs := f.byHeight[h]
	cids := make([]cid.Cid, 0, len(bs))
	for _, b := range bs {
		cids = append(cids, b.Cid())
	}
	return cids, nil
}

func (f *fakeBackfillSource) FetchBlock(_ context.Context, k cid.Cid) (*ltypes.BlockHeader, error) {
	b, ok := f.byCID[k]
	if !ok {
		return nil, errFakeSourceNoBlock
	}
	return b, nil
}

var errFakeSourceNoBlock = fmt.Errorf("fake source: block not registered")

func TestIngestor_InlineBackfillFillsGap(t *testing.T) {
	s, head := withStore(t, 10)
	src := newFakeBackfillSource()

	// Build a gap of 2 epochs at h=11, h=12, then a new head at h=13.
	parentCID := head.Blocks()[0].Cid()
	ep11 := mkBlock(t, 11, []cid.Cid{parentCID}, 1000, "e11")
	ep12 := mkBlock(t, 12, []cid.Cid{ep11.Cid()}, 1000, "e12")
	ep13 := mkBlock(t, 13, []cid.Cid{ep12.Cid()}, 1000, "e13")
	src.register(ep11)
	src.register(ep12)
	// ep13 is the gossipsub-arrived block; not registered as we don't fetch it

	ing := New(s, src)
	ing.process(context.Background(), &ltypes.BlockMsg{Header: ep13})

	require.Equal(t, uint64(1), ing.backfilled.Load(), "backfill should fire")
	require.Equal(t, uint64(1), ing.installed.Load(), "head block should install after backfill")
	require.Equal(t, abi.ChainEpoch(13), s.HeadEpoch(), "head should advance to 13")
	require.GreaterOrEqual(t, src.calls, 2, "backfill should fetch 2 epochs")
}

func TestIngestor_InlineBackfillRespectsCapsToSkip(t *testing.T) {
	s, _ := withStore(t, 10)
	src := newFakeBackfillSource()

	// Big gap with unknown parent: head at 10, new block at 20 pointing at
	// a parent block at 19 that's NOT in the store. Default cap is 3, so
	// the backfill walk from epoch 11 to 19 (9 epochs) exceeds the cap.
	unknownParent := mkCID(t, "unknown-parent-at-19")
	far := mkBlock(t, 20, []cid.Cid{unknownParent}, 1000, "far")

	ing := New(s, src)
	ing.process(context.Background(), &ltypes.BlockMsg{Header: far})

	require.Equal(t, uint64(0), ing.installed.Load(), "too-big gap must not install")
	require.Equal(t, uint64(1), ing.backfillFailed.Load(), "backfill should fail with gap > cap")
	require.Equal(t, uint64(1), ing.skipped.Load(), "and the head block itself is skipped")
}
