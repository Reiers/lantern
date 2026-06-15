// Unit tests for the FEVM contract state prefetcher (lantern#44).
//
// These tests exercise the wiring (config defaults, address parsing,
// coalescing, inflight gating, Trigger never blocks) without standing
// up a real chain head + state tree. The real "does it warm the cache"
// proof is the cc-smoke soak (see docs/issues/0044-...).
package prefetch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	proof "github.com/filecoin-project/go-state-types/proof"
	"github.com/ipfs/go-cid"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// stubBG: a no-op BlockGetter; every Get fails. The prefetcher must not
// crash on this — failed walks just count errors.
type stubBG struct{}

func (stubBG) Get(_ context.Context, _ cid.Cid) ([]byte, error) {
	return nil, errors.New("no block")
}

func TestPrefetcher_DefaultsApplied(t *testing.T) {
	p := New(Config{Addrs: []string{"0x" + "ab"+"00"+"cd"+"00"+"ef"+"00"+"11"+"00"+"22"+"00"+"33"+"00"+"44"+"00"+"55"+"00"+"66"+"00"+"77"+"00"}}, stubBG{})
	s := p.Stats()
	if s.MaxBlocksPerAddr != 256 {
		t.Fatalf("expected default MaxBlocksPerAddr=256, got %d", s.MaxBlocksPerAddr)
	}
	if s.ConfiguredAddrs != 1 {
		t.Fatalf("expected 1 configured addr, got %d", s.ConfiguredAddrs)
	}
}

func TestPrefetcher_ParseEthAddr(t *testing.T) {
	for _, c := range []struct {
		in string
		ok bool
	}{
		{"0xabcdef0102030405060708090a0b0c0d0e0f1011", true},
		{"abcdef0102030405060708090a0b0c0d0e0f1011", true},
		{"0xABCDEF0102030405060708090a0b0c0d0e0f1011", true},
		{"0x1234", false}, // too short
		{"0xnotanaddr00000000000000000000000000000000", false},
		{"", false},
	} {
		_, ok := parseEthAddr(c.in)
		if ok != c.ok {
			t.Errorf("parseEthAddr(%q) ok=%v, want %v", c.in, ok, c.ok)
		}
	}
}

func TestPrefetcher_TriggerOnNilHead_NoCrash(t *testing.T) {
	p := New(Config{Addrs: []string{"0xabcdef0102030405060708090a0b0c0d0e0f1011"}}, stubBG{})
	p.Trigger(context.Background(), nil)
	if p.Stats().Runs != 0 {
		t.Fatalf("nil head must not count a run")
	}
}

func TestPrefetcher_TriggerWithoutAddrs_NoOp(t *testing.T) {
	p := New(Config{}, stubBG{})
	ts := newTestTipSet(t, 100)
	p.Trigger(context.Background(), ts)
	if p.Stats().Runs != 0 {
		t.Fatalf("empty addr list must not count a run")
	}
}

func TestPrefetcher_TriggerRunsAddrOnce_WithCooldown(t *testing.T) {
	p := New(Config{
		Addrs:            []string{"0xabcdef0102030405060708090a0b0c0d0e0f1011"},
		MaxBlocksPerAddr: 4,
		PerAddrTimeout:   500 * time.Millisecond,
		MinInterval:      time.Hour, // very long cooldown
	}, stubBG{})
	ts := newTestTipSet(t, 100)
	p.Trigger(context.Background(), ts)
	// give the spawned goroutine a chance to complete its walk (stub
	// fails fast).
	time.Sleep(50 * time.Millisecond)

	// Second trigger same epoch should be coalesced.
	p.Trigger(context.Background(), ts)
	time.Sleep(50 * time.Millisecond)

	s := p.Stats()
	if s.Runs != 2 {
		t.Fatalf("expected Runs=2 (Trigger calls), got %d", s.Runs)
	}
	if s.SkippedCooldown == 0 {
		t.Fatalf("second trigger should be cooldown-skipped, stats=%+v", s)
	}
}

func TestPrefetcher_StringRendering(t *testing.T) {
	p := New(Config{Addrs: []string{"0xabcdef0102030405060708090a0b0c0d0e0f1011"}}, stubBG{})
	if got := p.Stats().String(); got == "" {
		t.Fatal("Stats.String() empty")
	}
}

// newTestTipSet returns the minimal usable *ltypes.TipSet for prefetcher
// tests: a single fully-formed block header with a defined parent-state
// CID at the requested height. The accessor + GetActor calls inside
// walkOne fail against the stub BlockGetter — that's fine; we're only
// exercising the Trigger path here.
func newTestTipSet(t *testing.T, height int64) *ltypes.TipSet {
	t.Helper()
	c, err := cid.Decode("bafy2bzaceaflvwa5ocjbhmyjgenr2xfvyvvwhxnmis4mb2ero2fh7zfbqhmd6")
	if err != nil {
		t.Fatalf("decode test CID: %v", err)
	}
	miner, err := address.NewIDAddress(1000)
	if err != nil {
		t.Fatalf("miner addr: %v", err)
	}
	bh := &ltypes.BlockHeader{
		Miner:                 miner,
		Ticket:                &ltypes.Ticket{VRFProof: []byte("test")},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1},
		BeaconEntries:         []ltypes.BeaconEntry{},
		WinPoStProof:          []proof.PoStProof{},
		Parents:               nil,
		ParentWeight:          big.NewInt(1),
		Height:                abi.ChainEpoch(height),
		ParentStateRoot:       c,
		ParentMessageReceipts: c,
		Messages:              c,
		BLSAggregate:          &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
		Timestamp:             uint64(time.Now().Unix()),
		BlockSig:              &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
	}
	ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{bh})
	if err != nil {
		t.Fatalf("NewTipSet: %v", err)
	}
	return ts
}

