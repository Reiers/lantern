// Package bootstrap implements Lantern's multi-source trust-bootstrap
// quorum, as specified in INSTALLER-SPEC.md §3.
//
// The premise: no single source determines Lantern's trust anchor. We
// query N independent sources in parallel, ask each "what is the current
// F3-finalized tipset?", and only accept the answer when ≥quorum sources
// cryptographically agree on the same (instance, tipsetKey, stateRoot).
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
	// Reached is true iff at least Quorum sources agreed on the same
	// finality.
	Reached bool
	// Required is the quorum threshold that was asked for.
	Required int
	// Winning is the finality that reached quorum (zero-value if none).
	Winning Finality
	// Agreeing is the names of sources that agreed on Winning.
	Agreeing []string
	// Results is per-source detail (all sources, in completion order).
	Results []SourceResult
	// Counted is the number of sources whose answers were actually
	// tallied (i.e. counted=true and OK).
	Counted int
	// Buckets summarises the distinct finalities seen, by count.
	Buckets []Bucket
	// Elapsed is the wall-clock time the probe took.
	Elapsed time.Duration
}

// Bucket is one distinct (instance, tsk, stateroot) tuple seen during a
// probe, plus the names of sources that returned it.
type Bucket struct {
	Finality Finality
	Sources  []string
}

// QuorumOptions configures a Quorum() run.
type QuorumOptions struct {
	// Quorum is the minimum number of agreeing, counted sources required.
	// Default: 5. Required is enforced before any source is queried;
	// Quorum() returns ErrInsufficientSources if len(sources_counted) <
	// Quorum.
	Quorum int
	// Timeout is the total wall-clock budget. Default: 60s.
	Timeout time.Duration
	// CountGateway: if true, KindLanternGateway sources count toward
	// quorum. Default false; the Lantern gateway is used for fast block
	// fetch but is not part of the trust quorum unless the operator opts
	// in.
	CountGateway bool
	// Progress, if non-nil, is called once per source result as it
	// completes. Useful for live spinners / per-source ✓/✗ in init UX.
	Progress func(SourceResult)
}

// ErrInsufficientSources is returned when fewer countable sources are
// supplied than the quorum threshold.
var ErrInsufficientSources = errors.New("bootstrap: fewer countable sources than quorum")

// ErrQuorumNotReached is returned when sources respond but no single
// finality reaches the quorum threshold.
var ErrQuorumNotReached = errors.New("bootstrap: quorum not reached")

