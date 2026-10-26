// Package bootstrap implements Lantern's multi-source trust-bootstrap
// quorum, as specified in INSTALLER-SPEC.md §3.
//
// The premise: no single operator determines Lantern's trust anchor. We
// query N sources in parallel, ask each "what is the current F3-finalized
// tipset?", and only accept the answer when sources run by at least
// quorum DISTINCT OPERATORS agree on the same (instance, tipsetKey,
// stateRoot) and no responder gave a different answer for the same
// instance (F10). Several endpoints run by one operator (for example the
// chain.love RPC and the chain.love libp2p bootstrap peer) are one vote.
//
// This is the trust foundation for V1.2 GA: the installer mathematically
// cannot be fooled by a single compromised gateway, RPC provider, or
// Lantern operator. The bootstrap quorum runs once at install time and
// produces the validated trust anchor that all subsequent verification
// builds on.
//
// Source implementations live in subpackages so they don't drag every
// dependency into the bootstrap package itself; the Quorum() driver here
// only depends on the Source interface.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
)

// Source is the narrow interface every bootstrap participant satisfies.
// Each source answers exactly one question: what is the latest F3
// finality result you can attest to?
type Source interface {
	// Name returns a short human-readable identifier for log/error output
	// (e.g. "libp2p-peer:12D3KooW...", "forest-archive", "user-peer:...").
	Name() string

	// Kind returns the source category for quorum policy decisions
	// (e.g. independent operators count, the project's own gateway does
	// not by default). See KindLibp2p / KindForest / KindLanternBeacon /
	// KindLanternGateway / KindUser.
	Kind() Kind

	// LatestFinality asks the source for its view of the most recent
	// F3-finalized result. Returns the GPBFT Instance, the tipset key
	// (block CIDs of the finalized tipset), and the state root after
	// applying that tipset.
	//
	// Implementations should respect ctx and return promptly on
	// cancellation. Network errors should be wrapped to be diagnosable.
	LatestFinality(ctx context.Context) (Finality, error)
}

// OperatorSource is implemented by sources that know which operator runs
// them (F10). The returned key is compared for equality only; use
// OperatorOf / OperatorOfMultiaddr to derive it so aliases are applied.
type OperatorSource interface {
	Operator() string
}

// OperatorKey returns the independence key the quorum counts a source
// under: its Operator() when implemented and non-empty, otherwise a key
// unique to the source name (an unattributable source is its own
// operator, which is no worse than the pre-F10 per-endpoint count).
func OperatorKey(s Source) string {
	if o, ok := s.(OperatorSource); ok {
		if op := strings.TrimSpace(o.Operator()); op != "" {
			return op
		}
	}
	return "source:" + s.Name()
}

// Kind groups sources for quorum-policy decisions.
type Kind string

const (
	// KindLibp2p is a libp2p mainnet bootstrap peer queried directly
	// over the F3 cert-exchange protocol.
	KindLibp2p Kind = "libp2p"
	// KindForest is a public Forest/Lotus JSON-RPC endpoint (e.g.
	// forest-archive.chainsafe.dev).
	KindForest Kind = "forest"
	// KindLanternBeacon is a DHT-discovered Lantern beacon.
	KindLanternBeacon Kind = "lantern-beacon"
	// KindLanternGateway is the Lantern project's gateway. NOT counted
	// in the quorum by default.
	KindLanternGateway Kind = "lantern-gateway"
	// KindUser is a user-supplied --peer endpoint.
	KindUser Kind = "user"
)

// Finality is one source's answer to "what is the latest F3-finalized
// tipset?". Two finalities agree iff they have the same Instance,
// TipSetKey (as a set, order-insensitive), and StateRoot.
type Finality struct {
	// Instance is the GPBFT instance that finalized this tipset. Lantern
	// requires it to be > 0 (the bootstrap manifest's initial instance
	// is 0 and we never accept that as a live-chain head).
	Instance uint64
	// TipSetKey is the block CIDs of the finalized tipset, in canonical
	// order (matches gpbft.TipSet.MarshalCBOR / Lotus tipset key form).
	TipSetKey []cid.Cid
	// StateRoot is the state root computed by applying the messages in
	// the finalized tipset. Light clients use this as the trust anchor
	// for state queries at and after the finalized epoch.
	StateRoot cid.Cid
	// Epoch is the chain epoch of the finalized tipset, if known.
	// Optional; used for human-readable display only. Quorum equality
	// does NOT depend on Epoch (because not every source can answer it
	// without extra round-trips).
	Epoch int64
}

