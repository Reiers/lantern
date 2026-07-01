// Package headcheck is Lantern's running-head divergence monitor.
//
// Background (snadrus#85, zenground0 fil-curio-dev thread, #80):
// Lantern follows the *running* (unfinalized) chain head over gossipsub
// (net/blockingest). The boot anchor is a strong multi-source 5-of-N
// quorum on an F3-finalized tipset (chain/bootstrap), and #79 added
// heaviest-ParentWeight fork choice so a competing *lighter* fork on the
// running head is rejected. What neither of those closes is the case
// where an attacker eclipses the gossip peer table and feeds a
// self-consistent, parent-linked, *heavier-looking* fork: the node would
// happily follow it because every individual header hashes fine and the
// weight arithmetic only sees the attacker's numbers.
//
// headcheck is the defense-in-depth layer #85 item 2 asks for: it
// periodically asks several INDEPENDENT head sources (gossip-observed
// tip, one or more Lotus-compatible RPC endpoints, user --peer
// endpoints) "what epoch is the head at?" and checks that they agree
// within a small look-back tolerance (default 3 blocks, matching the
// snadrus#85 ask). When the local gossip head drifts outside that
// tolerance from the diversity of external sources, headcheck raises a
// divergence signal so the daemon can log loudly / surface it on the
// dashboard / (optionally) refuse to serve a head it can't corroborate.
//
// It is explicitly NOT a trusted-RPC head oracle. Lantern never *takes*
// its head from an RPC (that is the whole point of the project, see
// TRUST-MODEL.md §3.1). headcheck only uses external sources to CORROBORATE
// or DISPUTE the head Lantern already derived from gossip. A single RPC
// saying something different does not move our head; a diverse quorum of
// independent sources disagreeing with us is an eclipse alarm.
//
// Source diversity matters: N sources that are really the same upstream
// (e.g. three Glif URLs) are one source for eclipse purposes. headcheck
// counts agreement by source Kind so a quorum requires genuinely
// independent observers, mirroring chain/bootstrap's Kind policy.
//
// This package is pure logic over a HeadSource interface; the libp2p /
// HTTP source adapters live with the daemon wiring so headcheck stays
// unit-testable with mocks.
package headcheck

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/bootstrap"
)

// DefaultLookback is the head-agreement tolerance in epochs. snadrus#85
// asks for a 3-block look-back: a source up to 3 epochs behind our head
// (or ahead of it) still counts as "agreeing", since gossip propagation
// and null rounds routinely put honest observers a couple epochs apart.
const DefaultLookback abi.ChainEpoch = 3

// DefaultInterval is how often the monitor polls its sources.
const DefaultInterval = 30 * time.Second

// DefaultMinAgree is the minimum number of distinct-Kind external sources
// that must report a head within Lookback of ours before we treat the
// local head as corroborated. With fewer than this many *reachable*
// sources headcheck reports StatusInsufficient rather than a false-clean
// or a false-alarm.
const DefaultMinAgree = 2

// HeadSource is a single independent observer of the chain head epoch.
// Implementations: the gossip-observed tip (local), a Lotus-compatible
// RPC endpoint, a user --peer endpoint, a libp2p peer. Kind is used to
// measure genuine independence (see package doc).
type HeadSource interface {
	// Name is a stable human label for logs/dashboard.
	Name() string
	// Kind classifies the source for diversity counting.
	Kind() bootstrap.Kind
	// HeadEpoch returns the source's current head epoch.
	HeadEpoch(ctx context.Context) (abi.ChainEpoch, error)
}

// Status is the outcome of one check round.
type Status int

const (
	// StatusUnknown: no round has run yet.
	StatusUnknown Status = iota
	// StatusAgree: enough independent sources are within Lookback of our
	// local head. The head is corroborated.
	StatusAgree
	// StatusDiverge: a quorum of independent sources reports a head
	// outside Lookback of ours. Possible eclipse / fork-follow. ALARM.
	StatusDiverge
	// StatusInsufficient: too few sources were reachable to make a call.
	// Not an alarm by itself, but means the head is uncorroborated.
	StatusInsufficient
)

func (s Status) String() string {
	switch s {
	case StatusAgree:
		return "agree"
	case StatusDiverge:
		return "diverge"
	case StatusInsufficient:
		return "insufficient"
	default:
		return "unknown"
	}
}

// Result is a snapshot of the most recent check round.
type Result struct {
	Status        Status
	LocalHead     abi.ChainEpoch
	Agreeing      int             // distinct-Kind sources within Lookback
	Disagreeing   int             // distinct-Kind sources outside Lookback
	Reachable     int             // sources that answered at all
	Total         int             // sources configured
	MedianExtHead abi.ChainEpoch  // median external head (−1 if none)
	At            time.Time       // when this round completed
	PerKind       map[string]bool // Kind -> agreed (for dashboard)
}

// Config configures a Monitor.
type Config struct {
	// Local reports Lantern's own (gossip-derived) head epoch. Required.
	Local func() abi.ChainEpoch
	// Sources are the external observers. Polled in parallel each round.
	Sources []HeadSource
	// Lookback tolerance in epochs (default DefaultLookback).
	Lookback abi.ChainEpoch
	// Interval between rounds (default DefaultInterval).
	Interval time.Duration
	// MinAgree distinct-Kind sources required to corroborate
	// (default DefaultMinAgree).
	MinAgree int
	// PerSourceTimeout caps each source's HeadEpoch call (default 15s).
	PerSourceTimeout time.Duration
	// OnResult, if set, fires after each round with the Result. Used by
	// the daemon to log alarms and update the dashboard. May be nil.
	OnResult func(Result)
}

