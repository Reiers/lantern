// Package cache is Lantern's persistent, bounded, CID-keyed block cache.
//
// The light node runs cache-first with an in-memory hamt.MemBlockStore:
// fast, but every process restart is cold, so a restarted node re-fetches
// its whole warm set (the PDP/payments/registry/USDFC contract subtrees)
// from the gateway/swarm on the first head advance. For a wallet that's
// fine. For a PDP node - which must prove/settle against warm contract
// state on a schedule and cannot afford a cold-start stall inside a
// proving window - it is not.
//
// This package backs the block cache with Badger (pure-Go, already a
// Lantern dependency via the header store), so the warm set SURVIVES
// restart. It is CID-keyed exactly like MemBlockStore, so the
// content-addressed trust property is unchanged: bytes are stored under
// their CID and verified against it. A peer can never poison this cache
// because insertion goes through PutVerify on the network path.
//
// Boundedness: a soft byte budget with sampled-LRU eviction plus a pin
// set (the warm contract subtrees a PDP node must never lose). This is
// the mid/PDP tier's 2-5 GB persistent footprint - deliberately bigger
// than the light node's memory cache, to cut the cold-fetch tail to zero
// in steady state.
//
// It satisfies combined.Cache (hamt.BlockGetter + Put) so it is a drop-in
// replacement for MemBlockStore in the daemon fetcher.
package cache

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v4"
	block "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
)

var log = logging.Logger("lantern/state-cache")

// DefaultSoftCapBytes is the default persistent-cache budget for the PDP
// tier: 3 GiB, the middle of the 2-5 GB target. Eviction keeps usage
// near this; pinned blocks are exempt.
const DefaultSoftCapBytes int64 = 3 << 30

// Key namespaces inside the single Badger DB.
var (
	blockPrefix = []byte("b/") // b/<cidbytes>      -> raw block
	metaPrefix  = []byte("m/") // m/<cidbytes>      -> meta (size, lastAccess, pinned)
)

// Store is a persistent, bounded, CID-keyed block cache.
type Store struct {
	db          *badger.DB
	softCap     int64
	sizeBytes   atomic.Int64 // approximate live (block) bytes
	mu          sync.Mutex   // guards eviction so only one runs at a time
	evicting    atomic.Bool
	closed      atomic.Bool
	hits        atomic.Uint64
	misses      atomic.Uint64
	puts        atomic.Uint64
	evictions   atomic.Uint64
	evictionSem chan struct{}
}

// Options configures the persistent cache.
type Options struct {
	// SoftCapBytes is the target on-disk block budget. 0 => default (3 GiB).
	// Eviction fires when live block bytes exceed this. Pinned blocks are
	// never evicted, so the effective floor is the pinned set size.
	SoftCapBytes int64
}

// Open opens (or creates) a persistent block cache at path.
func Open(path string, opts Options) (*Store, error) {
	bopts := badger.DefaultOptions(path).WithLogger(nil)
	// Blocks are small-to-medium IPLD nodes; keep value-log files modest
	// so eviction reclaims space without waiting for a huge GC cycle.
	bopts = bopts.WithValueLogFileSize(64 << 20)
	db, err := badger.Open(bopts)
	if err != nil {
		return nil, fmt.Errorf("open block cache badger at %s: %w", path, err)
	}
	s := &Store{
		db:          db,
		softCap:     opts.SoftCapBytes,
		evictionSem: make(chan struct{}, 1),
	}
	if s.softCap <= 0 {
		s.softCap = DefaultSoftCapBytes
	}
	// Recover the approximate live-bytes counter from persisted meta so the
	// soft cap is honored across restarts (not reset to 0 on every boot).
	if err := s.recomputeSize(); err != nil {
		log.Warnw("block cache: size recompute failed; starting from 0", "err", err)
	}
	log.Infow("persistent block cache open", "path", path, "soft_cap_bytes", s.softCap, "live_bytes", s.sizeBytes.Load())
	return s, nil
}

// Close flushes and closes the underlying Badger DB.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.db.Close()
}

// --- combined.Cache surface (hamt.BlockGetter + Put) ---

// Get returns the raw bytes for c, or an error if absent. Bumps the
// block's lastAccess for LRU. Implements hamt.BlockGetter.
func (s *Store) Get(_ context.Context, c cid.Cid) ([]byte, error) {
	var out []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(bkey(c))
		if err != nil {
			return err
		}
		out, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		s.misses.Add(1)
		return nil, fmt.Errorf("block not found: %s", c)
	}
	s.hits.Add(1)
	// Touch lastAccess asynchronously; never block a read on the LRU bump.
	go s.touch(c)
	return out, nil
}

