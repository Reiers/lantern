// Package headcheck is Lantern's running-head divergence monitor.
//
// Background (#85, community discussion, #80):
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
// #85 ask). When the local gossip head drifts outside that
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
	ltypes "github.com/Reiers/lantern/chain/types"
)

// DefaultLookback is the head-agreement tolerance in epochs. #85
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

// OperatorSource is an optional HeadSource extension (#153). A source that
// knows which upstream operator actually answers it (e.g. an RPC URL's
// registrable domain, or the upstream a Lantern gateway proxies to)
// reports it here, and independence is counted per operator instead of
// per transport Kind. This stops one upstream (Glif reached directly and
// via the gateway) from casting two votes.
type OperatorSource interface {
	Operator() string
}

// voterKey is the independence key a source votes under: its operator
// when known, else its Kind.
func voterKey(src HeadSource) string {
	if os, ok := src.(OperatorSource); ok {
		if op := os.Operator(); op != "" {
			return "op:" + op
		}
	}
	return "kind:" + string(src.Kind())
}

// PeerVote is one libp2p peer's head, pre-classified against our chain
// (#154). Voter is the peer's network group (see net/peerheads.GroupOf);
// peers in one group are ONE voter. OnChain: 1 = the peer's head (or its
// parent tipset) is on our canonical chain, -1 = provably on another
// chain, 0 = can't tell (height-only vote).
type PeerVote struct {
	Voter   string
	Epoch   abi.ChainEpoch
	OnChain int
}

// TipSetRef identifies one tipset as seen by a source (or locally).
type TipSetRef struct {
	Epoch        abi.ChainEpoch
	Key          ltypes.TipSetKey
	ParentWeight ltypes.BigInt
}

// TipSetAtSource is an optional HeadSource extension (#152). A source that
// can report WHICH tipset it has at an epoch (Lotus
// ChainGetTipSetByHeight semantics: null rounds resolve to the tipset
// below) is checked for chain agreement, not just head height.
//
// Why: height alone cannot distinguish an eclipse fork from the real
// chain. An attacker feeding a self-consistent fork keeps our head at the
// right height, so every honest source "agrees" within the lookback. The
// tipset key at a checkpoint a few epochs below our head is the same on
// every honest node (tip churn from late blocks only affects the last
// epoch or two) and differs on any fork deeper than the lookback.
type TipSetAtSource interface {
	HeadSource
	TipSetAt(ctx context.Context, epoch abi.ChainEpoch) (TipSetRef, error)
}

// ChainVote is one chain (tipset key at the checkpoint) and the distinct
// independent voters (see voterKey) that reported it. Used for
// cross-source fork choice.
type ChainVote struct {
	Ref    TipSetRef
	Voters []string // sorted voter keys
	Local  bool     // this is Lantern's own chain
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
	// StatusBehind (#162): a quorum of independent voters is AHEAD of us on
	// our own chain (tipset key matches at the checkpoint, or height-only
	// sources). That is lag (fresh start, restart, network blip), not an
	// eclipse. Closing the adoption gate would freeze the node behind
	// forever, so Behind keeps the gate open and lets Sync/gossip catch up.
	StatusBehind
)

