// Package store implements Lantern's persistent header store.
//
// The store is BadgerDB-backed. It keys block headers by their CID (canonical
// content-addressed lookup) and maintains:
//
//   - Per-epoch indexes:           h:epoch:<be8>            → []byte (set of header CIDs)
//   - Per-CID header blobs:        h:cid:<bytes>            → CBOR-encoded BlockHeader
//   - Canonical tipset per epoch:  h:can:<be8>              → tipset-key bytes (sorted CIDs)
//   - Head epoch:                  h:head                   → be8
//
// "Canonical" means: the tipset key currently selected as part of the active
// chain. On reorg we rewrite the canonical pointers for the affected epochs.
//
// The store cooperates with chain/header (per-block validation) and with a
// caller-provided beacon Verifier for BLS-signature replay on startup.
//
// Written for Lantern. Not a direct lift; structural inspiration from
// lotus/chain/store but pure pure-Go and header-only.
package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"

	"github.com/Reiers/lantern/chain/beacon"
	"github.com/Reiers/lantern/chain/header"
	ltypes "github.com/Reiers/lantern/chain/types"
)

var log = logging.Logger("lantern/header/store")

// Key prefixes.
const (
	prefixCID   = "h:cid:"   // h:cid:<binary-cid> → header bytes
	prefixEpoch = "h:epoch:" // h:epoch:<be8>      → []cid... (concatenated bytes)
	prefixCanon = "h:can:"   // h:can:<be8>        → tipset-key bytes
	keyHead     = "h:head"   // → be8 epoch
)

// ErrNotFound is returned when a header or tipset is missing.
var ErrNotFound = errors.New("header/store: not found")

// Store is a thread-safe persistent header store.
type Store struct {
	db *badger.DB

	beacon *beacon.Config // optional, for startup re-verification

	mu       sync.RWMutex
	headTS   *ltypes.TipSet
	listener []func(*ltypes.TipSet)
}

// Options configures Open.
type Options struct {
	// BeaconConfig is an optional DRAND verifier used at startup to
	// re-validate beacon entries on the most-recent N headers before
	// accepting the persisted head as current.
	BeaconConfig *beacon.Config
	// StartupVerifyDepth: how many recent headers to re-verify on Open.
	// 0 means "no verification" (e.g. unit tests).
	StartupVerifyDepth int
}

// Open opens (or creates) a BadgerDB-backed header store at the given path.
// Pass path="" to open an in-memory store (useful for tests).
func Open(path string, opts Options) (*Store, error) {
	bopts := badger.DefaultOptions(path).WithLogger(nil)
	if path == "" {
		bopts = bopts.WithInMemory(true)
	}
	db, err := badger.Open(bopts)
	if err != nil {
		return nil, fmt.Errorf("open badger: %w", err)
	}
	s := &Store{db: db, beacon: opts.BeaconConfig}
	// Rehydrate the head pointer.
	if err := s.rehydrateHead(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rehydrate head: %w", err)
	}
	if opts.StartupVerifyDepth > 0 && s.headTS != nil {
		if err := s.startupVerify(opts.StartupVerifyDepth); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("startup verify: %w", err)
		}
	}
	return s, nil
}

// Close closes the underlying BadgerDB.
func (s *Store) Close() error {
	return s.db.Close()
}

