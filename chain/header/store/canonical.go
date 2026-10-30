package store

import (
	"errors"
	"fmt"

	"github.com/dgraph-io/badger/v4"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// HasBlock reports whether the header with CID c is persisted.
func (s *Store) HasBlock(c cid.Cid) bool {
	ok, err := s.hasCID(c)
	return err == nil && ok
}

// canonicalBatch bounds the number of canonical-pointer writes per Badger
// transaction in SetCanonicalSegment, well under Badger's txn limits.
const canonicalBatch = 1000

// SetCanonicalSegment records canonical-tipset pointers for a contiguous,
// parent-linked run of already-persisted tipsets that hangs off the
// canonical chain (header backfill below the store's coverage).
//
// chain must be ordered newest first, and chain[i+1] must be the parent
// tipset of chain[i] (chain[i].Parents() == chain[i+1].Key()); the run is
// rejected otherwise. Only epochs <= maxHeight are touched, so callers can
// keep the write strictly below finality and never race head adoption or
// reorg handling (rewireHead owns everything above). Epochs strictly
// between two consecutive tipsets are null rounds on this chain: any stale
// canonical pointer there is deleted.
//
// The head pointer and listeners are never touched. Returns how many
// pointers were written or deleted.
func (s *Store) SetCanonicalSegment(chain []*ltypes.TipSet, maxHeight abi.ChainEpoch) (int, error) {
	for i := range chain {
		if chain[i] == nil {
			return 0, errors.New("header/store: nil tipset in canonical segment")
		}
	}
	for i := 0; i+1 < len(chain); i++ {
		if chain[i].Parents() != chain[i+1].Key() || chain[i+1].Height() >= chain[i].Height() {
			return 0, fmt.Errorf("header/store: canonical segment not parent-linked at height %d", chain[i].Height())
		}
	}
	type op struct {
		ep  abi.ChainEpoch
		key []byte // nil = delete
	}
	var ops []op
	for i, ts := range chain {
		h := ts.Height()
		if h <= maxHeight {
			cur, err := s.canonicalKey(h)
			if err != nil {
				return 0, err
			}
			want := ts.Key().Bytes()
			if cur == nil || !byteEq(cur, want) {
				ops = append(ops, op{ep: h, key: want})
			}
		}
		if i+1 < len(chain) {
			for ep := chain[i+1].Height() + 1; ep < h && ep <= maxHeight; ep++ {
				cur, err := s.canonicalKey(ep)
				if err != nil {
					return 0, err
				}
				if cur != nil {
					ops = append(ops, op{ep: ep})
				}
			}
		}
	}
	for start := 0; start < len(ops); start += canonicalBatch {
		end := start + canonicalBatch
		if end > len(ops) {
			end = len(ops)
		}
		if err := s.db.Update(func(txn *badger.Txn) error {
			for _, o := range ops[start:end] {
				k := epochKey(prefixCanon, o.ep)
				if o.key == nil {
					if err := txn.Delete(k); err != nil {
						return err
					}
					continue
				}
				if err := txn.Set(k, o.key); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return start, err
		}
	}
	return len(ops), nil
}

// CanonicalLink returns the canonical tipset key at epoch and the parent
// key its blocks reference, decoding a single header. ok=false when there
// is no canonical pointer at epoch (null round or not covered). Lets a
// caller verify that a run of canonical pointers is parent-linked without
// loading whole tipsets.
func (s *Store) CanonicalLink(epoch abi.ChainEpoch) (key, parents ltypes.TipSetKey, ok bool, err error) {
	raw, err := s.canonicalKey(epoch)
	if err != nil || raw == nil {
		return key, parents, false, err
	}
	key, err = ltypes.TipSetKeyFromBytes(raw)
	if err != nil {
		return key, parents, false, err
	}
	cids := key.Cids()
	if len(cids) == 0 {
		return key, parents, false, fmt.Errorf("header/store: empty canonical key at %d", epoch)
	}
	bh, err := s.Get(cids[0])
	if err != nil {
		return key, parents, false, err
	}
	return key, ltypes.NewTipSetKey(bh.Parents...), true, nil
}

// canonicalKey returns the raw canonical tipset-key bytes at epoch, or nil
// when there is no pointer.
func (s *Store) canonicalKey(epoch abi.ChainEpoch) ([]byte, error) {
	var out []byte
	err := s.db.View(func(txn *badger.Txn) error {
		v, err := getOrEmpty(txn, epochKey(prefixCanon, epoch))
		out = v
		return err
	})
	return out, err
}