func (s Status) String() string {
	switch s {
	case StatusAgree:
		return "agree"
	case StatusDiverge:
		return "diverge"
	case StatusInsufficient:
		return "insufficient"
	case StatusBehind:
		return "behind"
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
	Lagging       int             // #162: disagreeing voters that are only ahead of us on our chain
	PeerVoters    int             // #154: distinct libp2p peer groups that voted
	Reachable     int             // sources that answered at all
	Total         int             // sources configured
	MedianExtHead abi.ChainEpoch  // median external head (−1 if none)
	At            time.Time       // when this round completed
	PerKind       map[string]bool // voter key ("op:<operator>" or "kind:<Kind>") -> agreed (#153)

	// #152 tipset-key agreement. CheckpointEpoch is -1 on a height-only
	// round (no LocalTipSetAt, local head too low, or local tipset
	// unknown).
	CheckpointEpoch abi.ChainEpoch
	KeyChecked      int // distinct Kinds whose tipset key was compared
	ForkedKinds     int // Kinds within height tolerance but on another chain
	// Canonical is the cross-source fork-choice winner at the checkpoint:
	// the chain reported by the most distinct Kinds, ties broken by
	// heavier ParentWeight, then by key bytes (deterministic). nil when no
	// source was key-checked.
	Canonical *ChainVote
}

// Config configures a Monitor.
type Config struct {
	// Local reports Lantern's own (gossip-derived) head epoch. Required.
	Local func() abi.ChainEpoch
	// LocalTipSetAt resolves Lantern's own canonical tipset at or below an
	// epoch. When set, sources implementing TipSetAtSource are checked for
	// tipset-key agreement at checkpoint = local head - Lookback (#152).
	// nil = height-only corroboration (pre-#152 behaviour).
	LocalTipSetAt func(abi.ChainEpoch) (TipSetRef, bool)
	// Sources are the external observers. Polled in parallel each round.
	Sources []HeadSource
	// PeerVotes (#154), when set, adds libp2p peer groups as voters each
	// round. This is what gives bridge-off (--no-fallback-rpc) a running
	// quorum with zero RPC. Called with our local head epoch.
	PeerVotes func(local abi.ChainEpoch) []PeerVote
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

	// #152: resolve our own tipset at the checkpoint once per round.
	checkpoint := abi.ChainEpoch(-1)
	var localRef TipSetRef
	if m.cfg.LocalTipSetAt != nil && local >= m.cfg.Lookback {
		if ref, ok := m.cfg.LocalTipSetAt(local - m.cfg.Lookback); ok {
			checkpoint = local - m.cfg.Lookback
			localRef = ref
		}
	}

	type answer struct {
		kind  string // voter key (#153)
		epoch abi.ChainEpoch
		ok    bool
		keyed bool // tipset key compared at the checkpoint
		ref   TipSetRef
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
			a := answer{kind: voterKey(src), epoch: ep, ok: err == nil}
			if a.ok && checkpoint >= 0 {
				if ks, capable := src.(TipSetAtSource); capable {
					ref, rerr := ks.TipSetAt(cctx, checkpoint)
					if rerr == nil {
						a.keyed = true
						a.ref = ref
					}
					// On error (e.g. source lagging below the checkpoint)
					// the source degrades to a height-only vote, which keeps
					// the pre-#152 far-behind => disagree semantics.
				}
			}
			answers[i] = a
		}(i, src)
	}
	wg.Wait()

	// Collapse to distinct Kind: a Kind agrees if ANY source of that Kind
	// is within Lookback of local (and, when key-checked, on our chain at
	// the checkpoint); it disagrees only if it answered and no source of
	// that Kind agreed. This makes N Glif URLs count once.
	kindAgreed := map[string]bool{}
	kindAnswered := map[string]bool{}
	kindKeyed := map[string]bool{}
	kindForked := map[string]bool{}
	kindLag := map[string]bool{} // #162: ahead of us, on our chain
	votes := map[ltypes.TipSetKey]*ChainVote{}
	voteKinds := map[ltypes.TipSetKey]map[string]bool{}
	var extHeads []abi.ChainEpoch
	reachable := 0
	for _, a := range answers {
		if !a.ok {
			continue
		}
		reachable++
		extHeads = append(extHeads, a.epoch)
		kindAnswered[a.kind] = true
		heightOK := withinLookback(local, a.epoch, m.cfg.Lookback)
		keyOK := true
		if a.keyed {
			kindKeyed[a.kind] = true
			keyOK = a.ref.Key == localRef.Key
			if !keyOK {
				// On another chain at our checkpoint, at any height:
				// never mere lag (#162).
				kindForked[a.kind] = true
			}
			v, ok := votes[a.ref.Key]
			if !ok {
				v = &ChainVote{Ref: a.ref, Local: a.ref.Key == localRef.Key}
				votes[a.ref.Key] = v
				voteKinds[a.ref.Key] = map[string]bool{}
			}
			voteKinds[a.ref.Key][a.kind] = true
		}
		if heightOK && keyOK {
			kindAgreed[a.kind] = true
		} else if keyOK && a.epoch > local+m.cfg.Lookback {
			kindLag[a.kind] = true
		}
	}

	// #154: libp2p peer-group votes. A peer far BEHIND us is not evidence
	// of anything (peers sync, hello heads are connect-time), so it's
	// skipped unless it's provably on another chain. Forked peers always
	// count: that's the eclipse signal.
	peerVoters := map[string]bool{}
	if m.cfg.PeerVotes != nil {
		for _, pv := range m.cfg.PeerVotes(local) {
			if pv.Voter == "" {
				continue
			}
			if pv.OnChain != -1 && local >= 0 && pv.Epoch < local-m.cfg.Lookback {
				continue
			}
			k := "net:" + pv.Voter
			peerVoters[k] = true
			reachable++
			extHeads = append(extHeads, pv.Epoch)
			kindAnswered[k] = true
			switch {
			case pv.OnChain == -1:
				kindForked[k] = true
			case withinLookback(local, pv.Epoch, m.cfg.Lookback):
				kindAgreed[k] = true
			case pv.Epoch > local+m.cfg.Lookback:
				kindLag[k] = true
			}
		}
	}

	agreeing := 0
	disagreeing := 0
	lagging := 0
	forked := 0
	perKind := map[string]bool{}
	for k := range kindAnswered {
		if kindAgreed[k] {
			agreeing++
			perKind[k] = true
		} else {
			disagreeing++
			perKind[k] = false
			if kindForked[k] {
				forked++
			} else if kindLag[k] {
				lagging++
			}
		}
	}

	var canonical *ChainVote
	for key, v := range votes {
		for k := range voteKinds[key] {
			v.Voters = append(v.Voters, k)
		}
		sort.Strings(v.Voters)
		if canonical == nil || betterVote(v, canonical) {
			canonical = v
		}
	}

	status := classify(agreeing, disagreeing-lagging, lagging, reachable, m.cfg.MinAgree)
	res := Result{
		Status:        status,
		LocalHead:     local,
		Agreeing:      agreeing,
		Disagreeing:   disagreeing,
		Lagging:       lagging,
		PeerVoters:    len(peerVoters),
		Reachable:     reachable,
		Total:         len(m.cfg.Sources),
		MedianExtHead: median(extHeads),
		At:            time.Now(),
		PerKind:       perKind,

		CheckpointEpoch: checkpoint,
		KeyChecked:      len(kindKeyed),
		ForkedKinds:     forked,
		Canonical:       canonical,
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

// classify turns the tallies into a Status.
//
//   - DIVERGE: a quorum of independent voters is on another chain at the
//     checkpoint, or clusters BEHIND us (we are ahead of the independent
//     world), and they out-number the agreeing voters. Eclipse alarm.
//   - AGREE: at least MinAgree independent voters are within Lookback and
//     (when key-checked) on our chain.
//   - BEHIND (#162): the remaining disagreement is voters AHEAD of us on
//     our chain. Lag, not an eclipse; must not close the adoption gate.
//   - INSUFFICIENT: otherwise.
func classify(agreeing, diverging, lagging, reachable, minAgree int) Status {
	if reachable == 0 {
		return StatusInsufficient
	}
	if diverging >= minAgree && diverging >= agreeing {
		return StatusDiverge
	}
	if agreeing >= minAgree {
		return StatusAgree
	}
	if lagging >= minAgree {
		return StatusBehind
	}
	return StatusInsufficient
}

// betterVote is cross-source fork choice (#152): more distinct independent
// Kinds wins; ties go to the heavier ParentWeight (Filecoin fork choice);
// remaining ties to the lexically smaller key so the pick is deterministic.
func betterVote(a, b *ChainVote) bool {
	if len(a.Voters) != len(b.Voters) {
		return len(a.Voters) > len(b.Voters)
	}
	if c := cmpWeight(a.Ref.ParentWeight, b.Ref.ParentWeight); c != 0 {
		return c > 0
	}
	return string(a.Ref.Key.Bytes()) < string(b.Ref.Key.Bytes())
}

// cmpWeight compares two weights, treating an unset weight as zero.
func cmpWeight(a, b ltypes.BigInt) int {
	if a.Nil() {
		a = ltypes.NewInt(0)
	}
	if b.Nil() {
		b = ltypes.NewInt(0)
	}
	return a.Int.Cmp(b.Int)
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