func be8(u uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], u)
	return b[:]
}
func unbe8(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func epochKey(prefix string, ep abi.ChainEpoch) []byte {
	k := make([]byte, 0, len(prefix)+8)
	k = append(k, prefix...)
	k = append(k, be8(uint64(ep))...)
	return k
}

func cidKey(c cid.Cid) []byte {
	cb := c.Bytes()
	k := make([]byte, 0, len(prefixCID)+len(cb))
	k = append(k, prefixCID...)
	k = append(k, cb...)
	return k
}

// Put validates and persists a single BlockHeader. The caller is expected to
// have an established parent linkage; we verify that the header's declared
// parent CIDs exist in the store (if Height > 0).
func (s *Store) Put(bh *ltypes.BlockHeader) error {
	if bh == nil {
		return errors.New("header/store: nil header")
	}
	c := bh.Cid()
	raw, err := bh.Serialize()
	if err != nil {
		return fmt.Errorf("serialize header: %w", err)
	}
	if bh.Height > 0 {
		// Verify parent linkage: every Parents CID must exist in the
		// store (we don't enforce a single canonical parent tipset here
		// because we may be ingesting a competing fork during reorg
		// recovery).
		for _, p := range bh.Parents {
			has, err := s.hasCID(p)
			if err != nil {
				return err
			}
			if !has {
				return fmt.Errorf("parent %s of header %s not in store", p, c)
			}
		}
	}
	return s.db.Update(func(txn *badger.Txn) error {
		// Write CID → header bytes.
		if err := txn.Set(cidKey(c), raw); err != nil {
			return err
		}
		// Append to the per-epoch index.
		ek := epochKey(prefixEpoch, bh.Height)
		cur, err := getOrEmpty(txn, ek)
		if err != nil {
			return err
		}
		merged, changed := appendCIDIfMissing(cur, c)
		if changed {
			if err := txn.Set(ek, merged); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) hasCID(c cid.Cid) (bool, error) {
	err := s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(cidKey(c))
		return err
	})
	if err == nil {
		return true, nil
	}
	if errors.Is(err, badger.ErrKeyNotFound) {
		return false, nil
	}
	return false, err
}

// Get returns the block header for the given CID.
func (s *Store) Get(c cid.Cid) (*ltypes.BlockHeader, error) {
	var bh *ltypes.BlockHeader
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(cidKey(c))
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return ErrNotFound
			}
			return err
		}
		return item.Value(func(val []byte) error {
			h, derr := ltypes.DecodeBlock(val)
			if derr != nil {
				return derr
			}
			bh = h
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return bh, nil
}

// GetTipSetByHeight returns the canonical tipset at the given epoch.
// Walks backward for null-round epochs (returns the nearest canonical
// tipset with height <= epoch).
func (s *Store) GetTipSetByHeight(epoch abi.ChainEpoch) (*ltypes.TipSet, error) {
	for ep := epoch; ep >= 0; ep-- {
		ts, err := s.canonicalAt(ep)
		if err == nil && ts != nil {
			return ts, nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if ep == 0 {
			break
		}
	}
	return nil, ErrNotFound
}

func (s *Store) canonicalAt(epoch abi.ChainEpoch) (*ltypes.TipSet, error) {
	var keyBytes []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(epochKey(prefixCanon, epoch))
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return ErrNotFound
			}
			return err
		}
		return item.Value(func(val []byte) error {
			keyBytes = append([]byte(nil), val...)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	tsk, err := ltypes.TipSetKeyFromBytes(keyBytes)
	if err != nil {
		return nil, err
	}
	return s.loadTipSet(tsk)
}

// GetTipSet resolves a tipset directly by its key (the set of block
// CIDs). It reads each block header from the store and reassembles the
// tipset. Returns ErrNotFound if any constituent block is missing. Unlike
// GetTipSetByHeight this does NOT require the tipset to be on the
// canonical chain — it serves any tipset whose headers were persisted,
// which is what callers like ChainGetTipSet(key) need.
func (s *Store) GetTipSet(tsk ltypes.TipSetKey) (*ltypes.TipSet, error) {
	if tsk.IsEmpty() {
		return nil, ErrNotFound
	}
	return s.loadTipSet(tsk)
}

func (s *Store) loadTipSet(tsk ltypes.TipSetKey) (*ltypes.TipSet, error) {
	cids := tsk.Cids()
	blocks := make([]*ltypes.BlockHeader, 0, len(cids))
	for _, c := range cids {
		bh, err := s.Get(c)
		if err != nil {
			return nil, fmt.Errorf("missing block %s in canonical tipset: %w", c, err)
		}
		blocks = append(blocks, bh)
	}
	return ltypes.NewTipSet(blocks)
}

// Head returns the current canonical head tipset, or nil if the store is
// empty.
func (s *Store) Head() *ltypes.TipSet {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.headTS
}

// OnHeadChange registers a listener invoked whenever the canonical head
// advances. Listeners run synchronously in the goroutine that called
// SetHead — keep them fast.
func (s *Store) OnHeadChange(cb func(*ltypes.TipSet)) {
	s.mu.Lock()
	s.listener = append(s.listener, cb)
	s.mu.Unlock()
}

// SetHead marks ts as the canonical head, atomically rewiring canonical
// tipset pointers for every epoch from ts down to the most recent common
// ancestor with the previous head.
//
// On reorg (new head's parent chain diverges from current head's), all
// canonical-tipset pointers between the divergence epoch and the new head
// are overwritten.
func (s *Store) SetHead(_ctx context.Context, ts *ltypes.TipSet) error {
	if ts == nil {
		return errors.New("header/store: nil tipset")
	}
	// Persist all of ts's blocks. Try Put first (strict parent linkage);
	// fall back to a lenient direct write if the parents aren't yet in
	// store (e.g. caller is doing a deep bootstrap where the parent
	// chain hasn't been backfilled).
	for _, b := range ts.Blocks() {
		if err := s.Put(b); err != nil {
			raw, serr := b.Serialize()
			if serr != nil {
				return err
			}
			if derr := s.db.Update(func(txn *badger.Txn) error {
				if err := txn.Set(cidKey(b.Cid()), raw); err != nil {
					return err
				}
				ek := epochKey(prefixEpoch, b.Height)
				cur, err := getOrEmpty(txn, ek)
				if err != nil {
					return err
				}
				merged, changed := appendCIDIfMissing(cur, b.Cid())
				if changed {
					return txn.Set(ek, merged)
				}
				return nil
			}); derr != nil {
				return derr
			}
		}
	}

	// Walk down from ts: at each epoch update canonical pointer until
	// reaching a tipset whose canonical key already matches (= LCA) or
	// height 0.
	chain := []*ltypes.TipSet{ts}
	cur := ts
	for cur.Height() > 0 {
		// Stop if our canonical at cur.Height() already matches.
		canon, err := s.canonicalAt(cur.Height())
		if err == nil && canon != nil && tipsetKeyEq(canon.Key(), cur.Key()) {
			break
		}
		// Walk to parent tipset by loading its blocks.
		parentCIDs := cur.Blocks()[0].Parents
		parentBlocks := make([]*ltypes.BlockHeader, 0, len(parentCIDs))
		anyMissing := false
		for _, pc := range parentCIDs {
			bh, err := s.Get(pc)
			if err != nil {
				anyMissing = true
				break
			}
			parentBlocks = append(parentBlocks, bh)
		}
		if anyMissing || len(parentBlocks) == 0 {
			break // can't walk further; we only rewire what we have.
		}
		pts, err := ltypes.NewTipSet(parentBlocks)
		if err != nil {
			return fmt.Errorf("rebuild parent tipset at height %d: %w", parentBlocks[0].Height, err)
		}
		chain = append(chain, pts)
		cur = pts
	}

	// Reverse chain so we write oldest → newest (deterministic order).
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	if err := s.db.Update(func(txn *badger.Txn) error {
		for _, t := range chain {
			if err := txn.Set(epochKey(prefixCanon, t.Height()), t.Key().Bytes()); err != nil {
				return err
			}
		}
		return txn.Set([]byte(keyHead), be8(uint64(ts.Height())))
	}); err != nil {
		return err
	}

	s.mu.Lock()
	s.headTS = ts
	listeners := make([]func(*ltypes.TipSet), len(s.listener))
	copy(listeners, s.listener)
	s.mu.Unlock()
	for _, cb := range listeners {
		cb(ts)
	}
	return nil
}

// Tipset returns the canonical tipset at exactly `epoch` (no walk-back).
// Returns ErrNotFound if there's no canonical tipset there (e.g. null round).
func (s *Store) Tipset(epoch abi.ChainEpoch) (*ltypes.TipSet, error) {
	return s.canonicalAt(epoch)
}

// HeadEpoch returns the persisted head epoch, or -1 if no head is set.
func (s *Store) HeadEpoch() abi.ChainEpoch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.headTS == nil {
		return -1
	}
	return s.headTS.Height()
}

// rehydrateHead reads the persisted head pointer at startup and loads the
// canonical tipset there.
func (s *Store) rehydrateHead() error {
	var headEp abi.ChainEpoch = -1
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(keyHead))
		if err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return nil
			}
			return err
		}
		return item.Value(func(val []byte) error {
			headEp = abi.ChainEpoch(unbe8(val))
			return nil
		})
	})
	if err != nil {
		return err
	}
	if headEp < 0 {
		return nil
	}
	ts, err := s.canonicalAt(headEp)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	s.headTS = ts
	return nil
}

