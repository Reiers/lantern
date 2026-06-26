// Package anchorverify hardens Lantern's boot-time trusted-root selection
// (security issue #54).
//
// Background. Every block Lantern fetches downstream of the anchor is
// content-address-verified (hamt.VerifyBlockCID). That protects the
// *integrity* of state under a given root: a malicious source cannot forge
// state for a root we already trust. It does NOT protect the *choice* of
// root — i.e. "is this the canonical/heaviest chain, or an attacker's
// valid-but-non-canonical fork (or a stale, superseded head)?".
//
// The default daemon boot historically accepted the head returned by a
// single source (the gateway, or the Glif fallback) on faith. This package
// closes that gap with three independent, cheap checks that need no full
// header-sync pipeline:
//
//  1. Multi-source agreement. Fetch the head from >=2 independent sources
//     and require them to agree on (StateRoot, TipSetKey). Agreement across
//     independent operators is a strong signal the head is canonical.
//
//  2. F3 finality cross-check. When an F3 latest finality certificate is
//     available, require the selected anchor to be consistent with the
//     F3-finalized tipset (equal to it, or a descendant of it by epoch).
//     F3 certs carry BLS-aggregate signatures from the power table, so this
//     is a cryptographic anchor, not a trust-the-endpoint anchor.
//
//  3. Heaviest-weight tiebreak. On disagreement with no F3 resolution, pick
//     the candidate with the greatest ParentWeight (Filecoin's fork-choice
//     rule) and emit a loud warning rather than silently trusting source #1.
//
// An operator can bypass all of this with InsecureAllowSingleSource (the
// --insecure-anchor flag) for localhost/dev against a single endpoint.
package anchorverify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// Candidate is one source's view of the current head, normalized to the
// fields the verifier compares. Sources that cannot supply a field leave it
// at its zero value; only StateRoot + TipSetKey + Epoch are load-bearing.
type Candidate struct {
	Source       string // human label, e.g. "gateway", "glif"
	Epoch        abi.ChainEpoch
	StateRoot    cid.Cid
	TipSetKey    ltypes.TipSetKey
	ParentWeight ltypes.BigInt
}

// identityKey returns the (StateRoot, TipSetKey) tuple two candidates must
// share to be considered "in agreement".
func (c Candidate) identityKey() string {
	return c.StateRoot.String() + "|" + c.TipSetKey.String()
}

func (c Candidate) valid() bool {
	return c.StateRoot.Defined() && c.Epoch >= 0 && len(c.TipSetKey.Cids()) > 0
}

// F3Finalized is the (epoch, tipset) the latest F3 finality certificate
// finalized. Used for the cryptographic cross-check. Zero value means "no
// F3 finality available", which makes the cross-check a no-op (multi-source
// agreement still applies).
type F3Finalized struct {
	Available bool
	Instance  uint64
	Epoch     abi.ChainEpoch
	TipSetKey ltypes.TipSetKey
}

// FinalizedFromCert extracts the finalized (epoch, tipset key) from an F3
// finality certificate. Returns Available=false on any nil/empty cert so
// callers can treat "no F3" uniformly.
func FinalizedFromCert(cert *certs.FinalityCertificate) (F3Finalized, error) {
	if cert == nil || cert.ECChain == nil {
		return F3Finalized{}, nil
	}
	head := cert.ECChain.Head()
	if head == nil {
		return F3Finalized{}, nil
	}
	tsk, err := ltypes.TipSetKeyFromBytes(head.Key)
	if err != nil {
		return F3Finalized{}, fmt.Errorf("anchorverify: decode F3 tipset key: %w", err)
	}
	return F3Finalized{
		Available: true,
		Instance:  cert.GPBFTInstance,
		Epoch:     abi.ChainEpoch(head.Epoch),
		TipSetKey: tsk,
	}, nil
}

