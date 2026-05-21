package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// mockSource is a controllable in-process Source for testing.
type mockSource struct {
	name string
	kind Kind
	fin  Finality
	err  error
	wait time.Duration
}

func (m *mockSource) Name() string { return m.name }
func (m *mockSource) Kind() Kind   { return m.kind }
func (m *mockSource) LatestFinality(ctx context.Context) (Finality, error) {
	if m.wait > 0 {
		select {
		case <-time.After(m.wait):
		case <-ctx.Done():
			return Finality{}, ctx.Err()
		}
	}
	if m.err != nil {
		return Finality{}, m.err
	}
	return m.fin, nil
}

func mustCid(t *testing.T, s string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.Raw, h)
}

func fin(t *testing.T, inst uint64, tag, state string) Finality {
	t.Helper()
	return Finality{
		Instance:  inst,
		TipSetKey: []cid.Cid{mustCid(t, tag)},
		StateRoot: mustCid(t, state),
		Epoch:     int64(inst) * 30,
	}
}

func TestQuorum_AllAgree(t *testing.T) {
	f := fin(t, 100, "head", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindLibp2p, fin: f},
		&mockSource{name: "b", kind: KindLibp2p, fin: f},
		&mockSource{name: "c", kind: KindForest, fin: f},
		&mockSource{name: "d", kind: KindUser, fin: f},
		&mockSource{name: "e", kind: KindLibp2p, fin: f},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 5, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("expected quorum success, got %v\n%s", err, FormatReport(r))
	}
	if !r.Reached {
		t.Fatalf("expected Reached=true, got %s", FormatReport(r))
	}
	if len(r.Agreeing) != 5 {
		t.Fatalf("expected 5 agreeing, got %d", len(r.Agreeing))
	}
	if r.Winning.Instance != 100 {
		t.Fatalf("expected instance 100, got %d", r.Winning.Instance)
	}
}

func TestQuorum_FourOfFiveFails(t *testing.T) {
	good := fin(t, 100, "head", "state")
	bad := fin(t, 100, "other", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindLibp2p, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindForest, fin: good},
		&mockSource{name: "d", kind: KindUser, fin: good},
		&mockSource{name: "e", kind: KindLibp2p, fin: bad}, // disagrees
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 5, Timeout: 5 * time.Second})
	if err == nil {
		t.Fatalf("expected quorum failure (4/5 agreement), got success\n%s", FormatReport(r))
	}
	if !errors.Is(err, ErrQuorumNotReached) {
		t.Fatalf("expected ErrQuorumNotReached, got %v", err)
	}
	// Best bucket should be 4, not 5.
	if len(r.Buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(r.Buckets))
	}
	if len(r.Buckets[0].Sources) != 4 {
		t.Fatalf("expected best bucket = 4, got %d", len(r.Buckets[0].Sources))
	}
}

func TestQuorum_Divergence_3plus2Fails(t *testing.T) {
	a := fin(t, 100, "head-A", "state-A")
	b := fin(t, 100, "head-B", "state-B")
	srcs := []Source{
		&mockSource{name: "a1", kind: KindLibp2p, fin: a},
		&mockSource{name: "a2", kind: KindLibp2p, fin: a},
		&mockSource{name: "a3", kind: KindForest, fin: a},
		&mockSource{name: "b1", kind: KindLibp2p, fin: b},
		&mockSource{name: "b2", kind: KindUser, fin: b},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 5, Timeout: 5 * time.Second})
	if err == nil {
		t.Fatalf("expected quorum failure (split 3+2), got success\n%s", FormatReport(r))
	}
	if len(r.Buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(r.Buckets))
	}
}

func TestQuorum_LowerThreshold_3OfFiveSucceeds(t *testing.T) {
	good := fin(t, 100, "head", "state")
	bad := fin(t, 100, "other", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindLibp2p, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindForest, fin: good},
		&mockSource{name: "d", kind: KindUser, fin: bad},
		&mockSource{name: "e", kind: KindLibp2p, fin: bad},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("expected 3-quorum success, got %v\n%s", err, FormatReport(r))
	}
	if !r.Reached {
		t.Fatal("expected Reached=true")
	}
}