// startupVerify walks back up to `depth` canonical tipsets and re-verifies
// each header's beacon entries against s.beacon. We deliberately don't
// fail-stop on the first miss; we record the failure and require >= half
// to verify (covers null rounds and quicknet-unchained gaps where the
// previous-signature is unavailable without state).
func (s *Store) startupVerify(depth int) error {
	if s.headTS == nil || s.beacon == nil {
		return nil
	}
	ep := s.headTS.Height()
	checked, ok := 0, 0
	for i := 0; i < depth && ep >= 0; i++ {
		ts, err := s.canonicalAt(ep)
		if err == nil && ts != nil {
			for _, b := range ts.Blocks() {
				checked++
				if len(b.BeaconEntries) == 0 {
					ok++
					continue
				}
				// Use unchained-friendly verification (no prevSig
				// needed for quicknet).
				if err := s.beacon.VerifyEntries(b.BeaconEntries, nil); err == nil {
					ok++
				} else {
					log.Warnf("startup verify: header %s at epoch %d beacon-verify failed: %v", b.Cid(), b.Height, err)
				}
			}
		}
		ep--
	}
	if checked > 0 && ok*2 < checked {
		return fmt.Errorf("startup re-verify: only %d/%d headers passed beacon check", ok, checked)
	}
	return nil
}