// Put stores raw bytes under c and returns c (for chaining). It does NOT
// re-hash; the combined fetcher only calls Put after a CID-verified
// fetch, matching MemBlockStore.Put semantics. Use PutVerify to insert
// untrusted bytes.
func (s *Store) Put(c cid.Cid, raw []byte) cid.Cid {
	_ = s.put(c, raw, false)
	return c
}

// PutVerify recomputes c's hash over raw and inserts only on match.
func (s *Store) PutVerify(c cid.Cid, raw []byte) error {
	got, err := c.Prefix().Sum(raw)
	if err != nil {
		return fmt.Errorf("hashing block for CID verify: %w", err)
	}
	if !got.Equals(c) {
		return fmt.Errorf("CID mismatch: have %s, computed %s", c, got)
	}
	return s.put(c, raw, false)
}

// Has reports whether a block is present.
func (s *Store) Has(c cid.Cid) bool {
	err := s.db.View(func(txn *badger.Txn) error {
		_, e := txn.Get(bkey(c))
		return e
	})
	return err == nil
}

// GetBlock returns the block in go-block-format shape (for adapters).
func (s *Store) GetBlock(ctx context.Context, c cid.Cid) (block.Block, error) {
	raw, err := s.Get(ctx, c)
	if err != nil {
		return nil, err
	}
	return block.NewBlockWithCid(raw, c)
}

// --- pinning ---

// Pin marks c as un-evictable (the PDP warm set: contract subtrees a PDP
// node must never lose across restarts). Pinning a not-yet-present CID is
// a no-op that takes effect if/when the block is inserted.
func (s *Store) Pin(c cid.Cid) error   { return s.setPinned(c, true) }
func (s *Store) Unpin(c cid.Cid) error { return s.setPinned(c, false) }

// --- internals ---

type meta struct {
	size       int64
	lastAccess int64 // unix seconds
	pinned     bool
}

func bkey(c cid.Cid) []byte { return append(append([]byte{}, blockPrefix...), c.Bytes()...) }
func mkey(c cid.Cid) []byte { return append(append([]byte{}, metaPrefix...), c.Bytes()...) }

func encodeMeta(m meta) []byte {
	buf := make([]byte, 8+8+1)
	binary.BigEndian.PutUint64(buf[0:8], uint64(m.size))
	binary.BigEndian.PutUint64(buf[8:16], uint64(m.lastAccess))
	if m.pinned {
		buf[16] = 1
	}
	return buf
}

func decodeMeta(b []byte) (meta, bool) {
	if len(b) < 17 {
		return meta{}, false
	}
	return meta{
		size:       int64(binary.BigEndian.Uint64(b[0:8])),
		lastAccess: int64(binary.BigEndian.Uint64(b[8:16])),
		pinned:     b[16] == 1,
	}, true
}

