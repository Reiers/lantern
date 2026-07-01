package ecfinality

import (
	"errors"
	"math"
	"sync"

	"github.com/filecoin-project/go-state-types/abi"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// MinWindow is the minimum number of epochs of observed history required
// before the calculator reports a threshold at all. Below this the L
// distribution has too little data to be meaningful, so Status reports
// depth -1 (not computable) rather than an over-confident number. 30
// epochs (~15 min of history) is the depth at which a healthy chain
// typically meets 2^-30 in the first place.
const MinWindow = 30

// HeaderSource is the minimal store surface the cache walks. Lantern's
// chain/header/store.Store satisfies it.
type HeaderSource interface {
	Head() *ltypes.TipSet
	GetTipSet(tsk ltypes.TipSetKey) (*ltypes.TipSet, error)
}

// Status is the resolved EC finality state for a given head.
type Status struct {
	// ThresholdDepth is the shallowest epoch depth at which the reorg
	// probability drops below 2^-30. -1 when not met or not computable
	// (degraded chain, or observed window < MinWindow).
	ThresholdDepth int
	// FinalizedEpoch is head.Height() - ThresholdDepth, or -1 when
	// ThresholdDepth is -1.
	FinalizedEpoch abi.ChainEpoch
	// HeadEpoch is the head height the computation ran against.
	HeadEpoch abi.ChainEpoch
	// WindowEpochs is how many epochs of observed history the calculator
	// actually had (honesty signal: a freshly-booted node has only its
	// anchor depth until #91/#92 give it a deeper tail).
	WindowEpochs int
}

// Cache computes the FRC-0089 threshold for the current head and caches
// the result, recomputing only when the head changes. Computation is
// on-demand (dashboard/stats pulls), NOT on every head change: the cost
// is dominated by the Skellam PMF loops, so an idle node pays nothing.
// Safe for concurrent use.
type Cache struct {
	src      HeaderSource
	finality int // L-distribution lookback (900 mainnet/calibration)

	mu     sync.Mutex
	cached *Status
	calls  uint64 // lifetime Status calls
	comps  uint64 // lifetime recomputes (cache misses)
}

// NewCache builds a Cache over the given header source. finality is the
// lookback depth for the L distribution (900 for both mainnet and
// calibration).
func NewCache(src HeaderSource, finality int) *Cache {
	return &Cache{src: src, finality: finality}
}

// Status returns the EC finality state for the current head, cached per
// head tipset key. Errors are not cached; a transient store failure
// allows retry on the next call.
func (c *Cache) Status() (*Status, error) {
	if c == nil || c.src == nil {
		return nil, errors.New("ecfinality: no header source")
	}
	head := c.src.Head()
	if head == nil {
		return nil, errors.New("ecfinality: no head")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++

	if c.cached != nil && c.cached.HeadEpoch == head.Height() {
		return c.cached, nil
	}
	c.comps++

	chain, err := c.walkChain(head)
	if err != nil {
		return nil, err
	}

	st := &Status{
		ThresholdDepth: -1,
		FinalizedEpoch: -1,
		HeadEpoch:      head.Height(),
		WindowEpochs:   len(chain),
	}
	if len(chain) >= MinWindow {
		guarantee := math.Pow(2, float64(DefaultSafetyExponent))
		st.ThresholdDepth = FindThresholdDepth(chain, c.finality, DefaultBlocksPerEpoch, DefaultByzantineFraction, guarantee)
		if st.ThresholdDepth >= 0 {
			st.FinalizedEpoch = head.Height() - abi.ChainEpoch(st.ThresholdDepth)
		}
	}

	c.cached = st
	return st, nil
}

// Stats returns (lifetime Status calls, lifetime recomputes).
func (c *Cache) Stats() (calls, recomputes uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.comps
}

// walkChain walks back from head collecting block counts per epoch, oldest
// first. Null rounds appear as 0 entries so the array index maps directly
// to epoch height differences.
//
// Lantern's header store is windowed (anchor-rooted, pruning per #92), so
// the walk stops gracefully where history runs out: a missing parent ends
// the walk instead of erroring, and Status reports the achieved window.
// The target is finality+5 epochs (the reference lookback); nodes with a
// deeper tail (#91) converge to the exact reference behavior.
func (c *Cache) walkChain(head *ltypes.TipSet) ([]int, error) {
	needed := c.finality + 5
	chain := make([]int, 0, min(needed, 1024))
	ts := head
	for {
		chain = append(chain, len(ts.Cids()))
		if len(chain) >= needed {
			break
		}
		if ts.Height() == 0 {
			break // genesis
		}
		parent, err := c.src.GetTipSet(ts.Parents())
		if err != nil || parent == nil {
			break // window exhausted: use what we observed
		}
		// Insert 0 entries for null rounds between this tipset and parent.
		for nulls := int(ts.Height()-parent.Height()) - 1; nulls > 0 && len(chain) < needed; nulls-- {
			chain = append(chain, 0)
		}
		ts = parent
	}
	// Reverse to chronological order (oldest first).
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}