// String renders the finality for log output.
func (f Finality) String() string {
	short := ""
	if len(f.TipSetKey) > 0 {
		short = shortCid(f.TipSetKey[0])
	}
	return fmt.Sprintf("instance=%d epoch=%d tsk=[%s,...×%d] state=%s",
		f.Instance, f.Epoch, short, len(f.TipSetKey), shortCid(f.StateRoot))
}

// Key returns the canonical equality key for this finality: the joined
// stringified instance + tipsetkey + stateroot. Used to bucket
// quorum-agreeing sources.
func (f Finality) Key() string {
	keys := make([]string, len(f.TipSetKey))
	for i, c := range f.TipSetKey {
		keys[i] = c.String()
	}
	sort.Strings(keys)
	return fmt.Sprintf("%d|%s|%s", f.Instance, strings.Join(keys, ","), f.StateRoot.String())
}

// shortCid renders a CID as the first 12 chars for human-readable logs.
func shortCid(c cid.Cid) string {
	s := c.String()
	if len(s) <= 16 {
		return s
	}
	return s[:10] + "..." + s[len(s)-3:]
}

// SourceResult records one source's response (or failure) during a
// quorum probe. Always populated even on error.
type SourceResult struct {
	Name     string
	Kind     Kind
	Operator string
	Finality Finality
	Error    error
	Duration time.Duration
	// Counted is true if this source contributes to the quorum tally
	// (i.e. it's not a non-counted KindLanternGateway when CountGateway
	// is false).
	Counted bool
}

// OK reports whether the source returned a finality without error.
func (r SourceResult) OK() bool { return r.Error == nil }

// QuorumResult is the full output of a quorum probe.
type QuorumResult struct {
	// Reached is true iff sources run by at least Required distinct
	// operators agreed on the same finality AND no responder returned a
	// conflicting finality for the same instance.
	Reached bool
	// Required is the quorum threshold that was asked for, in distinct
	// operators.
	Required int
	// Winning is the finality that reached quorum (zero-value if none).
	Winning Finality
	// Agreeing is the names of sources that agreed on Winning.
	Agreeing []string
	// AgreeingOperators is the distinct operators behind Agreeing.
	AgreeingOperators []string
	// Results is per-source detail (all sources, in completion order).
	Results []SourceResult
	// Counted is the number of sources whose answers were actually
	// tallied (i.e. counted=true and OK).
	Counted int
	// CountableOperators is the number of distinct operators among the
	// sources that could count toward the quorum.
	CountableOperators int
	// Buckets summarises the distinct finalities seen, most operators
	// first.
	Buckets []Bucket
	// Conflicts lists every pair of answers that named different
	// tipsets for the same F3 instance. Any conflict fails the probe.
	Conflicts []Conflict
	// Elapsed is the wall-clock time the probe took.
	Elapsed time.Duration
}

// Bucket is one distinct (instance, tsk, stateroot) tuple seen during a
// probe, plus the names and distinct operators of sources that returned
// it.
type Bucket struct {
	Finality  Finality
	Sources   []string
	Operators []string
}

// Conflict records two responders that disagree about the same F3
// instance. F3 finality is unique per instance (GPBFT safety), so two
// honest, correctly configured nodes can never produce this; it means a
// source is lying, broken, or on a different network.
type Conflict struct {
	Instance uint64
	A, B     Bucket
}

func (c Conflict) String() string {
	return fmt.Sprintf("instance %d: {%s} say %s, {%s} say %s",
		c.Instance, strings.Join(c.A.Sources, ", "), c.A.Finality,
		strings.Join(c.B.Sources, ", "), c.B.Finality)
}

