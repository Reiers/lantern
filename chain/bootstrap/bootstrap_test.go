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
	op   string // operator key; "" falls back to a per-source key
	fin  Finality
	err  error
	wait time.Duration
}

func (m *mockSource) Name() string     { return m.name }
func (m *mockSource) Operator() string { return m.op }
func (m *mockSource) Kind() Kind       { return m.kind }
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
	// Same instance, different tipset: a conflict, not just a short count.
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
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

// F10: a 3-of-5 majority no longer wins over a same-instance minority.
// Any conflicting responder fails the probe loudly.
func TestQuorum_LowerThreshold_3OfFiveWithConflictFails(t *testing.T) {
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
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v\n%s", err, FormatReport(r))
	}
	if r.Reached || len(r.Conflicts) == 0 {
		t.Fatalf("expected Reached=false with conflicts recorded\n%s", FormatReport(r))
	}
}

func TestQuorum_LowerThreshold_3OfFiveWithFailuresSucceeds(t *testing.T) {
	good := fin(t, 100, "head", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindLibp2p, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindForest, fin: good},
		&mockSource{name: "d", kind: KindUser, err: fmt.Errorf("dial timeout")},
		&mockSource{name: "e", kind: KindLibp2p, err: fmt.Errorf("connection refused")},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("expected 3-quorum success, got %v\n%s", err, FormatReport(r))
	}
	if !r.Reached || len(r.FailedSources()) != 2 {
		t.Fatalf("expected Reached=true with 2 failed sources\n%s", FormatReport(r))
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

// ---------- F10: operator-aware quorum ----------

// Several endpoints of one operator count once. This is the shape of the
// mainnet default set: the chain.love RPC, the Glif RPC (served by
// chain.love) and the chain.love libp2p peer are ONE vote.
func TestQuorum_SameOperatorCountsOnce(t *testing.T) {
	f := fin(t, 466453, "head", "state")
	srcs := []Source{
		&mockSource{name: "rpc:glif", kind: KindForest, op: "chain.love", fin: f},
		&mockSource{name: "rpc:chain.love", kind: KindForest, op: "chain.love", fin: f},
		&mockSource{name: "libp2p:chain.love", kind: KindLibp2p, op: "chain.love", fin: f},
		&mockSource{name: "libp2p:chainsafe-1", kind: KindLibp2p, op: "chainsafe-fil.io", fin: f},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second})
	if !errors.Is(err, ErrInsufficientSources) {
		t.Fatalf("4 endpoints from 2 operators must not satisfy a 3-operator quorum; got %v\n%s", err, FormatReport(r))
	}
	if r.CountableOperators != 2 {
		t.Fatalf("expected 2 countable operators, got %d", r.CountableOperators)
	}

	srcs = append(srcs, &mockSource{name: "rpc:filfox", kind: KindForest, op: "filfox.info", fin: f})
	r, err = Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second})
	if err != nil || !r.Reached {
		t.Fatalf("expected success with 3 distinct operators, got %v\n%s", err, FormatReport(r))
	}
	if len(r.AgreeingOperators) != 3 || len(r.Agreeing) != 5 {
		t.Fatalf("expected 5 agreeing sources from 3 operators, got %d sources / %v", len(r.Agreeing), r.AgreeingOperators)
	}
}

// Countable endpoints from many operators, but only two answer: the
// agreeing endpoints of one operator still do not reach 3.
func TestQuorum_RespondingOperatorsShort(t *testing.T) {
	f := fin(t, 7, "head", "state")
	srcs := []Source{
		&mockSource{name: "a1", kind: KindForest, op: "a.example", fin: f},
		&mockSource{name: "a2", kind: KindLibp2p, op: "a.example", fin: f},
		&mockSource{name: "b1", kind: KindLibp2p, op: "b.example", fin: f},
		&mockSource{name: "c1", kind: KindLibp2p, op: "c.example", err: fmt.Errorf("timeout")},
		&mockSource{name: "d1", kind: KindForest, op: "d.example", err: fmt.Errorf("HTTP 503")},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second})
	if !errors.Is(err, ErrQuorumNotReached) {
		t.Fatalf("expected ErrQuorumNotReached, got %v\n%s", err, FormatReport(r))
	}
	if got := len(r.Buckets[0].Operators); got != 2 {
		t.Fatalf("expected best bucket to have 2 operators, got %d", got)
	}
	if len(r.FailedSources()) != 2 {
		t.Fatalf("expected 2 failed sources, got %d", len(r.FailedSources()))
	}
}

