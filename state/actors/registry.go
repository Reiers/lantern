// Registry maps actor Code CIDs to (actor type, actor-version) tuples.
//
// Filecoin upgrades the on-chain "actor bundle" at each network-version
// boundary. Each release publishes a Manifest pinning code CIDs per
// builtin actor (account, cron, init, miner, market, power, ...). To
// decode an actor's Head state, Lantern needs to pick the right
// go-state-types/builtin/vN/<actor> package for that code CID.
//
// We pre-compute a flat code-CID → (kind, version) table from the
// canonical Lotus-shipped Bundles. The table is small (~12 actors ×
// ~11 versions × 2 networks ≈ 250 entries) and lookups are O(1).
//
// Provenance: the source data is `lotus@v1.36.0
// build/builtin_actors_gen.go`. Lotus carries dual Apache-2.0 / MIT
// license; see LICENSE-LOTUS{,-APACHE,-MIT}. The Manifest data itself is
// reproducible by computing the IPLD CID of each release of
// `github.com/filecoin-project/builtin-actors`.

package actors

import (
	"fmt"

	"github.com/ipfs/go-cid"
)

// Kind enumerates the canonical actor "name" keys used by Filecoin's
// builtin-actors manifest.
type Kind string

const (
	KindAccount     Kind = "account"
	KindCron        Kind = "cron"
	KindInit        Kind = "init"
	KindMarket      Kind = "storagemarket"
	KindMiner       Kind = "storageminer"
	KindMultisig    Kind = "multisig"
	KindPaych       Kind = "paymentchannel"
	KindPower       Kind = "storagepower"
	KindReward      Kind = "reward"
	KindSystem      Kind = "system"
	KindVerifreg    Kind = "verifiedregistry"
	KindDatacap     Kind = "datacap"
	KindEvm         Kind = "evm"
	KindEam         Kind = "eam"
	KindPlaceholder Kind = "placeholder"
	KindEthAccount  Kind = "ethaccount"
)

// CodeInfo identifies one actor code CID.
type CodeInfo struct {
	Kind    Kind
	Version int    // actors version (8..18)
	Network string // "mainnet" or "calibrationnet"
}

// Registry resolves a code CID to its (kind, version, network) tuple.
// Built once at process start from the Bundles table.
type Registry struct {
	byCode map[cid.Cid]CodeInfo
}

// DefaultRegistry returns a Registry populated from every shipped Bundle.
func DefaultRegistry() *Registry {
	r := &Registry{byCode: make(map[cid.Cid]CodeInfo, 256)}
	for _, b := range Bundles {
		for kind, code := range b.Actors {
			// First-seen wins. If two networks share a code CID for a
			// given (kind, version) — common for shared system actors —
			// we still resolve correctly because the kind and version
			// are identical. Network is reported as the first one seen
			// which is a reasonable default; callers needing the
			// per-network manifest CID can iterate Bundles directly.
			if existing, ok := r.byCode[code]; ok {
				// Sanity: same kind+version, different network → fine.
				if existing.Kind != Kind(kind) || existing.Version != b.Version {
					// Two different bundles claim the same code CID. This
					// shouldn't happen, but if it does we keep the first.
					continue
				}
				continue
			}
			r.byCode[code] = CodeInfo{
				Kind:    Kind(kind),
				Version: b.Version,
				Network: b.Network,
			}
		}
	}
	return r
}

// Lookup returns the CodeInfo for a given code CID, or false if unknown.
func (r *Registry) Lookup(code cid.Cid) (CodeInfo, bool) {
	ci, ok := r.byCode[code]
	return ci, ok
}

// MustLookup returns the CodeInfo or panics with a descriptive error.
// Use only for tests / debug paths.
func (r *Registry) MustLookup(code cid.Cid) CodeInfo {
	ci, ok := r.Lookup(code)
	if !ok {
		panic(fmt.Sprintf("actors: unknown code CID %s", code))
	}
	return ci
}

// ErrUnknownCode is returned when a code CID is not in the table.
type ErrUnknownCode struct{ Code cid.Cid }

func (e ErrUnknownCode) Error() string {
	return fmt.Sprintf("actors: unknown code CID %s (not in Lotus@v1.36 bundle table)", e.Code)
}