// DefaultQuorum is the default number of distinct agreeing operators.
const DefaultQuorum = 3

// DefaultGrace is how long Quorum keeps listening after the threshold is
// met so a late conflicting answer still fails the probe.
const DefaultGrace = 3 * time.Second

// QuorumOptions configures a Quorum() run.
type QuorumOptions struct {
	// Quorum is the minimum number of DISTINCT OPERATORS (see
	// OperatorKey) whose sources must agree. Default: DefaultQuorum (3).
	// Quorum() returns ErrInsufficientSources up front if the countable
	// sources span fewer operators than this.
	Quorum int
	// Timeout is the total wall-clock budget. Default: 60s.
	Timeout time.Duration
	// Grace is how long to keep collecting answers after the threshold
	// is met, so a conflicting responder is still seen. Default
	// DefaultGrace; negative disables (stop as soon as quorum is met).
	Grace time.Duration
	// CountGateway: if true, KindLanternGateway sources count toward
	// quorum. Default false; the Lantern gateway is used for fast block
	// fetch but is not part of the trust quorum unless the operator opts
	// in.
	CountGateway bool
	// Progress, if non-nil, is called once per source result as it
	// completes. Useful for live spinners / per-source ✓/✗ in init UX.
	Progress func(SourceResult)
}

// ErrInsufficientSources is returned when the countable sources span
// fewer distinct operators than the quorum threshold.
var ErrInsufficientSources = errors.New("bootstrap: fewer countable operators than quorum")

// ErrQuorumNotReached is returned when sources respond but no single
// finality is backed by enough distinct operators.
var ErrQuorumNotReached = errors.New("bootstrap: quorum not reached")

// ErrConflict is returned when two responders name different tipsets for
// the same F3 instance. It is never retried and never overridden by a
// large agreeing majority: the bootstrap refuses to pick a side.
var ErrConflict = errors.New("bootstrap: conflicting finality answers")

