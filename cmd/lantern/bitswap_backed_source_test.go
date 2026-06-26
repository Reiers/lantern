package main

import (
	"context"
	"errors"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
)

// --- fakes ---

type fakeGetter struct {
	raw  []byte
	err  error
	hits int
}

func (f *fakeGetter) Get(_ context.Context, _ cid.Cid) ([]byte, error) {
	f.hits++
	return f.raw, f.err
}

type fakeGlif struct {
	bh         *types.BlockHeader
	err        error
	fetchHits  int
	headCalls  int
	tipsetCall int
}

func (g *fakeGlif) HeadEpoch(context.Context) (abi.ChainEpoch, error) {
	g.headCalls++
	return 100, nil
}
func (g *fakeGlif) TipsetCIDsByHeight(context.Context, abi.ChainEpoch) ([]cid.Cid, error) {
	g.tipsetCall++
	return nil, nil
}
func (g *fakeGlif) FetchBlock(context.Context, cid.Cid) (*types.BlockHeader, error) {
	g.fetchHits++
	return g.bh, g.err
}

func mustAddr(t *testing.T) address.Address {
	t.Helper()
	a, err := address.NewIDAddress(1000)
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	return a
}

func mustCID(t *testing.T) cid.Cid {
	t.Helper()
	c, err := abi.CidBuilder.Sum([]byte("sample-state"))
	if err != nil {
		t.Fatalf("cid: %v", err)
	}
	return c
}

// minimal valid block we can serialize/decode round-trip.
func sampleBlock(t *testing.T) (*types.BlockHeader, []byte, cid.Cid) {
	t.Helper()
	bh := &types.BlockHeader{
		Miner:                 mustAddr(t),
		ParentWeight:          types.NewInt(100),
		Height:                42,
		ParentStateRoot:       mustCID(t),
		ParentMessageReceipts: mustCID(t),
		Messages:              mustCID(t),
		Timestamp:             1234,
		ParentBaseFee:         types.NewInt(100),
		ForkSignaling:         0,
	}
	raw, err := bh.Serialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	// round-trip sanity
	if _, err := types.DecodeBlock(raw); err != nil {
		t.Fatalf("decode round-trip: %v", err)
	}
	return bh, raw, bh.Cid()
}

// --- tests ---

// fetcher hit: served from bitswap/gateway, Glif never touched.
func TestBitswapBacked_FetcherHit(t *testing.T) {
	_, raw, c := sampleBlock(t)
	fg := &fakeGetter{raw: raw}
	gl := &fakeGlif{}
	s := newBitswapBackedSource(gl, func() blockGetter { return fg })

	bh, err := s.FetchBlock(context.Background(), c)
	if err != nil {
		t.Fatalf("FetchBlock: %v", err)
	}
	if bh == nil || bh.Height != 42 {
		t.Fatalf("wrong block: %+v", bh)
	}
	if fg.hits != 1 {
		t.Errorf("fetcher hits = %d, want 1", fg.hits)
	}
	if gl.fetchHits != 0 {
		t.Errorf("glif should NOT be called on a fetcher hit, got %d", gl.fetchHits)
	}
}

// fetcher miss (error) -> falls back to Glif.
func TestBitswapBacked_FetcherMissFallsBackToGlif(t *testing.T) {
	bh, _, c := sampleBlock(t)
	fg := &fakeGetter{err: errors.New("bitswap miss")}
	gl := &fakeGlif{bh: bh}
	s := newBitswapBackedSource(gl, func() blockGetter { return fg })

	got, err := s.FetchBlock(context.Background(), c)
	if err != nil {
		t.Fatalf("FetchBlock: %v", err)
	}
	if got == nil || got.Height != 42 {
		t.Fatalf("wrong block from glif fallback: %+v", got)
	}
	if gl.fetchHits != 1 {
		t.Errorf("glif fetch hits = %d, want 1 (fallback)", gl.fetchHits)
	}
}

// fetcher returns undecodable bytes -> falls back to Glif rather than failing.
func TestBitswapBacked_DecodeFailFallsBackToGlif(t *testing.T) {
	bh, _, c := sampleBlock(t)
	fg := &fakeGetter{raw: []byte("not-cbor")}
	gl := &fakeGlif{bh: bh}
	s := newBitswapBackedSource(gl, func() blockGetter { return fg })

	got, err := s.FetchBlock(context.Background(), c)
	if err != nil {
		t.Fatalf("FetchBlock: %v", err)
	}
	if got == nil || got.Height != 42 {
		t.Fatalf("expected glif fallback block, got %+v", got)
	}
	if gl.fetchHits != 1 {
		t.Errorf("glif fetch hits = %d, want 1", gl.fetchHits)
	}
}

// nil fetcher (getter returns nil) -> straight to Glif, no panic.
func TestBitswapBacked_NilFetcher(t *testing.T) {
	bh, _, c := sampleBlock(t)
	gl := &fakeGlif{bh: bh}
	s := newBitswapBackedSource(gl, func() blockGetter { return nil })
	got, err := s.FetchBlock(context.Background(), c)
	if err != nil || got == nil {
		t.Fatalf("nil-fetcher path: got=%v err=%v", got, err)
	}
	if gl.fetchHits != 1 {
		t.Errorf("glif fetch hits = %d, want 1", gl.fetchHits)
	}
}

// HeadEpoch / TipsetCIDsByHeight delegate to Glif.
func TestBitswapBacked_RPCMethodsDelegate(t *testing.T) {
	gl := &fakeGlif{}
	s := newBitswapBackedSource(gl, func() blockGetter { return nil })
	if _, err := s.HeadEpoch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TipsetCIDsByHeight(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if gl.headCalls != 1 || gl.tipsetCall != 1 {
		t.Errorf("delegation miss: head=%d tipset=%d", gl.headCalls, gl.tipsetCall)
	}
}
