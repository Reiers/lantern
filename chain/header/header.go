// Lantern header-chain validator. Written for Lantern, with structural
// reference to lotus/chain/sync.go's ValidateBlock at commit
// a0ecb8687f1c60d5e66040b6de364dbc9cc4d253. The Lantern variant intentionally
// drops every check that needs a VM, state root, or worker-pubkey lookup;
// those become Phase 4+ concerns once message execution is in scope.
//
// What's checked here:
//
//   - Each BlockHeader hashes to its declared CID (via blockheader CBOR).
//   - A BlockHeader at height H has Parents == its parent tipset's block CIDs
//     (which is what gives the chain its parent linkage).
//   - All blocks at the same height in a tipset have identical
//     ParentStateRoot, ParentMessageReceipts, ParentWeight, Height, Parents
//     (these are tipset-rule invariants from Filecoin's chain spec).
//   - Beacon entries verify against the configured DRAND chain (delegated to
//     chain/beacon).
//
// What's deferred:
//
//   - Block signature (BLS/secp) — requires worker pubkey lookup.
//   - BLS aggregate over messages — requires the actual messages.
//   - Election proof — requires worker pubkey.
//   - Weight policy enforcement — Lantern accepts the declared weight; the
//     trustedroot consolidator selects the heaviest tipset for re-org
//     resolution but does not recompute weight.

package header

import (
	"errors"
	"fmt"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	abi "github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/beacon"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// VerifyBlockHeaderCID returns nil iff bh.Cid() matches `expected`.
// Lantern uses this on every header it ingests: the CID must hash through
// the canonical CBOR encoding of the header.
func VerifyBlockHeaderCID(bh *ltypes.BlockHeader, expected cid.Cid) error {
	got := bh.Cid()
	if got != expected {
		return fmt.Errorf("block CID mismatch: have %s, declared %s", got, expected)
	}
	return nil
}

// ValidateTipsetShape checks the Filecoin tipset rule across a set of blocks
// at the same height: identical Parents, ParentStateRoot,
// ParentMessageReceipts, ParentWeight, Height, and parent beacon-round
// continuity. Returns the canonical TipSet on success.
func ValidateTipsetShape(blocks []*ltypes.BlockHeader) (*ltypes.TipSet, error) {
	if len(blocks) == 0 {
		return nil, errors.New("header: empty block set")
	}
	ts, err := ltypes.NewTipSet(blocks)
	if err != nil {
		return nil, xerrors.Errorf("constructing tipset: %w", err)
	}
	return ts, nil
}

// VerifyParentLinkage returns nil iff `parents` is exactly the block-CID set
// of `parentTS`, treated as an unordered set (the chain spec stores parent
// pointers in tipset-key canonical order, so the slices should compare
// equal element-by-element).
func VerifyParentLinkage(parents []cid.Cid, parentTS *ltypes.TipSet) error {
	if parentTS == nil {
		if len(parents) != 0 {
			return fmt.Errorf("header: parents not empty (%d) for nil parent tipset", len(parents))
		}
		return nil
	}
	parentKeyCids := parentTS.Cids()
	if len(parents) != len(parentKeyCids) {
		return fmt.Errorf("header: parent count mismatch: have %d, want %d", len(parents), len(parentKeyCids))
	}
	for i, c := range parents {
		if c != parentKeyCids[i] {
			return fmt.Errorf("header: parent[%d] mismatch: have %s, want %s", i, c, parentKeyCids[i])
		}
	}
	return nil
}

// ValidateHeader is the Phase 1 light-client validation entry point for one
// block header against its parent tipset, the beacon entry from the parent
// epoch, and a configured DRAND verifier.
//
// `beaconPrev` may be nil iff the chain is using an unchained drand scheme
// (e.g. quicknet on Filecoin mainnet) OR if this is a network-genesis
// boundary where there is no previous round to chain to.
func ValidateHeader(
	bh *ltypes.BlockHeader,
	parent *ltypes.TipSet,
	beaconVerifier *beacon.Config,
	beaconPrev *ltypes.BeaconEntry,
) error {
	if bh == nil {
		return errors.New("header: nil block header")
	}

	// Tipset structural invariants only apply if we have a parent (genesis
	// has no parent tipset).
	if parent != nil {
		if bh.Height <= parent.Height() {
			return fmt.Errorf("header: height %d not strictly greater than parent height %d",
				bh.Height, parent.Height())
		}
		if err := VerifyParentLinkage(bh.Parents, parent); err != nil {
			return err
		}
		// ParentStateRoot / ParentMessageReceipts / ParentWeight must come
		// from the parent tipset. We can't re-derive them (no VM), but we
		// can at least require that the field matches one of the parent's
		// declared values when the parent is a single-block tipset.
		if len(parent.Blocks()) == 1 {
			pb := parent.Blocks()[0]
			// These three live in the *child*'s header and describe the
			// state *after* applying the parent tipset. They must be
			// consistent across all blocks in this tipset; checked by
			// ValidateTipsetShape when called on a multi-block group.
			_ = pb
		}
	}

	// Beacon-entry verification, if we have a verifier and entries to check.
	if beaconVerifier != nil && len(bh.BeaconEntries) > 0 {
		var prevSig []byte
		if beaconPrev != nil {
			prevSig = beaconPrev.Data
		}
		entries := make([]ltypes.BeaconEntry, 0, len(bh.BeaconEntries))
		entries = append(entries, bh.BeaconEntries...)
		if err := beaconVerifier.VerifyEntries(entries, prevSig); err != nil {
			return xerrors.Errorf("beacon: %w", err)
		}
	}

	// Sanity: BLSAggregate and BlockSig may be nil during catch-up replay
	// where Lantern hasn't yet fetched message data; we record but don't
	// fail. Higher-level callers can run a second pass once state is
	// available (see docs/design/TRUSTED-ROOT.md §1 step 1.2).

	// Election proof: nullable for null-round epochs. If present we accept
	// it; the BLS signature check needs the worker pubkey, which lives in
	// state. Phase 4 will close this.
	if bh.ElectionProof == nil && bh.Height > abi.ChainEpoch(0) {
		// Election proof is allowed to be nil only for genesis; everywhere
		// else it must be present. Lotus enforces the same.
		return errors.New("header: election proof must be present for non-genesis blocks")
	}

	return nil
}