// Quorum runs all sources in parallel under a single deadline, tallies
// their answers by distinct operator, and returns a QuorumResult.
//
// Success requires both:
//   - one finality backed by >= opts.Quorum distinct operators, and
//   - zero conflicts: no responder (counted or not) named a different
//     tipset for an instance some other responder answered.
//
// Answers for a different (older or newer) instance are lag, not
// conflict: they do not count toward the winner and do not fail the probe.
//
// Otherwise err is one of ErrInsufficientSources, ErrQuorumNotReached or
// ErrConflict. Quorum never panics on a misbehaving source; per-source
// failures are recorded in Results and surface to opts.Progress.
func Quorum(ctx context.Context, sources []Source, opts QuorumOptions) (*QuorumResult, error) {
	if opts.Quorum <= 0 {
		opts.Quorum = DefaultQuorum
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.Grace == 0 {
		opts.Grace = DefaultGrace
	}

	counts := func(s Source) bool { return s.Kind() != KindLanternGateway || opts.CountGateway }

	// Count how many distinct operators can be tallied at all.
	countableOps := map[string]bool{}
	countable := 0
	for _, s := range sources {
		if !counts(s) {
			continue
		}
		countable++
		countableOps[OperatorKey(s)] = true
	}
	if len(countableOps) < opts.Quorum {
		return &QuorumResult{
				Required:           opts.Quorum,
				CountableOperators: len(countableOps),
			}, fmt.Errorf("%w: have %d countable sources from %d distinct operators, need %d operators",
				ErrInsufficientSources, countable, len(countableOps), opts.Quorum)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	ch := make(chan SourceResult, len(sources))
	var wg sync.WaitGroup
	for _, s := range sources {
		wg.Add(1)
		go func(s Source) {
			defer wg.Done()
			t0 := time.Now()
			fin, err := s.LatestFinality(ctx)
			ch <- SourceResult{
				Name:     s.Name(),
				Kind:     s.Kind(),
				Operator: OperatorKey(s),
				Finality: fin,
				Error:    err,
				Duration: time.Since(t0),
				Counted:  counts(s),
			}
		}(s)
	}
	go func() { wg.Wait(); close(ch) }()

	t := newTally()
	results := make([]SourceResult, 0, len(sources))
	countedOK := 0
	var graceC <-chan time.Time
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel() // in-flight sources return promptly with ctx errors
		}
	}

loop:
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				break loop
			}
			results = append(results, r)
			if opts.Progress != nil {
				opts.Progress(r)
			}
			if !r.OK() {
				continue
			}
			if r.Counted {
				countedOK++
			}
			t.add(r)
			if len(t.conflicts) > 0 {
				stop() // fail fast: nothing can make this probe succeed
				continue
			}
			if graceC == nil && !stopped && t.bestOps() >= opts.Quorum {
				if opts.Grace < 0 {
					stop()
				} else {
					timer := time.NewTimer(opts.Grace)
					defer timer.Stop()
					graceC = timer.C
				}
			}
		case <-graceC:
			graceC = nil
			stop()
		}
	}

	out := &QuorumResult{
		Required:           opts.Quorum,
		Results:            results,
		Counted:            countedOK,
		CountableOperators: len(countableOps),
		Buckets:            t.sortedBuckets(),
		Conflicts:          t.conflicts,
		Elapsed:            time.Since(start),
	}
	if len(out.Conflicts) > 0 {
		msgs := make([]string, 0, len(out.Conflicts))
		for _, c := range out.Conflicts {
			msgs = append(msgs, c.String())
		}
		return out, fmt.Errorf("%w: %s", ErrConflict, strings.Join(msgs, "; "))
	}
	if len(out.Buckets) > 0 && len(out.Buckets[0].Operators) >= opts.Quorum {
		w := out.Buckets[0]
		out.Reached = true
		out.Winning = w.Finality
		out.Agreeing = append([]string(nil), w.Sources...)
		out.AgreeingOperators = append([]string(nil), w.Operators...)
		return out, nil
	}
	best := 0
	if len(out.Buckets) > 0 {
		best = len(out.Buckets[0].Operators)
	}
	return out, fmt.Errorf("%w: best answer was backed by %d of %d required distinct operators (%d of %d sources answered, %d distinct operators reachable)",
		ErrQuorumNotReached, best, opts.Quorum, countedOK, countable, respondingOperators(results))
}

// tally buckets answers and detects same-instance conflicts. Counted and
// non-counted answers both feed conflict detection; only counted answers
// back a bucket's operator set.
type tally struct {
	buckets   map[string]*bucketAcc
	byInst    map[uint64][]string // instance -> bucket keys seen
	conflicts []Conflict
}

type bucketAcc struct {
	b   Bucket
	ops map[string]bool
}

func newTally() *tally {
	return &tally{buckets: map[string]*bucketAcc{}, byInst: map[uint64][]string{}}
}

func (t *tally) add(r SourceResult) {
	k := r.Finality.Key()
	acc, ok := t.buckets[k]
	if !ok {
		acc = &bucketAcc{b: Bucket{Finality: r.Finality}, ops: map[string]bool{}}
		t.buckets[k] = acc
		inst := r.Finality.Instance
		for _, other := range t.byInst[inst] {
			t.conflicts = append(t.conflicts, Conflict{Instance: inst, A: t.buckets[other].snapshot(), B: Bucket{Finality: r.Finality, Sources: []string{r.Name}}})
		}
		t.byInst[inst] = append(t.byInst[inst], k)
	}
	acc.b.Sources = append(acc.b.Sources, r.Name)
	if r.Counted && !acc.ops[r.Operator] {
		acc.ops[r.Operator] = true
		acc.b.Operators = append(acc.b.Operators, r.Operator)
		sort.Strings(acc.b.Operators)
	}
}

func (a *bucketAcc) snapshot() Bucket {
	b := a.b
	b.Sources = append([]string(nil), a.b.Sources...)
	b.Operators = append([]string(nil), a.b.Operators...)
	return b
}

func (t *tally) bestOps() int {
	best := 0
	for _, a := range t.buckets {
		if n := len(a.b.Operators); n > best {
			best = n
		}
	}
	return best
}