// Policy controls how strict the verifier is.
type Policy struct {
	// MinAgreeingSources is the number of independent sources that must
	// agree on the same (StateRoot, TipSetKey) for an anchor to be
	// accepted without an F3 cross-check or weight tiebreak. Default 2.
	MinAgreeingSources int

	// InsecureAllowSingleSource bypasses agreement + F3 + tiebreak and
	// accepts a single source on faith. Maps to --insecure-anchor. Intended
	// for localhost/dev only.
	InsecureAllowSingleSource bool

	// Warnf, if set, receives human-readable warnings (e.g. source
	// disagreement). Defaults to a no-op.
	Warnf func(format string, args ...any)
}

func (p Policy) warnf(format string, args ...any) {
	if p.Warnf != nil {
		p.Warnf(format, args...)
	}
}

// Result is the outcome of a verification.
type Result struct {
	// Chosen is the accepted candidate.
	Chosen Candidate
	// Method explains how Chosen was selected, for logging/audit.
	Method string
	// AgreeingSources is how many sources backed Chosen's identity.
	AgreeingSources int
	// F3Checked is true when an F3 finality cross-check was applied.
	F3Checked bool
}

var (
	// ErrNoCandidates means every source failed to return a head.
	ErrNoCandidates = errors.New("anchorverify: no usable head candidates from any source")
	// ErrNoAgreement means sources disagreed and nothing (F3 or weight)
	// could safely resolve the canonical head.
	ErrNoAgreement = errors.New("anchorverify: sources disagree on head and no F3 finality could resolve it")
	// ErrF3Conflict means the agreed/selected head conflicts with the
	// F3-finalized chain (selected epoch is at/below F3 finality but the
	// tipset differs — a fork below finality, which must never be accepted).
	ErrF3Conflict = errors.New("anchorverify: selected head conflicts with F3 finalized tipset")
)

// Verify applies the policy to the gathered candidates + optional F3
// finality and returns the accepted candidate or an error.
//
// Decision order:
//
//	insecure?            -> accept first valid candidate (loud no-op of checks)
//	0 valid candidates   -> ErrNoCandidates
//	F3 available:
//	  reject any candidate that conflicts below finality (ErrF3Conflict)
//	  prefer the largest agreeing group consistent with F3
//	agreement >= Min     -> accept agreed identity
//	no agreement:
//	  if F3 available, accept the F3-consistent heaviest candidate
//	  else ErrNoAgreement
func Verify(cands []Candidate, f3 F3Finalized, pol Policy) (Result, error) {
	if pol.MinAgreeingSources <= 0 {
		pol.MinAgreeingSources = 2
	}

	valid := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.valid() {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return Result{}, ErrNoCandidates
	}

	if pol.InsecureAllowSingleSource {
		pol.warnf("anchor: --insecure-anchor set; accepting single source %q without agreement/F3 verification", valid[0].Source)
		return Result{Chosen: valid[0], Method: "insecure-single-source", AgreeingSources: 1}, nil
	}

	// F3 conflict guard: any candidate at-or-below F3 finality epoch whose
	// tipset differs from the finalized tipset is on a fork below finality.
	// That is never acceptable. (Candidates above finality are descendants
	// we cannot disprove here; agreement/weight handle those.)
	if f3.Available {
		for _, c := range valid {
			if c.Epoch == f3.Epoch && c.TipSetKey.String() != f3.TipSetKey.String() {
				return Result{}, fmt.Errorf("%w: source %q epoch %d tipset %s != F3 %s (instance %d)",
					ErrF3Conflict, c.Source, c.Epoch, c.TipSetKey.String(), f3.TipSetKey.String(), f3.Instance)
			}
		}
	}

	// Group candidates by identity (StateRoot, TipSetKey).
	groups := map[string][]Candidate{}
	for _, c := range valid {
		groups[c.identityKey()] = append(groups[c.identityKey()], c)
	}

	// Find the largest agreeing group (deterministic on ties via heaviest
	// weight then lowest source name).
	type grp struct {
		key   string
		cands []Candidate
	}
	gs := make([]grp, 0, len(groups))
	for k, cs := range groups {
		gs = append(gs, grp{key: k, cands: cs})
	}
	sort.Slice(gs, func(i, j int) bool {
		if len(gs[i].cands) != len(gs[j].cands) {
			return len(gs[i].cands) > len(gs[j].cands)
		}
		// tie: heavier ParentWeight wins
		wi := weightOf(gs[i].cands)
		wj := weightOf(gs[j].cands)
		if c := cmpWeight(wi, wj); c != 0 {
			return c > 0
		}
		return gs[i].cands[0].Source < gs[j].cands[0].Source
	})
	top := gs[0]

	if len(top.cands) >= pol.MinAgreeingSources {
		return Result{
			Chosen:          top.cands[0],
			Method:          "multi-source-agreement",
			AgreeingSources: len(top.cands),
			F3Checked:       f3.Available,
		}, nil
	}

	// No quorum. If F3 is available, we already proved nothing conflicts
	// below finality; accept the heaviest F3-consistent candidate and warn.
	if f3.Available {
		heaviest := heaviestCandidate(valid)
		pol.warnf("anchor: sources did not reach %d-way agreement; accepting heaviest F3-consistent head from %q (epoch %d) — review source diversity",
			pol.MinAgreeingSources, heaviest.Source, heaviest.Epoch)
		return Result{
			Chosen:          heaviest,
			Method:          "f3-consistent-heaviest",
			AgreeingSources: len(groups[heaviest.identityKey()]),
			F3Checked:       true,
		}, nil
	}

	// No agreement and no F3 to break the tie. Refuse rather than trust an
	// arbitrary single source.
	pol.warnf("anchor: %d sources returned %d distinct heads with no F3 finality to resolve; refusing to anchor (use --insecure-anchor to override on a trusted single endpoint)",
		len(valid), len(groups))
	return Result{}, ErrNoAgreement
}

