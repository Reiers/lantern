// Package crosscheck implements the VM-bridge auditor (#98).
//
// The VM bridge (vm/bridge) is normally a PRODUCTION dependency: it
// computes post-execution state roots for block production. This package
// turns the same connection into an AUDITOR for the read path: it
// periodically asks the operator's own Forest/Lotus node for the
// canonical tipset at a settled depth and compares it against Lantern's
// header-store view. A mismatch means Lantern's head path and the
// operator's full node disagree about the canonical chain - a state-root
// level attack, an eclipse, or a bug - and is worth a loud alarm.
//
// Why this is honest trust-wise (TRUST-MODEL 4.x): the bridge is the
// operator's own node, already trusted for block production. Using it to
// CROSS-CHECK adds no new trust; the read path stays 100% local and is
// never answered by the bridge. The check is observe-only: a divergence
// alarms (log + counter + dashboard) but never blocks or rewrites
// Lantern's head. This delivers most of the practical security value of
// full re-execution (#89 Stage C) at ~zero cost, and doubles as the
// vector generator Stage C will need.
//
// Sampling: checks run at a fixed interval (default 60s) against depth
// head-K (default 3 epochs) so legitimate near-tip reorgs don't produce
// false alarms. Null rounds are skipped (heights must match to compare).
package crosscheck

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/filecoin-project/go-state-types/abi"

	logging "github.com/ipfs/go-log/v2"

	ltypes "github.com/Reiers/lantern/chain/types"
)

var log = logging.Logger("lantern/crosscheck")

// DefaultInterval is the default wall-clock spacing between checks.
const DefaultInterval = 60 * time.Second

// DefaultDepth is how many epochs behind head the check targets. 3
// epochs (~90s) is deep enough that honest near-tip reorgs have settled,
// shallow enough that an attack alarms within ~2 minutes.
const DefaultDepth = 3

// RPC is the bridge surface the checker needs. vm/bridge.ForestBridge
// (and the Bridge interface) satisfy it.
type RPC interface {
	RawJSONRPC(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
	Provenance() string
}

// Source is the local chain surface the checker audits.
// chain/header/store.Store satisfies it.
type Source interface {
	Head() *ltypes.TipSet
	GetTipSetByHeight(epoch abi.ChainEpoch) (*ltypes.TipSet, error)
}

// Config wires a Checker.
type Config struct {
	Bridge   RPC
	Source   Source
	Interval time.Duration // 0 => DefaultInterval
	Depth    int           // 0 => DefaultDepth
	// OnDiverge, when set, fires on every confirmed divergence (after
	// the height-match guard). Wire alerts/notifications here.
	OnDiverge func(epoch abi.ChainEpoch, ours, theirs string)
}

// Stats is a snapshot of checker counters.
type Stats struct {
	Checks           uint64
	Agrees           uint64
	Diverges         uint64
	Skipped          uint64 // bridge lag / null round / shallow chain / errors
	LastCheckedEpoch abi.ChainEpoch
	LastResult       string // "agree" | "DIVERGE" | "skip" | ""
	Provenance       string
}

// Checker periodically cross-checks the local canonical chain against
// the operator's bridge node. Observe-only: never mutates local state.
type Checker struct {
	cfg      Config
	interval time.Duration
	depth    abi.ChainEpoch

	checks    atomic.Uint64
	agrees    atomic.Uint64
	diverges  atomic.Uint64
	skipped   atomic.Uint64
	lastEpoch atomic.Int64
	lastRes   atomic.Value // string
}

// New builds a Checker. Bridge and Source are required.
func New(cfg Config) (*Checker, error) {
	if cfg.Bridge == nil || cfg.Source == nil {
		return nil, fmt.Errorf("crosscheck: Bridge and Source are required")
	}
	iv := cfg.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	d := cfg.Depth
	if d <= 0 {
		d = DefaultDepth
	}
	c := &Checker{cfg: cfg, interval: iv, depth: abi.ChainEpoch(d)}
	c.lastRes.Store("")
	return c, nil
}

// Start launches the periodic check loop; returns immediately. The loop
// exits when ctx is cancelled.
func (c *Checker) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(c.interval)
		defer t.Stop()
		log.Infow("vm-bridge cross-check auditor started",
			"provenance", c.cfg.Bridge.Provenance(), "interval", c.interval, "depth", int64(c.depth))
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.CheckOnce(ctx)
			}
		}
	}()
}