func (t *tally) sortedBuckets() []Bucket {
	out := make([]Bucket, 0, len(t.buckets))
	for _, a := range t.buckets {
		out = append(out, a.snapshot())
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].Operators) != len(out[j].Operators) {
			return len(out[i].Operators) > len(out[j].Operators)
		}
		if len(out[i].Sources) != len(out[j].Sources) {
			return len(out[i].Sources) > len(out[j].Sources)
		}
		return out[i].Finality.Instance > out[j].Finality.Instance
	})
	return out
}

func respondingOperators(rs []SourceResult) int {
	ops := map[string]bool{}
	for _, r := range rs {
		if r.OK() && r.Counted {
			ops[r.Operator] = true
		}
	}
	return len(ops)
}

// FailedSources returns the sources that errored, excluding the ones the
// driver itself cancelled after the outcome was decided.
func (r *QuorumResult) FailedSources() []SourceResult {
	if r == nil {
		return nil
	}
	var out []SourceResult
	for _, s := range r.Results {
		if s.Error == nil || (r.Reached && errors.Is(s.Error, context.Canceled)) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// FormatReport renders a multi-line human-readable summary of a
// QuorumResult, suitable for printing under `lantern init`,
// `lantern doctor`, or error pages in the installer.
func FormatReport(r *QuorumResult) string {
	if r == nil {
		return "(no result)"
	}
	var b strings.Builder
	switch {
	case r.Reached:
		fmt.Fprintf(&b, "✓ Quorum reached: %d distinct operators agree (need %d): %s\n",
			len(r.AgreeingOperators), r.Required, r.Winning)
		fmt.Fprintf(&b, "  Operators: %s\n", strings.Join(r.AgreeingOperators, ", "))
	case len(r.Results) == 0 && r.CountableOperators < r.Required:
		fmt.Fprintf(&b, "✗ Not probed: only %d distinct operators configured, %d required\n", r.CountableOperators, r.Required)
		return b.String()
	case len(r.Conflicts) > 0:
		fmt.Fprintf(&b, "✗ CONFLICT: sources returned different finalized tipsets for the same F3 instance\n")
	default:
		fmt.Fprintf(&b, "✗ Quorum NOT reached: %s\n", bestBucketSize(r))
	}
	fmt.Fprintf(&b, "  Elapsed: %s, sources answered: %d/%d, distinct operators reachable: %d/%d\n",
		r.Elapsed.Round(time.Millisecond), r.Counted, len(r.Results), respondingOperators(r.Results), r.CountableOperators)
	fmt.Fprintln(&b, "  Per-source:")
	for _, s := range r.Results {
		mark := "✓"
		extra := s.Finality.String()
		switch {
		case s.Error != nil && r.Reached && errors.Is(s.Error, context.Canceled):
			mark = "·"
			extra = "not needed (quorum already reached)"
		case s.Error != nil:
			mark = "✗"
			extra = s.Error.Error()
		case !s.Counted:
			mark = "·"
			extra = extra + " (not counted)"
		}
		fmt.Fprintf(&b, "    %s [%s] %s {%s}: %s (%s)\n",
			mark, s.Kind, s.Name, s.Operator, extra, s.Duration.Round(time.Millisecond))
	}
	for _, c := range r.Conflicts {
		fmt.Fprintf(&b, "  CONFLICT %s\n", c)
	}
	if !r.Reached && len(r.Buckets) > 1 {
		fmt.Fprintln(&b, "  Distinct answers:")
		for i, bk := range r.Buckets {
			fmt.Fprintf(&b, "    [%d] %s: %d operators (%s) via %s\n",
				i, bk.Finality, len(bk.Operators), strings.Join(bk.Operators, ", "), strings.Join(bk.Sources, ", "))
		}
	}
	return b.String()
}

func bestBucketSize(r *QuorumResult) string {
	if len(r.Buckets) == 0 {
		return "no successful sources"
	}
	b := r.Buckets[0]
	return fmt.Sprintf("best answer backed by %d/%d distinct operators (%s): %s",
		len(b.Operators), r.Required, strings.Join(b.Operators, ", "), b.Finality)
}