// Two endpoints of the SAME operator disagreeing is still a conflict.
func TestQuorum_ConflictWithinOperatorFails(t *testing.T) {
	a := fin(t, 9, "head-A", "s")
	b := fin(t, 9, "head-B", "s")
	srcs := []Source{
		&mockSource{name: "x-rpc", kind: KindForest, op: "x", fin: a},
		&mockSource{name: "x-p2p", kind: KindLibp2p, op: "x", fin: b},
		&mockSource{name: "y", kind: KindForest, op: "y", fin: a},
		&mockSource{name: "z", kind: KindForest, op: "z", fin: a},
	}
	_, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

// A non-counted source (the gateway without --count-gateway) cannot vote,
// but a conflicting answer from it still fails the probe.
func TestQuorum_NonCountedConflictStillFails(t *testing.T) {
	good := fin(t, 50, "head", "state")
	bad := fin(t, 50, "evil", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindForest, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindLibp2p, fin: good},
		&mockSource{name: "gw", kind: KindLanternGateway, fin: bad},
	}
	_, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict from non-counted gateway, got %v", err)
	}
}

// An answer for a different instance is lag, not conflict.
func TestQuorum_LaggingSourceIsNotConflict(t *testing.T) {
	cur := fin(t, 100, "head-100", "s100")
	old := fin(t, 99, "head-99", "s99")
	srcs := []Source{
		&mockSource{name: "a", kind: KindForest, fin: cur},
		&mockSource{name: "b", kind: KindLibp2p, fin: cur},
		&mockSource{name: "c", kind: KindLibp2p, fin: cur},
		&mockSource{name: "lagging", kind: KindLibp2p, fin: old},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second})
	if err != nil || !r.Reached || r.Winning.Instance != 100 {
		t.Fatalf("expected success on instance 100 despite a lagging source, got %v\n%s", err, FormatReport(r))
	}
}

// After the threshold is met the driver keeps listening for Grace, so a
// slightly slower conflicting responder still fails the probe.
func TestQuorum_GraceCatchesLateConflict(t *testing.T) {
	good := fin(t, 100, "head", "state")
	bad := fin(t, 100, "evil", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindForest, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindLibp2p, fin: good},
		&mockSource{name: "late", kind: KindLibp2p, fin: bad, wait: 150 * time.Millisecond},
	}
	_, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second, Grace: time.Second})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected late conflict to fail the probe, got %v", err)
	}
	// With grace disabled the late answer is never seen (documented
	// tradeoff of Grace < 0).
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second, Grace: -1})
	if err != nil || !r.Reached {
		t.Fatalf("expected success with grace disabled, got %v", err)
	}
}

// Grace does not stretch the probe when every source has answered.
func TestQuorum_GraceEndsWhenAllAnswered(t *testing.T) {
	good := fin(t, 100, "head", "state")
	srcs := []Source{
		&mockSource{name: "a", kind: KindForest, fin: good},
		&mockSource{name: "b", kind: KindLibp2p, fin: good},
		&mockSource{name: "c", kind: KindLibp2p, fin: good},
	}
	t0 := time.Now()
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Quorum: 3, Timeout: 5 * time.Second, Grace: 10 * time.Second})
	if err != nil || !r.Reached {
		t.Fatalf("unexpected: %v", err)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("grace should end once all sources answered, took %s", time.Since(t0))
	}
}

func TestQuorum_DefaultIsThreeOperators(t *testing.T) {
	good := fin(t, 1, "h", "s")
	srcs := []Source{
		&mockSource{name: "a", kind: KindForest, fin: good},
		&mockSource{name: "b", kind: KindForest, fin: good},
	}
	r, err := Quorum(context.Background(), srcs, QuorumOptions{Timeout: time.Second})
	if !errors.Is(err, ErrInsufficientSources) || r.Required != DefaultQuorum {
		t.Fatalf("expected default quorum %d to reject 2 operators, got %v (required=%d)", DefaultQuorum, err, r.Required)
	}
}