// Quorum runs all sources in parallel under a single deadline, tallies
// their answers, and returns a QuorumResult. If at least opts.Quorum
// counted sources agreed on the same finality, Reached is true and err
// is nil. Otherwise err is one of ErrInsufficientSources,
// ErrQuorumNotReached, or context.DeadlineExceeded.
//
// Quorum never panics on a misbehaving source; per-source failures are
// recorded in Results and surface to opts.Progress as they happen.
func Quorum(ctx context.Context, sources []Source, opts QuorumOptions) (*QuorumResult, error) {
	if opts.Quorum <= 0 {
		opts.Quorum = 5
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}

	// Count how many sources will be tallied at all.
	countable := 0
	for _, s := range sources {
		if s.Kind() == KindLanternGateway && !opts.CountGateway {
			continue
		}
		countable++
	}
	if countable < opts.Quorum {
		return &QuorumResult{
				Required: opts.Quorum,
			}, fmt.Errorf("%w: have %d countable sources, need %d",
				ErrInsufficientSources, countable, opts.Quorum)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	type signal struct {
		i   int
		res SourceResult
	}
	ch := make(chan signal, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Add(1)
		go func(i int, s Source) {
			defer wg.Done()
			t0 := time.Now()
			fin, err := s.LatestFinality(ctx)
			counted := s.Kind() != KindLanternGateway || opts.CountGateway
			ch <- signal{i: i, res: SourceResult{
				Name:     s.Name(),
				Kind:     s.Kind(),
				Finality: fin,
				Error:    err,
				Duration: time.Since(t0),
				Counted:  counted,
			}}
		}(i, s)
	}
	go func() { wg.Wait(); close(ch) }()

	results := make([]SourceResult, 0, len(sources))
	buckets := map[string]*Bucket{}
	countedOK := 0

	// Short-circuit: as soon as the winning bucket reaches quorum we can
	// cancel the context and stop waiting. Other in-flight responses
	// still arrive via ch — we drain them so per-source detail is
	// preserved for the user-facing report.
	var winning *Bucket
	for sig := range ch {
		r := sig.res
		results = append(results, r)
		if opts.Progress != nil {
			opts.Progress(r)
		}
		if r.OK() && r.Counted {
			countedOK++
			k := r.Finality.Key()
			b, ok := buckets[k]
			if !ok {
				b = &Bucket{Finality: r.Finality}
				buckets[k] = b
			}
			b.Sources = append(b.Sources, r.Name)
			if len(b.Sources) >= opts.Quorum && winning == nil {
				w := b
				winning = w
				cancel() // stop the deadline timer / in-flight queries
			}
		}
	}

	out := &QuorumResult{
		Required: opts.Quorum,
		Results:  results,
		Counted:  countedOK,
		Elapsed:  time.Since(start),
	}
	for _, b := range buckets {
		out.Buckets = append(out.Buckets, *b)
	}
	sort.Slice(out.Buckets, func(i, j int) bool {
		return len(out.Buckets[i].Sources) > len(out.Buckets[j].Sources)
	})
	if winning != nil {
		out.Reached = true
		out.Winning = winning.Finality
		out.Agreeing = append([]string(nil), winning.Sources...)
		return out, nil
	}
	if ctx.Err() != nil && countedOK < opts.Quorum {
		return out, fmt.Errorf("%w: %v (got %d/%d counted responses)",
			ErrQuorumNotReached, ctx.Err(), countedOK, opts.Quorum)
	}
	return out, fmt.Errorf("%w: no single tipset reached %d sources (countable=%d, counted=%d)",
		ErrQuorumNotReached, opts.Quorum, countable, countedOK)
}

// FormatReport renders a multi-line human-readable summary of a
// QuorumResult, suitable for printing under `lantern init`,
// `lantern doctor`, or error pages in the installer.
func FormatReport(r *QuorumResult) string {
	if r == nil {
		return "(no result)"
	}
	var b strings.Builder
	if r.Reached {
		fmt.Fprintf(&b, "✓ Quorum reached (%d/%d agree): %s\n",
			len(r.Agreeing), r.Required, r.Winning)
	} else {
		fmt.Fprintf(&b, "✗ Quorum NOT reached: best bucket had %s\n", bestBucketSize(r))
	}
	fmt.Fprintf(&b, "  Elapsed: %s, sources counted: %d/%d\n", r.Elapsed, r.Counted, len(r.Results))
	fmt.Fprintln(&b, "  Per-source:")
	for _, s := range r.Results {
		mark := "✓"
		extra := s.Finality.String()
		if s.Error != nil {
			mark = "✗"
			extra = s.Error.Error()
		} else if !s.Counted {
			mark = "·"
			extra = extra + " (not counted)"
		}
		fmt.Fprintf(&b, "    %s [%s] %s — %s (%s)\n",
			mark, s.Kind, s.Name, extra, s.Duration.Round(time.Millisecond))
	}
	if !r.Reached && len(r.Buckets) > 1 {
		fmt.Fprintln(&b, "  Divergence — distinct answers:")
		for i, bk := range r.Buckets {
			fmt.Fprintf(&b, "    [%d] %s — %d sources: %s\n",
				i, bk.Finality, len(bk.Sources), strings.Join(bk.Sources, ", "))
		}
	}
	return b.String()
}

func bestBucketSize(r *QuorumResult) string {
	if len(r.Buckets) == 0 {
		return "no successful sources"
	}
	b := r.Buckets[0]
	return fmt.Sprintf("%d/%d agree on %s", len(b.Sources), r.Required, b.Finality)
}