// bridgeTipSet is the JSON shape of Filecoin.ChainGetTipSetByHeight.
type bridgeTipSet struct {
	Cids   []cidJSON `json:"Cids"`
	Height int64     `json:"Height"`
	Blocks []struct {
		ParentStateRoot cidJSON `json:"ParentStateRoot"`
	} `json:"Blocks"`
}

type cidJSON struct {
	Slash string `json:"/"`
}

// CheckOnce performs one cross-check. Exposed for tests and for a
// dashboard "check now" action.
func (c *Checker) CheckOnce(ctx context.Context) {
	head := c.cfg.Source.Head()
	if head == nil || head.Height() <= c.depth {
		c.skip("no head / shallow chain")
		return
	}
	target := head.Height() - c.depth

	ours, err := c.cfg.Source.GetTipSetByHeight(target)
	if err != nil || ours == nil {
		c.skip("local tipset unavailable")
		return
	}
	if ours.Height() != target {
		// Null round at target locally; comparing across a null round
		// invites false positives. Skip this tick.
		c.skip("null round")
		return
	}

	params, _ := json.Marshal([]interface{}{target, nil})
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	raw, err := c.cfg.Bridge.RawJSONRPC(cctx, "Filecoin.ChainGetTipSetByHeight", params)
	cancel()
	if err != nil {
		c.skip("bridge unreachable: " + err.Error())
		return
	}
	var theirs bridgeTipSet
	if err := json.Unmarshal(raw, &theirs); err != nil {
		c.skip("bridge response parse: " + err.Error())
		return
	}
	if theirs.Height != int64(target) {
		// The bridge resolved a null round to an earlier tipset, or it
		// lags behind target. Either way: not comparable this tick.
		c.skip("bridge height mismatch (lag or null round)")
		return
	}

	c.checks.Add(1)
	c.lastEpoch.Store(int64(target))

	if tipsetKeysEqual(ours, &theirs) {
		c.agrees.Add(1)
		c.lastRes.Store("agree")
		return
	}

	// Confirmed divergence at settled depth: our canonical tipset and
	// the operator's full node disagree. Loud, but observe-only.
	c.diverges.Add(1)
	c.lastRes.Store("DIVERGE")
	oursStr := fmt.Sprintf("%v", ours.Cids())
	theirsStr := fmt.Sprintf("%v", theirs.Cids)
	log.Errorw("VM-BRIDGE CROSS-CHECK DIVERGENCE: local canonical tipset disagrees with bridge node",
		"epoch", int64(target), "local", oursStr, "bridge", theirsStr,
		"provenance", c.cfg.Bridge.Provenance())
	if c.cfg.OnDiverge != nil {
		c.cfg.OnDiverge(target, oursStr, theirsStr)
	}
}

func (c *Checker) skip(reason string) {
	c.skipped.Add(1)
	c.lastRes.Store("skip")
	log.Debugw("cross-check skipped", "reason", reason)
}

// tipsetKeysEqual compares our tipset's block CIDs with the bridge's,
// order-insensitively (both sides canonicalize by ticket, but a set
// compare is robust and cheap at <=15 blocks).
func tipsetKeysEqual(ours *ltypes.TipSet, theirs *bridgeTipSet) bool {
	oc := ours.Cids()
	if len(oc) != len(theirs.Cids) {
		return false
	}
	set := make(map[string]struct{}, len(oc))
	for _, c := range oc {
		set[c.String()] = struct{}{}
	}
	for _, tc := range theirs.Cids {
		if _, ok := set[tc.Slash]; !ok {
			return false
		}
	}
	return true
}

// Stats returns a snapshot of counters.
func (c *Checker) Stats() Stats {
	res, _ := c.lastRes.Load().(string)
	return Stats{
		Checks:           c.checks.Load(),
		Agrees:           c.agrees.Load(),
		Diverges:         c.diverges.Load(),
		Skipped:          c.skipped.Load(),
		LastCheckedEpoch: abi.ChainEpoch(c.lastEpoch.Load()),
		LastResult:       res,
		Provenance:       c.cfg.Bridge.Provenance(),
	}
}
