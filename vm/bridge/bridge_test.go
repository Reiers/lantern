package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
)

// mockBridge counts calls + returns a deterministic root/receipt.
type mockBridge struct {
	calls int
	root  cid.Cid
}

func (m *mockBridge) Provenance() string { return "mock" }
func (m *mockBridge) ComputeStateRoot(_ context.Context, _ cid.Cid, _ int64, msgs []*types.Message) (cid.Cid, []*types.MessageReceipt, error) {
	m.calls++
	recs := make([]*types.MessageReceipt, len(msgs))
	for i := range msgs {
		recs[i] = &types.MessageReceipt{ExitCode: exitcode.Ok, GasUsed: 12345 + int64(i)}
	}
	return m.root, recs, nil
}

func mustParseCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	c, err := cid.Parse(s)
	if err != nil {
		t.Fatalf("parse cid %s: %v", s, err)
	}
	return c
}

func sampleMessage(t *testing.T, nonce uint64) *types.Message {
	t.Helper()
	from, _ := address.NewIDAddress(99)
	to, _ := address.NewIDAddress(100)
	return &types.Message{
		Version:    0,
		To:         to,
		From:       from,
		Nonce:      nonce,
		Value:      big.Zero(),
		Method:     0,
		GasLimit:   1_000_000,
		GasFeeCap:  big.NewInt(100),
		GasPremium: big.NewInt(10),
	}
}

func TestCachingBridge_HappyPath(t *testing.T) {
	root := mustParseCID(t, "bafy2bzaceaflvwa5ocjbhmyjgenr2xfvyvvwhxnmis4mb2ero2fh7zfbqhmd6")
	m := &mockBridge{root: root}
	cb := NewCachingBridge(m, 8)

	base := mustParseCID(t, "bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")
	msg := sampleMessage(t, 7)

	r1, recs1, err := cb.ComputeStateRoot(context.Background(), base, 6_000_000, []*types.Message{msg})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if r1 != root {
		t.Fatalf("root mismatch: got %s want %s", r1, root)
	}
	if len(recs1) != 1 || recs1[0].GasUsed != 12345 {
		t.Fatalf("unexpected receipts: %+v", recs1)
	}
	if m.calls != 1 {
		t.Fatalf("calls=%d want 1", m.calls)
	}

	// Second call with the same inputs must hit the cache.
	r2, recs2, err := cb.ComputeStateRoot(context.Background(), base, 6_000_000, []*types.Message{msg})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if r2 != r1 {
		t.Fatalf("root mismatch on cache hit")
	}
	if recs2[0].GasUsed != recs1[0].GasUsed {
		t.Fatalf("receipts mismatch on cache hit")
	}
	if m.calls != 1 {
		t.Fatalf("expected cache hit, mockBridge.calls=%d", m.calls)
	}
}

func TestCachingBridge_DifferentEpochsMiss(t *testing.T) {
	m := &mockBridge{root: mustParseCID(t, "bafy2bzaceaflvwa5ocjbhmyjgenr2xfvyvvwhxnmis4mb2ero2fh7zfbqhmd6")}
	cb := NewCachingBridge(m, 8)
	base := mustParseCID(t, "bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")
	msg := sampleMessage(t, 1)

	_, _, _ = cb.ComputeStateRoot(context.Background(), base, 100, []*types.Message{msg})
	_, _, _ = cb.ComputeStateRoot(context.Background(), base, 101, []*types.Message{msg})
	if m.calls != 2 {
		t.Fatalf("expected 2 distinct calls for different epochs, got %d", m.calls)
	}
}

func TestCachingBridge_Eviction(t *testing.T) {
	m := &mockBridge{root: mustParseCID(t, "bafy2bzaceaflvwa5ocjbhmyjgenr2xfvyvvwhxnmis4mb2ero2fh7zfbqhmd6")}
	cb := NewCachingBridge(m, 2)
	base := mustParseCID(t, "bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")

	for i := 0; i < 4; i++ {
		_, _, _ = cb.ComputeStateRoot(context.Background(), base, int64(i), []*types.Message{sampleMessage(t, uint64(i))})
	}
	if m.calls != 4 {
		t.Fatalf("expected 4 cold calls, got %d", m.calls)
	}
	if len(cb.cache) > 2 {
		t.Fatalf("cache exceeded max: %d > 2", len(cb.cache))
	}
}

// errBridge: always returns an error.
type errBridge struct{}

func (errBridge) Provenance() string { return "err" }
func (errBridge) ComputeStateRoot(_ context.Context, _ cid.Cid, _ int64, _ []*types.Message) (cid.Cid, []*types.MessageReceipt, error) {
	return cid.Undef, nil, errors.New("upstream unreachable")
}

func TestCachingBridge_ErrorNotCached(t *testing.T) {
	cb := NewCachingBridge(errBridge{}, 8)
	base := mustParseCID(t, "bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")
	_, _, err := cb.ComputeStateRoot(context.Background(), base, 1, []*types.Message{sampleMessage(t, 0)})
	if err == nil {
		t.Fatal("expected error")
	}
	// Second call should error too, never cached.
	_, _, err = cb.ComputeStateRoot(context.Background(), base, 1, []*types.Message{sampleMessage(t, 0)})
	if err == nil {
		t.Fatal("expected error again")
	}
	if len(cb.cache) != 0 {
		t.Fatalf("error was cached: %d entries", len(cb.cache))
	}
}