func (s *Store) put(c cid.Cid, raw []byte, forcePin bool) error {
	if s.closed.Load() {
		return fmt.Errorf("block cache closed")
	}
	sz := int64(len(raw))
	err := s.db.Update(func(txn *badger.Txn) error {
		// Preserve pin flag if the block already existed.
		pinned := forcePin
		var prevSize int64
		existed := false
		if it, e := txn.Get(mkey(c)); e == nil {
			if b, e2 := it.ValueCopy(nil); e2 == nil {
				if m, ok := decodeMeta(b); ok {
					pinned = pinned || m.pinned
					prevSize = m.size
					existed = true
				}
			}
		}
		if err := txn.Set(bkey(c), raw); err != nil {
			return err
		}
		m := meta{size: sz, lastAccess: time.Now().Unix(), pinned: pinned}
		if err := txn.Set(mkey(c), encodeMeta(m)); err != nil {
			return err
		}
		if existed {
			s.sizeBytes.Add(sz - prevSize)
		} else {
			s.sizeBytes.Add(sz)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.puts.Add(1)
	// Trigger eviction opportunistically when over cap (non-blocking).
	if s.sizeBytes.Load() > s.softCap {
		go s.evictToTarget()
	}
	return nil
}

func (s *Store) touch(c cid.Cid) {
	if s.closed.Load() {
		return
	}
	_ = s.db.Update(func(txn *badger.Txn) error {
		it, err := txn.Get(mkey(c))
		if err != nil {
			return nil // no meta; block may have been evicted
		}
		b, err := it.ValueCopy(nil)
		if err != nil {
			return nil
		}
		m, ok := decodeMeta(b)
		if !ok {
			return nil
		}
		m.lastAccess = time.Now().Unix()
		return txn.Set(mkey(c), encodeMeta(m))
	})
}

func (s *Store) setPinned(c cid.Cid, pinned bool) error {
	return s.db.Update(func(txn *badger.Txn) error {
		var m meta
		if it, err := txn.Get(mkey(c)); err == nil {
			if b, e := it.ValueCopy(nil); e == nil {
				if dm, ok := decodeMeta(b); ok {
					m = dm
				}
			}
		}
		m.pinned = pinned
		if m.lastAccess == 0 {
			m.lastAccess = time.Now().Unix()
		}
		return txn.Set(mkey(c), encodeMeta(m))
	})
}

// evictToTarget is the opportunistic (background) eviction path: single-
// flight, a no-op if another eviction already holds the semaphore. Called
// via `go s.evictToTarget()` on over-cap Put.
func (s *Store) evictToTarget() {
	select {
	case s.evictionSem <- struct{}{}:
		defer func() { <-s.evictionSem }()
	default:
		return // another eviction in flight
	}
	s.evictLocked()
}

// evictNow runs a blocking eviction pass, waiting for the eviction lock so
// the caller is guaranteed the pass ran (used by the sync path + tests).
func (s *Store) evictNow() {
	s.evictionSem <- struct{}{}
	defer func() { <-s.evictionSem }()
	s.evictLocked()
}

// evictLocked evicts oldest-access unpinned blocks until live bytes drop to
// ~90% of the soft cap. Caller MUST hold the eviction semaphore. Sampled-
// LRU: scan meta, collect unpinned candidates with lastAccess, sort oldest
// first, delete until under target. The pinned set is the hard floor - if
// pins alone exceed the cap, we evict everything unpinned and stop.
func (s *Store) evictLocked() {
	if s.closed.Load() {
		return
	}
	target := s.softCap * 9 / 10
	if s.sizeBytes.Load() <= target {
		return
	}

	var cands []evictCand
	_ = s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = true
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(metaPrefix); it.ValidForPrefix(metaPrefix); it.Next() {
			item := it.Item()
			b, err := item.ValueCopy(nil)
			if err != nil {
				continue
			}
			m, ok := decodeMeta(b)
			if !ok || m.pinned {
				continue
			}
			mk := item.KeyCopy(nil)
			cidBytes := mk[len(metaPrefix):]
			bk := append(append([]byte{}, blockPrefix...), cidBytes...)
			cands = append(cands, evictCand{key: mk, bkeyBytes: bk, lastAccess: m.lastAccess, size: m.size})
		}
		return nil
	})

	// Oldest first (LRU).
	sortCandsByAccess(cands)

	freed := int64(0)
	for _, cd := range cands {
		if s.sizeBytes.Load() <= target {
			break
		}
		err := s.db.Update(func(txn *badger.Txn) error {
			if err := txn.Delete(cd.bkeyBytes); err != nil {
				return err
			}
			return txn.Delete(cd.key)
		})
		if err == nil {
			s.sizeBytes.Add(-cd.size)
			s.evictions.Add(1)
			freed += cd.size
		}
	}
	log.Debugw("block cache eviction pass", "freed_bytes", freed, "live_bytes", s.sizeBytes.Load(), "target", target)
}

// evictCand is one LRU eviction candidate (an unpinned block).
type evictCand struct {
	key        []byte // meta key
	bkeyBytes  []byte // block key
	lastAccess int64
	size       int64
}

// sortCandsByAccess sorts eviction candidates oldest-access first (LRU).
func sortCandsByAccess(cands []evictCand) {
	sort.Slice(cands, func(i, j int) bool { return cands[i].lastAccess < cands[j].lastAccess })
}

// recomputeSize walks meta to reconstruct the live-bytes counter at Open.
func (s *Store) recomputeSize() error {
	var total int64
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = true
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(metaPrefix); it.ValidForPrefix(metaPrefix); it.Next() {
			b, err := it.Item().ValueCopy(nil)
			if err != nil {
				continue
			}
			if m, ok := decodeMeta(b); ok {
				total += m.size
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.sizeBytes.Store(total)
	return nil
}

// Stats is an observability snapshot of the cache.
type Stats struct {
	LiveBytes    int64
	SoftCapBytes int64
	Hits         uint64
	Misses       uint64
	Puts         uint64
	Evictions    uint64
}

// Stats returns a snapshot of cache counters.
func (s *Store) Stats() Stats {
	return Stats{
		LiveBytes:    s.sizeBytes.Load(),
		SoftCapBytes: s.softCap,
		Hits:         s.hits.Load(),
		Misses:       s.misses.Load(),
		Puts:         s.puts.Load(),
		Evictions:    s.evictions.Load(),
	}
}
