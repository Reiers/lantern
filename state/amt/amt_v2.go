// Legacy (v2) AMT support.
//
// Filecoin's block message AMTs (MsgMeta.BlsMessages / SecpkMessages) and the
// per-tipset ParentMessageReceipts AMT are encoded with the ORIGINAL
// go-amt-ipld v2 format: a 3-field root [Height, Count, Node] with an implicit
// fixed width of 8 (bitWidth 3) that is NOT stored in the root.
//
// FEVM contract-state AMTs and most modern actor sub-AMTs use the v4 format
// (4-field root [BitWidth, Height, Count, Node]). Loading a v2 root with the
// v4 loader fails with "cbor input had wrong number of fields" because the
// v4 internal.Root decoder expects 4 fields and the bytes carry 3.
//
// This file provides v2-format Lookup / ForEach over the same path-recording
// store the rest of state/amt uses, so message-search (chain/msgsearch) can
// resolve receipts locally with zero external trust.
package amt

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	amtv2 "github.com/filecoin-project/go-amt-ipld/v2"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/Reiers/lantern/state/hamt"
)

// LookupV2 reads the raw value bytes at index `i` from a legacy (v2) AMT
// rooted at `root`, returning the value bytes and the proof path (node CIDs
// fetched in order). Use this for block message AMTs and ParentMessageReceipts.
func LookupV2(ctx context.Context, root cid.Cid, i uint64, bg hamt.BlockGetter) ([]byte, []cid.Cid, error) {
	prs := newPathRecordingStore(bg)
	cstore := cbornode.NewCborStore(prs)

	r, err := amtv2.LoadAMT(ctx, cstore, root)
	if err != nil {
		return nil, prs.Path(), fmt.Errorf("loading v2 AMT root %s: %w", root, err)
	}

	var d cbg.Deferred
	if err := r.Get(ctx, i, &d); err != nil {
		// v2's Get returns an error (not a bool) when the index is absent.
		// Distinguish "not found" so callers can stop enumerating.
		if isV2NotFound(err) {
			return nil, prs.Path(), ErrNotFound
		}
		return nil, prs.Path(), fmt.Errorf("v2 AMT walk: %w", err)
	}
	if d.Raw == nil {
		return nil, prs.Path(), ErrNotFound
	}
	return d.Raw, prs.Path(), nil
}

// ForEachV2CIDs walks a legacy (v2) AMT-of-CIDs in index order, returning the
// CIDs. This is the canonical way to enumerate a block's message AMT (each
// leaf value is a CBOR-tagged CID).
func ForEachV2CIDs(ctx context.Context, root cid.Cid, bg hamt.BlockGetter) ([]cid.Cid, error) {
	if !root.Defined() {
		return nil, nil
	}
	prs := newPathRecordingStore(bg)
	cstore := cbornode.NewCborStore(prs)

	r, err := amtv2.LoadAMT(ctx, cstore, root)
	if err != nil {
		return nil, fmt.Errorf("loading v2 AMT root %s: %w", root, err)
	}

	var out []cid.Cid
	err = r.ForEach(ctx, func(_ uint64, d *cbg.Deferred) error {
		var cc cbg.CborCid
		if err := cc.UnmarshalCBOR(bytes.NewReader(d.Raw)); err != nil {
			return fmt.Errorf("decode AMT leaf CID: %w", err)
		}
		out = append(out, cid.Cid(cc))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// isV2NotFound reports whether a v2 AMT Get error means the index was absent
// rather than a real failure. v2 returns a typed *amtv2.ErrNotFound; match on
// the type so a genuine fetch error is never masked.
func isV2NotFound(err error) bool {
	if err == nil {
		return false
	}
	var nf *amtv2.ErrNotFound
	if errors.As(err, &nf) {
		return true
	}
	var nfv amtv2.ErrNotFound
	return errors.As(err, &nfv)
}
