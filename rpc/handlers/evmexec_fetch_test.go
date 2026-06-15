// Unit tests for the eth_call retry-on-miss BlockGetter wrapper
// (lantern#44).

package handlers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/hamt"
)

// flakyGetter returns an error for the first N calls, then succeeds.
type flakyGetter struct {
	failFirst int
	calls     atomic.Int64
	payload   []byte
	errMsg    string
}

func (f *flakyGetter) Get(_ context.Context, _ cid.Cid) ([]byte, error) {
	n := f.calls.Add(1)
	if n <= int64(f.failFirst) {
		return nil, errors.New(f.errMsg)
	}
	return append([]byte(nil), f.payload...), nil
}

// alwaysErrorGetter never succeeds.
type alwaysErrorGetter struct {
	calls atomic.Int64
}

func (a *alwaysErrorGetter) Get(_ context.Context, _ cid.Cid) ([]byte, error) {
	a.calls.Add(1)
	return nil, errors.New("permanent miss")
}

func testCID(t *testing.T) cid.Cid {
	t.Helper()
	// Any deterministic CID is fine for the wrapper itself; the wrapper
	// is CID-blind. Use the empty-block dag-cbor CID.
	c, err := cid.Decode("bafyreiczsscdsbs7ffqz55asqdf3smv6klcw3gofszvwlyarci47bgf354")
	if err != nil {
		t.Fatalf("decode test CID: %v", err)
	}
	return c
}

// Sanity: ensure the wrapper still implements the interface.
var _ hamt.BlockGetter = (*retryingBlockGetter)(nil)

func TestRetryingBlockGetter_FastPath_NoRetries(t *testing.T) {
	g := &alwaysErrorGetter{}
	r := newRetryingBlockGetter(g, 0, 0) // disabled
	_, err := r.Get(context.Background(), testCID(t))
	if err == nil {
		t.Fatal("expected error")
	}
	if got := g.calls.Load(); got != 1 {
		t.Fatalf("disabled-retry should fire exactly one inner call, got %d", got)
	}
}

func TestRetryingBlockGetter_SucceedsOnFirst(t *testing.T) {
	g := &flakyGetter{failFirst: 0, payload: []byte("ok"), errMsg: "n/a"}
	r := newRetryingBlockGetter(g, 2, time.Second)
	raw, err := r.Get(context.Background(), testCID(t))
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if string(raw) != "ok" {
		t.Fatalf("payload mismatch: %q", raw)
	}
	s := r.stats()
	if s.Attempts != 1 || s.Retried != 0 || s.Succeeds != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestRetryingBlockGetter_SucceedsOnRetry(t *testing.T) {
	g := &flakyGetter{failFirst: 1, payload: []byte("late"), errMsg: "cold"}
	r := newRetryingBlockGetter(g, 2, 2*time.Second)
	raw, err := r.Get(context.Background(), testCID(t))
	if err != nil {
		t.Fatalf("expected success after retry: %v", err)
	}
	if string(raw) != "late" {
		t.Fatalf("payload mismatch: %q", raw)
	}
	s := r.stats()
	if s.Attempts != 2 || s.Retried != 1 || s.Succeeds != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestRetryingBlockGetter_ExhaustsAttempts(t *testing.T) {
	g := &alwaysErrorGetter{}
	r := newRetryingBlockGetter(g, 2, 2*time.Second)
	_, err := r.Get(context.Background(), testCID(t))
	if err == nil {
		t.Fatal("expected error after exhaustion")
	}
	if got := g.calls.Load(); got != 3 {
		t.Fatalf("expected 3 inner calls (1 + 2 retries), got %d", got)
	}
	s := r.stats()
	if s.Fails != 1 {
		t.Fatalf("expected 1 final-fail, got %+v", s)
	}
}

func TestRetryingBlockGetter_RespectsTotalBudget(t *testing.T) {
	// Inner blocks 200ms, total budget 250ms => max 1 full attempt
	// before budget exhausts.
	g := &slowFailGetter{delay: 200 * time.Millisecond}
	r := newRetryingBlockGetter(g, 5, 250*time.Millisecond)
	start := time.Now()
	_, err := r.Get(context.Background(), testCID(t))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("total budget should bound elapsed time; got %v", elapsed)
	}
}

func TestRetryingBlockGetter_NilInner(t *testing.T) {
	r := newRetryingBlockGetter(nil, 2, time.Second)
	if _, err := r.Get(context.Background(), testCID(t)); err == nil {
		t.Fatal("expected error from nil inner")
	}
}

// slowFailGetter always errors but takes `delay` per call. Used to
// exercise the total-budget bound.
type slowFailGetter struct {
	delay time.Duration
}

func (s *slowFailGetter) Get(ctx context.Context, _ cid.Cid) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
		return nil, errors.New("slow fail")
	}
}