func TestQuorum_TimeoutPerSource(t *testing.T) {
	good := fin(t, 100, "head", "state")
	srcs := []Source{
		&mockSource{name: "fast1", kind: KindLibp2p, fin: good},
		&mockSource{name: "fast2", kind: KindLibp2p, fin: good},
		&mockSource{name: "slow1", kind: KindLibp2p, fin: good, wait: 2 * time.Second},
		&mockSource{name: "slow2", kind: KindForest, fin: good, wait: 2 * time.Second},
		&mockSource{name: "slow3", kind: KindUser, fin: good, wait: 2 * time.Second},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 5, Timeout: 200 * time.Millisecond})
	if err == nil {
		t.Fatalf("expected quorum failure on tight timeout, got success\n%s", FormatReport(r))
	}
	if !errors.Is(err, ErrQuorumNotReached) {
		t.Fatalf("expected ErrQuorumNotReached, got %v", err)
	}
	// Slow sources should have failed with context.DeadlineExceeded.
	slowFailed := 0
	for _, s := range r.Results {
		if strings.HasPrefix(s.Name, "slow") && s.Error != nil {
			slowFailed++
		}
	}
	if slowFailed != 3 {
		t.Fatalf("expected 3 slow sources to fail, got %d", slowFailed)
	}
}

func TestQuorum_GatewayNotCountedByDefault(t *testing.T) {
	good := fin(t, 100, "head", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindLibp2p, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindForest, fin: good},
		&mockSource{name: "d", kind: KindUser, fin: good},
		&mockSource{name: "gw", kind: KindLanternGateway, fin: good},
	}
	// 5 sources total, but gateway shouldn't count → only 4 countable.
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 5, Timeout: 5 * time.Second})
	if err == nil {
		t.Fatalf("expected ErrInsufficientSources, got success\n%s", FormatReport(r))
	}
	if !errors.Is(err, ErrInsufficientSources) {
		t.Fatalf("expected ErrInsufficientSources, got %v", err)
	}
}

func TestQuorum_GatewayCountedWhenOptIn(t *testing.T) {
	good := fin(t, 100, "head", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindLibp2p, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindForest, fin: good},
		&mockSource{name: "d", kind: KindUser, fin: good},
		&mockSource{name: "gw", kind: KindLanternGateway, fin: good},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 5, Timeout: 5 * time.Second, CountGateway: true})
	if err != nil {
		t.Fatalf("expected quorum success with CountGateway, got %v\n%s", err, FormatReport(r))
	}
	if !r.Reached {
		t.Fatal("expected Reached=true")
	}
}

func TestQuorum_ProgressCallback(t *testing.T) {
	good := fin(t, 100, "head", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindLibp2p, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindForest, fin: good},
		&mockSource{name: "d", kind: KindUser, fin: good},
		&mockSource{name: "e", kind: KindLibp2p, fin: good},
	}
	var got []string
	r, err := Quorum(context.Background(), srcs, QuorumOptions{
		Quorum:  5,
		Timeout: 5 * time.Second,
		Progress: func(s SourceResult) {
			got = append(got, s.Name)
		},
	})
	if err != nil || !r.Reached {
		t.Fatalf("unexpected: %v\n%s", err, FormatReport(r))
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 progress callbacks, got %d", len(got))
	}
}

func TestQuorum_AllFail(t *testing.T) {
	srcs := []Source{
		&mockSource{name: "a", kind: KindLibp2p, err: fmt.Errorf("a unreachable")},
		&mockSource{name: "b", kind: KindLibp2p, err: fmt.Errorf("b unreachable")},
		&mockSource{name: "c", kind: KindForest, err: fmt.Errorf("c unreachable")},
		&mockSource{name: "d", kind: KindUser, err: fmt.Errorf("d unreachable")},
		&mockSource{name: "e", kind: KindLibp2p, err: fmt.Errorf("e unreachable")},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 5, Timeout: 1 * time.Second})
	if err == nil {
		t.Fatal("expected failure when all sources error")
	}
	if !errors.Is(err, ErrQuorumNotReached) {
		t.Fatalf("expected ErrQuorumNotReached, got %v", err)
	}
	if r.Counted != 0 {
		t.Fatalf("expected 0 counted, got %d", r.Counted)
	}
}