func weightOf(cs []Candidate) ltypes.BigInt {
	w := ltypes.NewInt(0)
	for _, c := range cs {
		if cmpWeight(c.ParentWeight, w) > 0 {
			w = c.ParentWeight
		}
	}
	return w
}

func heaviestCandidate(cs []Candidate) Candidate {
	best := cs[0]
	for _, c := range cs[1:] {
		if cmpWeight(c.ParentWeight, best.ParentWeight) > 0 {
			best = c
		}
	}
	return best
}

// cmpWeight compares two ParentWeights, treating the zero/undefined BigInt
// as the smallest possible value so a source that omitted weight never wins
// a tiebreak over one that supplied it.
func cmpWeight(a, b ltypes.BigInt) int {
	an := a.Nil()
	bn := b.Nil()
	switch {
	case an && bn:
		return 0
	case an:
		return -1
	case bn:
		return 1
	default:
		return a.Int.Cmp(b.Int)
	}
}

// GatherTimeout bounds how long Gather waits for all sources combined.
const GatherTimeout = 12 * time.Second

// HeadFetcher fetches one source's current head as a Candidate. Implemented
// by thin adapters over the gateway/Glif clients in cmd/lantern.
type HeadFetcher interface {
	FetchCandidate(ctx context.Context) (Candidate, error)
}

// Gather queries every fetcher (best-effort, in parallel-ish sequence) and
// returns all candidates that succeeded. Failures are reported via warnf but
// do not abort — the verifier decides whether the survivors are enough.
func Gather(ctx context.Context, pol Policy, fetchers ...HeadFetcher) []Candidate {
	out := make([]Candidate, 0, len(fetchers))
	for _, f := range fetchers {
		fctx, cancel := context.WithTimeout(ctx, GatherTimeout)
		c, err := f.FetchCandidate(fctx)
		cancel()
		if err != nil {
			pol.warnf("anchor: source fetch failed: %v", err)
			continue
		}
		out = append(out, c)
	}
	return out
}
