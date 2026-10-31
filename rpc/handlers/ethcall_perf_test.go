package handlers

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	hamtipld "github.com/filecoin-project/go-hamt-ipld/v3"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	evm19 "github.com/filecoin-project/go-state-types/builtin/v19/evm"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/crypto/sha3"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/trustedroot"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/hamt"
)

// calibration actors v19 EVM code CID (pinned in state/actors registry tests).
var testEVMCode = cid.MustParse("bafk2bzaceceiey6pgg2qfkqxhqyrlffyylq4gyefkza6uirnowshcuyixoquo")

// gatedGetter counts inner Gets per CID and can hold every Get until
// release is closed (to force concurrent callers to overlap).
type gatedGetter struct {
	inner   hamt.BlockGetter
	mu      sync.Mutex
	per     map[cid.Cid]int
	total   atomic.Int64
	release chan struct{}
}

func newGatedGetter(inner hamt.BlockGetter) *gatedGetter {
	return &gatedGetter{inner: inner, per: map[cid.Cid]int{}}
}

func (g *gatedGetter) Get(ctx context.Context, c cid.Cid) ([]byte, error) {
	if g.release != nil {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	g.total.Add(1)
	g.mu.Lock()
	g.per[c]++
	g.mu.Unlock()
	return g.inner.Get(ctx, c)
}

func (g *gatedGetter) maxPerCID() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := 0
	for _, n := range g.per {
		if n > m {
			m = n
		}
	}
	return m
}

func putCBOR(t *testing.T, bs *hamt.MemBlockStore, raw []byte) cid.Cid {
	t.Helper()
	c, err := cid.Prefix{Version: 1, Codec: cid.DagCBOR, MhType: mh.BLAKE2B_MIN + 31, MhLength: 32}.Sum(raw)
	if err != nil {
		t.Fatal(err)
	}
	return bs.Put(c, raw)
}

// constReturn is EVM bytecode returning the 32-byte word v.
func constReturn(v byte) []byte {
	return []byte{0x60, v, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3}
}

// numberReturn returns NUMBER (the execution tipset's epoch).
var numberReturn = []byte{0x43, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3}

// revertEmpty is REVERT(0, 0).
var revertEmpty = []byte{0x60, 0x00, 0x60, 0x00, 0xfd}