// ------- helpers -------

func getOrEmpty(txn *badger.Txn, k []byte) ([]byte, error) {
	item, err := txn.Get(k)
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var out []byte
	err = item.Value(func(val []byte) error {
		out = append([]byte(nil), val...)
		return nil
	})
	return out, err
}

// Per-epoch index format: concatenated raw CID bytes (length-prefixed varint).
// Use a simple uvarint length prefix.
func appendCIDIfMissing(existing []byte, c cid.Cid) ([]byte, bool) {
	cb := c.Bytes()
	// Check membership.
	for cur := existing; len(cur) > 0; {
		clen, n := binary.Uvarint(cur)
		if n <= 0 || int(clen) > len(cur)-n {
			break
		}
		if eq := byteEq(cur[n:n+int(clen)], cb); eq {
			return existing, false
		}
		cur = cur[n+int(clen):]
	}
	out := make([]byte, 0, len(existing)+binary.MaxVarintLen64+len(cb))
	out = append(out, existing...)
	var buf [binary.MaxVarintLen64]byte
	m := binary.PutUvarint(buf[:], uint64(len(cb)))
	out = append(out, buf[:m]...)
	out = append(out, cb...)
	return out, true
}

func parseCIDIndex(b []byte) []cid.Cid {
	var out []cid.Cid
	for cur := b; len(cur) > 0; {
		clen, n := binary.Uvarint(cur)
		if n <= 0 || int(clen) > len(cur)-n {
			break
		}
		c, err := cid.Cast(cur[n : n+int(clen)])
		if err == nil {
			out = append(out, c)
		}
		cur = cur[n+int(clen):]
	}
	// Stable sort by raw bytes.
	sort.Slice(out, func(i, j int) bool { return out[i].KeyString() < out[j].KeyString() })
	return out
}

func byteEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tipsetKeyEq(a, b ltypes.TipSetKey) bool {
	return a == b
}

// AllHeadersAtEpoch returns every header we've seen at the given epoch
// (canonical + competing forks). Useful for reorg debugging.
func (s *Store) AllHeadersAtEpoch(epoch abi.ChainEpoch) ([]*ltypes.BlockHeader, error) {
	var idx []byte
	err := s.db.View(func(txn *badger.Txn) error {
		var ierr error
		idx, ierr = getOrEmpty(txn, epochKey(prefixEpoch, epoch))
		return ierr
	})
	if err != nil {
		return nil, err
	}
	cids := parseCIDIndex(idx)
	out := make([]*ltypes.BlockHeader, 0, len(cids))
	for _, c := range cids {
		bh, err := s.Get(c)
		if err != nil {
			continue
		}
		out = append(out, bh)
	}
	return out, nil
}

// Stats returns simple counters for diagnostics.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{}
	if s.headTS != nil {
		st.HeadEpoch = s.headTS.Height()
	} else {
		st.HeadEpoch = -1
	}
	st.OpenedAt = time.Now()
	return st
}

// Stats captures store-level metrics.
type Stats struct {
	HeadEpoch abi.ChainEpoch
	OpenedAt  time.Time
}

// Compile-time check: header.ValidateHeader is the canonical per-header
// validator; the store relies on the caller to invoke it before Put.
var _ = header.ValidateHeader