// Monitor periodically checks local-vs-external head agreement.
type Monitor struct {
	cfg Config

	mu   sync.RWMutex
	last Result

	diverged atomic.Uint64
	rounds   atomic.Uint64
	stopOnce sync.Once
	stopCh   chan struct{}
}

// New builds a Monitor. Local is required; with no Sources every round
// is StatusInsufficient (the monitor is then a no-op alarm-wise, which
// is the correct behaviour for a node the operator hasn't given any
// corroborating endpoints).
func New(cfg Config) *Monitor {
	if cfg.Lookback <= 0 {
		cfg.Lookback = DefaultLookback
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MinAgree <= 0 {
		cfg.MinAgree = DefaultMinAgree
	}
	if cfg.PerSourceTimeout <= 0 {
		cfg.PerSourceTimeout = 15 * time.Second
	}
	return &Monitor{cfg: cfg, stopCh: make(chan struct{})}
}

// Last returns the most recent round's Result.
func (m *Monitor) Last() Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.last
}

// Stats returns lifetime counters: total rounds, divergent rounds.
func (m *Monitor) Stats() (rounds, diverged uint64) {
	return m.rounds.Load(), m.diverged.Load()
}

// Start runs the monitor loop until ctx is cancelled or Stop is called.
// Non-blocking: spawns a goroutine.
func (m *Monitor) Start(ctx context.Context) {
	go m.loop(ctx)
}

// Stop halts the monitor loop.
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
}

func (m *Monitor) loop(ctx context.Context) {
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	// Run one immediately so the dashboard isn't blank for a full interval.
	m.runRound(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-t.C:
			m.runRound(ctx)
		}
	}
}

// CheckOnce runs a single round synchronously and returns the Result.
// Exported for tests and for an on-demand dashboard refresh.
func (m *Monitor) CheckOnce(ctx context.Context) Result {
	return m.runRound(ctx)
}

func (m *Monitor) runRound(ctx context.Context) Result {
	m.rounds.Add(1)
	local := abi.ChainEpoch(-1)
	if m.cfg.Local != nil {
		local = m.cfg.Local()
	}

	type answer struct {
		kind  bootstrap.Kind
		epoch abi.ChainEpoch
		ok    bool
	}
	answers := make([]answer, len(m.cfg.Sources))
	var wg sync.WaitGroup
	for i, src := range m.cfg.Sources {
		wg.Add(1)
		go func(i int, src HeadSource) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, m.cfg.PerSourceTimeout)
			defer cancel()
			ep, err := src.HeadEpoch(cctx)
			answers[i] = answer{kind: src.Kind(), epoch: ep, ok: err == nil}
		}(i, src)
	}
	wg.Wait()

	// Collapse to distinct Kind: a Kind agrees if ANY source of that Kind
	// is within Lookback of local; it disagrees only if it answered and
	// no source of that Kind agreed. This makes N Glif URLs count once.
	kindAgreed := map[bootstrap.Kind]bool{}
	kindAnswered := map[bootstrap.Kind]bool{}
	var extHeads []abi.ChainEpoch
	reachable := 0
	for _, a := range answers {
		if !a.ok {
			continue
		}
		reachable++
		extHeads = append(extHeads, a.epoch)
		kindAnswered[a.kind] = true
		if withinLookback(local, a.epoch, m.cfg.Lookback) {
			kindAgreed[a.kind] = true
		}
	}

	agreeing := 0
	disagreeing := 0
	perKind := map[string]bool{}
	for k := range kindAnswered {
		if kindAgreed[k] {
			agreeing++
			perKind[string(k)] = true
		} else {
			disagreeing++
			perKind[string(k)] = false
		}
	}

	status := classify(agreeing, disagreeing, reachable, m.cfg.MinAgree)
	res := Result{
		Status:        status,
		LocalHead:     local,
		Agreeing:      agreeing,
		Disagreeing:   disagreeing,
		Reachable:     reachable,
		Total:         len(m.cfg.Sources),
		MedianExtHead: median(extHeads),
		At:            time.Now(),
		PerKind:       perKind,
	}
	if status == StatusDiverge {
		m.diverged.Add(1)
	}

	m.mu.Lock()
	m.last = res
	m.mu.Unlock()
	if m.cfg.OnResult != nil {
		m.cfg.OnResult(res)
	}
	return res
}

// classify turns the agree/disagree tallies into a Status.
//
//   - DIVERGE: a quorum of distinct independent Kinds disagree AND they
//     out-number the agreeing Kinds. A real eclipse shows up as the
//     external world (multiple Kinds) clustering away from our head.
//   - AGREE: at least MinAgree distinct Kinds are within Lookback.
//   - INSUFFICIENT: otherwise (too few corroborating observers).
func classify(agreeing, disagreeing, reachable, minAgree int) Status {
	if reachable == 0 {
		return StatusInsufficient
	}
	if disagreeing >= minAgree && disagreeing >= agreeing {
		return StatusDiverge
	}
	if agreeing >= minAgree {
		return StatusAgree
	}
	return StatusInsufficient
}

// withinLookback reports whether external head `ext` is within `tol`
// epochs of `local` in either direction. local==-1 (no local head yet)
// is never within tolerance.
func withinLookback(local, ext, tol abi.ChainEpoch) bool {
	if local < 0 {
		return false
	}
	d := local - ext
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// median returns the median of the epochs, or -1 for an empty slice.
func median(xs []abi.ChainEpoch) abi.ChainEpoch {
	if len(xs) == 0 {
		return -1
	}
	cp := append([]abi.ChainEpoch(nil), xs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[len(cp)/2]
}