// buildState writes a state tree holding one EVM contract per actor ID and
// returns the state root CID.
func buildState(t *testing.T, bs *hamt.MemBlockStore, contracts map[uint64][]byte) cid.Cid {
	t.Helper()
	ctx := context.Background()
	cst := hamt.CborStoreFromMem(bs)
	node, err := hamtipld.NewNode(cst, hamtipld.UseTreeBitWidth(hamt.FilBitWidth), hamtipld.UseHashFunction(hamtipld.HashFunc(hamt.FilHash)))
	if err != nil {
		t.Fatal(err)
	}
	emptyState := putCBOR(t, bs, []byte{0x80})
	for id, code := range contracts {
		bc, err := cid.Prefix{Version: 1, Codec: cid.Raw, MhType: mh.BLAKE2B_MIN + 31, MhLength: 32}.Sum(code)
		if err != nil {
			t.Fatal(err)
		}
		bs.Put(bc, code)
		h := sha3.NewLegacyKeccak256()
		h.Write(code)
		var bh [32]byte
		copy(bh[:], h.Sum(nil))
		st := evm19.State{Bytecode: bc, BytecodeHash: bh, ContractState: emptyState}
		var sb bytes.Buffer
		if err := st.MarshalCBOR(&sb); err != nil {
			t.Fatal(err)
		}
		head := putCBOR(t, bs, sb.Bytes())

		var ab bytes.Buffer
		w := cbg.NewCborWriter(&ab)
		_ = w.WriteMajorTypeHeader(cbg.MajArray, 4)
		_ = cbg.WriteCid(w, testEVMCode)
		_ = cbg.WriteCid(w, head)
		_ = w.WriteMajorTypeHeader(cbg.MajUnsignedInt, 0)
		bal := big.Zero()
		if err := bal.MarshalCBOR(w); err != nil {
			t.Fatal(err)
		}
		ida, _ := address.NewIDAddress(id)
		if err := node.Set(ctx, string(ida.Bytes()), &cbg.Deferred{Raw: ab.Bytes()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := node.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	actorsRoot, err := cst.Put(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	var rb bytes.Buffer
	w := cbg.NewCborWriter(&rb)
	_ = w.WriteMajorTypeHeader(cbg.MajArray, 3)
	_ = w.WriteMajorTypeHeader(cbg.MajUnsignedInt, 5)
	_ = cbg.WriteCid(w, actorsRoot)
	_ = cbg.WriteCid(w, emptyState)
	return putCBOR(t, bs, rb.Bytes())
}

func testBlockHeader(t *testing.T, h abi.ChainEpoch, parents []cid.Cid, stateRoot cid.Cid, tag string) *ltypes.BlockHeader {
	t.Helper()
	miner, _ := address.NewIDAddress(1000)
	mk := func(s string) cid.Cid {
		c, _ := cid.Prefix{Version: 1, Codec: cid.DagCBOR, MhType: mh.SHA2_256, MhLength: -1}.Sum([]byte(s))
		return c
	}
	return &ltypes.BlockHeader{
		Miner:                 miner,
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t-" + tag)},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + tag)},
		Parents:               parents,
		ParentWeight:          ltypes.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       stateRoot,
		ParentMessageReceipts: mk("receipts-" + tag),
		Messages:              mk("msgs-" + tag),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         ltypes.NewInt(100),
	}
}

func mustTipSet(t *testing.T, b *ltypes.BlockHeader) *ltypes.TipSet {
	t.Helper()
	ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{b})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// ethCallFixture is a header store + state trees for the live-head
// eth_call path, with the contract under test at actor ID 1234.
type ethCallFixture struct {
	c    *ChainAPI
	hs   *hstore.Store
	bs   *hamt.MemBlockStore
	cg   *gatedGetter
	tip  *ltypes.TipSet
	to   string
	numb string
}

const testContractID = 1234
const testNumberID = 1235
const testRevertID = 1236

func maskedID(id uint64) string {
	return "0xff" + strings.Repeat("0", 22) + hex.EncodeToString([]byte{0, 0, 0, 0, byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)})[0:16]
}

func newEthCallFixture(t *testing.T, ret byte) *ethCallFixture {
	t.Helper()
	bs := hamt.NewMemBlockStore()
	sr := buildState(t, bs, map[uint64][]byte{testContractID: constReturn(ret), testNumberID: numberReturn, testRevertID: revertEmpty})
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	gen := mustTipSet(t, testBlockHeader(t, 10, nil, sr, "g"))
	head := mustTipSet(t, testBlockHeader(t, 11, gen.Cids(), sr, "h11"))
	ctx := context.Background()
	if err := hs.SetHead(ctx, gen); err != nil {
		t.Fatal(err)
	}
	if err := hs.SetHead(ctx, head); err != nil {
		t.Fatal(err)
	}
	cg := newGatedGetter(bs)
	tr := &trustedroot.TrustedRoot{Epoch: 10, StateRoot: sr}
	c := &ChainAPI{
		Trusted:     tr,
		BlockGetter: cg,
		Accessor:    accessor.New(tr, cg),
		NetworkName: "calibrationnet",
		HeaderStore: hs,
	}
	return &ethCallFixture{c: c, hs: hs, bs: bs, cg: cg, tip: head, to: maskedID(testContractID), numb: maskedID(testNumberID)}
}

// advance adopts a new head whose parent state holds the contract
// returning ret.
func (f *ethCallFixture) advance(t *testing.T, ret byte) {
	t.Helper()
	sr := buildState(t, f.bs, map[uint64][]byte{testContractID: constReturn(ret), testNumberID: numberReturn, testRevertID: revertEmpty})
	h := f.tip.Height() + 1
	next := mustTipSet(t, testBlockHeader(t, h, f.tip.Cids(), sr, "h"+string(rune('a'+int(h%26)))))
	if err := f.hs.SetHead(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	f.tip = next
}

func word(v uint64) string {
	var b [32]byte
	for i := 0; i < 8; i++ {
		b[31-i] = byte(v >> (8 * i))
	}
	return "0x" + hex.EncodeToString(b[:])
}

func (f *ethCallFixture) call(t *testing.T, to string) string {
	t.Helper()
	got, err := f.c.EthCall(context.Background(), map[string]any{"to": to, "data": "0x"}, "latest")
	if err != nil {
		t.Fatalf("EthCall %s: %v", to, err)
	}
	return got
}

func TestMaskedIDHelper(t *testing.T) {
	if got, want := maskedID(testContractID), "0xff000000000000000000000000000000000004d2"; got != want {
		t.Fatalf("maskedID = %s, want %s", got, want)
	}
}

// TestLocalEthCall_ResultCacheInvalidatesOnNewHead: identical calls on the
// same head are served from the result cache; a new head (new state root
// and new execution tipset) is never served the previous head's result.
func TestLocalEthCall_ResultCacheInvalidatesOnNewHead(t *testing.T) {
	f := newEthCallFixture(t, 1)

	if got := f.call(t, f.to); got != word(1) {
		t.Fatalf("head 11: got %s, want %s", got, word(1))
	}
	if got := f.call(t, f.numb); got != word(10) {
		t.Fatalf("head 11 NUMBER: got %s, want %s (execution tipset = head parent)", got, word(10))
	}
	fetches := f.cg.total.Load()
	if got := f.call(t, f.to); got != word(1) {
		t.Fatalf("head 11 repeat: got %s", got)
	}
	if n := f.cg.total.Load(); n != fetches {
		t.Fatalf("repeat call on same head fetched %d blocks, want 0 (result cache)", n-fetches)
	}
	if st := f.c.EthCallCacheStats(); st.ResultHits != 1 {
		t.Fatalf("result hits = %d, want 1", st.ResultHits)
	}

	// New head: the contract now returns 2, NUMBER moves to 11.
	f.advance(t, 2)
	if got := f.call(t, f.to); got != word(2) {
		t.Fatalf("head 12: got %s, want %s (stale result served across heads)", got, word(2))
	}
	if got := f.call(t, f.numb); got != word(11) {
		t.Fatalf("head 12 NUMBER: got %s, want %s", got, word(11))
	}

	// Same state root, different execution tipset: env-dependent results
	// must not be reused either.
	f.advance(t, 2)
	if got := f.call(t, f.numb); got != word(12) {
		t.Fatalf("head 13 NUMBER: got %s, want %s", got, word(12))
	}
	if gens := f.c.ethPerf().results.generations(); len(gens) != perfResultGens {
		t.Fatalf("result cache holds %d generations, want %d", len(gens), perfResultGens)
	}
}

// TestLocalEthCall_RevertIsCachedAsRevert: a definitive revert is cached
// and replayed as the same revert error, not as a success.
func TestLocalEthCall_RevertIsCachedAsRevert(t *testing.T) {
	f := newEthCallFixture(t, 1)
	for i := 0; i < 2; i++ {
		_, err := f.c.EthCall(context.Background(), map[string]any{"to": maskedID(testRevertID), "data": "0x"}, "latest")
		var re *revertError
		if !errors.As(err, &re) {
			t.Fatalf("call %d: want revertError, got %v", i, err)
		}
	}
	if st := f.c.EthCallCacheStats(); st.ResultHits != 1 {
		t.Fatalf("result hits = %d, want 1", st.ResultHits)
	}
}

// TestLocalEthCall_ConcurrentBurstSharesWork: a Curio-style burst of
// identical and distinct calls on a fresh head fetches every block once.
func TestLocalEthCall_ConcurrentBurstSharesWork(t *testing.T) {
	f := newEthCallFixture(t, 7)
	f.cg.release = make(chan struct{})

	const n = 48
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			to, want := f.to, word(7)
			if i%2 == 1 {
				to, want = f.numb, word(10)
			}
			got, err := f.c.EthCall(context.Background(), map[string]any{"to": to, "data": "0x"}, "latest")
			if err != nil {
				errs <- err
				return
			}
			if got != want {
				errs <- errors.New("got " + got + " want " + want)
			}
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let every caller pile up on the gate
	close(f.cg.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if m := f.cg.maxPerCID(); m != 1 {
		t.Fatalf("a block was fetched %d times during the burst, want 1", m)
	}
	st := f.c.EthCallCacheStats()
	if st.Executions != 2 {
		t.Fatalf("interpreter ran %d times for 2 distinct calls (shared=%d hits=%d)", st.Executions, st.CallsShared, st.ResultHits)
	}
}

// --- unit tests for the building blocks -----------------------------------

func TestEthResultCache_Generations(t *testing.T) {
	r := newEthResultCache(2, 2)
	ok := ethCallOutcome{served: true, ret: "0x01"}
	r.put("A", "k", ok)
	if o, hit := r.get("A", "k"); !hit || o.ret != "0x01" {
		t.Fatal("same generation should hit")
	}
	if _, hit := r.get("B", "k"); hit {
		t.Fatal("different generation must miss")
	}
	r.put("A", "fault", ethCallOutcome{served: false, reason: "x"})
	if _, hit := r.get("A", "fault"); hit {
		t.Fatal("non-served outcome must not be cached")
	}
	r.put("B", "k", ethCallOutcome{served: true, ret: "0x02"})
	r.put("C", "k", ethCallOutcome{served: true, ret: "0x03"})
	if _, hit := r.get("A", "k"); hit {
		t.Fatal("oldest generation should be evicted")
	}
	if o, hit := r.get("C", "k"); !hit || o.ret != "0x03" {
		t.Fatal("newest generation should hit")
	}
	r.put("C", "k2", ok)
	r.put("C", "k3", ok) // over the per-generation cap: dropped
	if _, hit := r.get("C", "k3"); hit {
		t.Fatal("per-generation cap not enforced")
	}
}

func TestSharedBlockGetter_SingleflightAndCache(t *testing.T) {
	bs := hamt.NewMemBlockStore()
	c := putCBOR(t, bs, []byte{0x82, 0x01, 0x02})
	cg := newGatedGetter(bs)
	cg.release = make(chan struct{})
	s := newSharedBlockGetter(cg, 16)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := s.Get(context.Background(), c)
			if err != nil || !bytes.Equal(raw, []byte{0x82, 0x01, 0x02}) {
				t.Errorf("Get: %v %x", err, raw)
			}
		}()
	}
	time.Sleep(30 * time.Millisecond)
	close(cg.release)
	wg.Wait()
	if n := cg.total.Load(); n != 1 {
		t.Fatalf("inner fetched %d times, want 1", n)
	}
	if _, err := s.Get(context.Background(), c); err != nil || cg.total.Load() != 1 {
		t.Fatalf("cached Get went to inner (err=%v, fetches=%d)", err, cg.total.Load())
	}
}

type badBytesGetter struct{ n atomic.Int64 }

func (b *badBytesGetter) Get(context.Context, cid.Cid) ([]byte, error) {
	b.n.Add(1)
	return []byte("not the block"), nil
}

func TestSharedBlockGetter_RejectsAndDoesNotCacheBadBytes(t *testing.T) {
	bs := hamt.NewMemBlockStore()
	c := putCBOR(t, bs, []byte{0x80})
	bad := &badBytesGetter{}
	s := newSharedBlockGetter(bad, 16)
	for i := 0; i < 2; i++ {
		if _, err := s.Get(context.Background(), c); err == nil {
			t.Fatal("bytes not matching the CID must be rejected")
		}
	}
	if bad.n.Load() != 2 {
		t.Fatalf("bad bytes were cached (inner calls = %d)", bad.n.Load())
	}
}

// TestSharedBlockGetter_CallerCancelDoesNotFailOthers: the first caller
// giving up (Curio's client timeout) must not fail the callers sharing its
// fetch, and the fetch still lands in the cache.
func TestSharedBlockGetter_CallerCancelDoesNotFailOthers(t *testing.T) {
	bs := hamt.NewMemBlockStore()
	c := putCBOR(t, bs, []byte{0x81, 0x01})
	cg := newGatedGetter(bs)
	cg.release = make(chan struct{})
	s := newSharedBlockGetter(cg, 16)

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := s.Get(leaderCtx, c)
		leaderErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	followerErr := make(chan error, 1)
	go func() {
		_, err := s.Get(context.Background(), c)
		followerErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: want context.Canceled, got %v", err)
	}
	close(cg.release)
	if err := <-followerErr; err != nil {
		t.Fatalf("follower failed because the leader gave up: %v", err)
	}
	if _, ok := s.cache.Get(c); !ok {
		t.Fatal("shared fetch did not populate the cache")
	}
}

// f4Contract is an EVM contract with a delegated (f410) address.
type f4Contract struct {
	id        uint64
	code      []byte
	delegated *address.Address // nil: no delegated address
}

// buildStateF4 writes a state tree with an Init actor whose AddressMap
// maps each contract's delegated address to its ID, plus the contracts.
// nextID only makes the Init state (and so its block CID) unique per call.
func buildStateF4(t *testing.T, bs *hamt.MemBlockStore, nextID uint64, contracts []f4Contract) (stateRoot, initHead cid.Cid) {
	t.Helper()
	ctx := context.Background()
	cst := hamt.CborStoreFromMem(bs)
	opts := []hamtipld.Option{hamtipld.UseTreeBitWidth(hamt.FilBitWidth), hamtipld.UseHashFunction(hamtipld.HashFunc(hamt.FilHash))}
	tree, err := hamtipld.NewNode(cst, opts...)
	if err != nil {
		t.Fatal(err)
	}
	amap, err := hamtipld.NewNode(cst, opts...)
	if err != nil {
		t.Fatal(err)
	}
	emptyState := putCBOR(t, bs, []byte{0x80})
	putActor := func(id uint64, code, head cid.Cid, delegated *address.Address) {
		var ab bytes.Buffer
		w := cbg.NewCborWriter(&ab)
		n := uint64(4)
		if delegated != nil {
			n = 5
		}
		_ = w.WriteMajorTypeHeader(cbg.MajArray, n)
		_ = cbg.WriteCid(w, code)
		_ = cbg.WriteCid(w, head)
		_ = w.WriteMajorTypeHeader(cbg.MajUnsignedInt, 0)
		bal := big.Zero()
		if err := bal.MarshalCBOR(w); err != nil {
			t.Fatal(err)
		}
		if delegated != nil {
			if err := delegated.MarshalCBOR(w); err != nil {
				t.Fatal(err)
			}
		}
		ida, _ := address.NewIDAddress(id)
		if err := tree.Set(ctx, string(ida.Bytes()), &cbg.Deferred{Raw: ab.Bytes()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range contracts {
		bc, err := cid.Prefix{Version: 1, Codec: cid.Raw, MhType: mh.BLAKE2B_MIN + 31, MhLength: 32}.Sum(c.code)
		if err != nil {
			t.Fatal(err)
		}
		bs.Put(bc, c.code)
		h := sha3.NewLegacyKeccak256()
		h.Write(c.code)
		var bh [32]byte
		copy(bh[:], h.Sum(nil))
		st := evm19.State{Bytecode: bc, BytecodeHash: bh, ContractState: emptyState}
		var sb bytes.Buffer
		if err := st.MarshalCBOR(&sb); err != nil {
			t.Fatal(err)
		}
		putActor(c.id, testEVMCode, putCBOR(t, bs, sb.Bytes()), c.delegated)
		if c.delegated != nil {
			v := cbg.CborInt(c.id)
			if err := amap.Set(ctx, string(c.delegated.Bytes()), &v); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := amap.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	amapRoot, err := cst.Put(ctx, amap)
	if err != nil {
		t.Fatal(err)
	}
	var ib bytes.Buffer
	w := cbg.NewCborWriter(&ib)
	_ = w.WriteMajorTypeHeader(cbg.MajArray, 3)
	_ = cbg.WriteCid(w, amapRoot)
	_ = w.WriteMajorTypeHeader(cbg.MajUnsignedInt, nextID)
	_ = cbg.WriteMajorTypeHeader(w, cbg.MajTextString, 4)
	_, _ = w.Write([]byte("test"))
	initHead = putCBOR(t, bs, ib.Bytes())
	putActor(1, testEVMCode, initHead, nil)

	if err := tree.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	actorsRoot, err := cst.Put(ctx, tree)
	if err != nil {
		t.Fatal(err)
	}
	var rb bytes.Buffer
	w = cbg.NewCborWriter(&rb)
	_ = w.WriteMajorTypeHeader(cbg.MajArray, 3)
	_ = w.WriteMajorTypeHeader(cbg.MajUnsignedInt, 5)
	_ = cbg.WriteCid(w, actorsRoot)
	_ = cbg.WriteCid(w, emptyState)
	return putCBOR(t, bs, rb.Bytes()), initHead
}

// TestLocalEthCall_F4HintVerifiedAgainstState: the cached f410 -> ID hint
// skips the Init actor walk on later state roots, and is rejected (full
// lookup) when the actor at the hinted ID no longer carries the address.
func TestLocalEthCall_F4HintVerifiedAgainstState(t *testing.T) {
	payload := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}
	x, err := address.NewDelegatedAddress(10, payload)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := address.NewDelegatedAddress(10, append([]byte{0x99}, payload[1:]...))
	to := "0x" + hex.EncodeToString(payload)

	f := newEthCallFixture(t, 1)
	ctx := context.Background()
	adopt := func(sr cid.Cid) {
		h := f.tip.Height() + 1
		next := mustTipSet(t, testBlockHeader(t, h, f.tip.Cids(), sr, "f4-"+string(rune('a'+int(h%26)))))
		if err := f.hs.SetHead(ctx, next); err != nil {
			t.Fatal(err)
		}
		f.tip = next
	}

	// S1: x -> 2000 (returns 1). Full lookup, hint recorded.
	s1, _ := buildStateF4(t, f.bs, 3001, []f4Contract{{id: 2000, code: constReturn(1), delegated: &x}})
	adopt(s1)
	if got := f.call(t, to); got != word(1) {
		t.Fatalf("S1: got %s want %s", got, word(1))
	}

	// S2: same mapping, new root. The hint must be used: the Init
	// actor's state block for S2 is never fetched.
	s2, init2 := buildStateF4(t, f.bs, 3002, []f4Contract{{id: 2000, code: constReturn(2), delegated: &x}})
	adopt(s2)
	if got := f.call(t, to); got != word(2) {
		t.Fatalf("S2: got %s want %s", got, word(2))
	}
	f.cg.mu.Lock()
	n := f.cg.per[init2]
	f.cg.mu.Unlock()
	if n != 0 {
		t.Fatalf("S2: Init actor state fetched %d times; the verified f410 hint should skip it", n)
	}

	// S3: x now maps to 2001; actor 2000 carries a different delegated
	// address. The hint must be rejected and the full lookup used.
	s3, _ := buildStateF4(t, f.bs, 3003, []f4Contract{
		{id: 2000, code: constReturn(9), delegated: &other},
		{id: 2001, code: constReturn(3), delegated: &x},
	})
	adopt(s3)
	if got := f.call(t, to); got != word(3) {
		t.Fatalf("S3: got %s want %s (stale f410 hint served the wrong actor)", got, word(3))
	}
}

// TestHeadWarmer_WarmsRecordedActorsOnNewHead: actors read by live-head
// eth_calls are re-resolved at the next head's state root as soon as the
// head is adopted, and the warmed entry is exactly what a call then uses.
func TestHeadWarmer_WarmsRecordedActorsOnNewHead(t *testing.T) {
	f := newEthCallFixture(t, 1)
	if got := f.call(t, f.to); got != word(1) {
		t.Fatalf("head 11: got %s", got)
	}
	f.advance(t, 2)
	deadline := time.Now().Add(5 * time.Second)
	for f.c.EthCallCacheStats().WarmPasses == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no warm pass after head change")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ida, _ := address.NewIDAddress(testContractID)
	p := f.c.ethPerf()
	if _, ok := p.actors.Get(actorCacheKey{root: f.tip.ParentState(), addr: string(ida.Bytes())}); !ok {
		t.Fatal("recorded actor was not warmed at the new state root")
	}
	before := f.cg.total.Load()
	if got := f.call(t, f.to); got != word(2) {
		t.Fatalf("head 12 after warm: got %s want %s", got, word(2))
	}
	if n := f.cg.total.Load() - before; n != 0 {
		t.Fatalf("call after warm still fetched %d blocks", n)
	}
}
