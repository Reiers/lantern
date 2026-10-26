// Package ethtxindex is Lantern's persistent, bounded eth tx index.
//
// It maps an eth tx hash (keccak of the signed EIP-1559 envelope for
// delegated messages, the CID digest for native ones, exactly what Lotus
// reports) to where the message was included on the canonical chain:
// inclusion height, execution-order index, message CID, the logIndex base
// for its events, and a compact copy of its receipt. With it,
// eth_getTransactionReceipt / eth_getTransactionByHash are one local KV
// read plus a handful of block reads, instead of a backward chain walk
// that fetches every message of every tipset over the network.
//
// The index covers the last Window epochs. A per-height record (kept in
// memory and persisted) remembers which inclusion tipset was indexed at
// each height (or that the height was a null round) and which tx hashes
// it contributed, so:
//
//   - coverage ("is every height in [lo, hi] indexed?") is a map scan,
//     which lets a miss inside the window answer "not found" in
//     microseconds, Lotus-style;
//   - a reorg replaces the record at the affected heights and deletes the
//     tx entries the orphaned tipset contributed;
//   - pruning below the window deletes exactly the entries of the pruned
//     heights, no full scan.
//
// Records for locally published txs (eth_sendRawTransaction) are persisted
// too, so a restart no longer forgets what this node sent.
//
// Storage is any Backend (the header store's auxiliary namespace in
// production, see chain/header/store/aux.go).
package ethtxindex

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
)

// DefaultWindow is one day of epochs (PDP / Full tier default).
const DefaultWindow abi.ChainEpoch = 2880

// LightWindow is the light-tier default (about 8 hours).
const LightWindow abi.ChainEpoch = 960

// maxReturnBytes caps the receipt return payload copied into an entry.
// Larger returns are read from the receipts AMT at lookup time instead.
const maxReturnBytes = 256

// maxSent bounds persisted locally-published tx records.
const maxSent = 4096

// Backend is the persistence surface (satisfied by *hstore.Store).
type Backend interface {
	AuxGet(key []byte) ([]byte, bool, error)
	AuxWrite(puts []hstore.AuxPair, dels [][]byte) error
	AuxIterate(prefix []byte, fn func(k, v []byte) error) error
}

// Key layout (inside the backend's namespace).
var (
	pfxTx     = []byte("etx/t/") // + 32B hash          -> Loc
	pfxHeight = []byte("etx/h/") // + be8 height        -> height record
	pfxSent   = []byte("etx/s/") // + 32B hash          -> Sent
	pfxEmit   = []byte("etx/e/") // + be8 actor id      -> 20B eth address
)

const encVersion = 1

// Receipt is a compact copy of a message receipt.
type Receipt struct {
	ExitCode   int64
	GasUsed    int64
	Return     []byte
	EventsRoot cid.Cid // cid.Undef when the message emitted no events
}

// Loc is where a tx landed.
type Loc struct {
	Height abi.ChainEpoch // inclusion tipset height
	Idx    uint32         // execution-order index == receipt index
	IsSecp bool           // message came from a block's secp list
	MsgCID cid.Cid
	// EvBase is the number of events emitted by earlier messages of the
	// same tipset (the tx's first logIndex). Valid when HasEvBase.
	EvBase    uint32
	HasEvBase bool
	// Receipt is the compact receipt when it was captured at index time
	// (nil when the return payload was too large to inline).
	Receipt *Receipt
}

// HeightRecord is what was indexed at one height.
type HeightRecord struct {
	Height abi.ChainEpoch
	Null   bool            // null round: no tipset at this height
	Incl   types.TipSetKey // inclusion tipset (empty for null)
	Exec   types.TipSetKey // child whose ParentMessageReceipts hold the receipts
	Hashes [][32]byte      // tx hashes contributed (persisted only)
}

type heightMeta struct {
	null bool
	incl types.TipSetKey
	exec types.TipSetKey
}

// Sent is a locally published tx record.
type Sent struct {
	MsgCID    cid.Cid
	From      [20]byte
	SentEpoch abi.ChainEpoch
	Raw       []byte // signed RLP envelope as received by eth_sendRawTransaction
}

// Index is the persistent eth tx index. Safe for concurrent use.
type Index struct {
	kv     Backend
	window abi.ChainEpoch

	mu      sync.RWMutex
	heights map[abi.ChainEpoch]heightMeta
	nSent   int
	emitMu  sync.RWMutex
	emit    map[abi.ActorID][20]byte
}

// Open loads the per-height records (and emitter cache) into memory.
func Open(kv Backend, window abi.ChainEpoch) (*Index, error) {
	if kv == nil {
		return nil, errors.New("ethtxindex: nil backend")
	}
	if window <= 0 {
		window = DefaultWindow
	}
	ix := &Index{kv: kv, window: window, heights: map[abi.ChainEpoch]heightMeta{}, emit: map[abi.ActorID][20]byte{}}
	err := kv.AuxIterate(pfxHeight, func(k, v []byte) error {
		if len(k) != len(pfxHeight)+8 {
			return nil
		}
		h := abi.ChainEpoch(binary.BigEndian.Uint64(k[len(pfxHeight):]))
		rec, err := decodeHeight(h, v)
		if err != nil {
			return nil // skip a corrupt record; it will be re-indexed
		}
		ix.heights[h] = heightMeta{null: rec.Null, incl: rec.Incl, exec: rec.Exec}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ethtxindex: load height records: %w", err)
	}
	err = kv.AuxIterate(pfxSent, func(k, v []byte) error {
		ix.nSent++
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ethtxindex: load sent records: %w", err)
	}
	err = kv.AuxIterate(pfxEmit, func(k, v []byte) error {
		if len(k) != len(pfxEmit)+8 || len(v) != 20 {
			return nil
		}
		var a [20]byte
		copy(a[:], v)
		ix.emit[abi.ActorID(binary.BigEndian.Uint64(k[len(pfxEmit):]))] = a
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ethtxindex: load emitter cache: %w", err)
	}
	return ix, nil
}

// Window is the configured retention in epochs.
func (ix *Index) Window() abi.ChainEpoch { return ix.window }

func txKey(h [32]byte) []byte {
	return append(append(make([]byte, 0, len(pfxTx)+32), pfxTx...), h[:]...)
}

func sentKey(h [32]byte) []byte {
	return append(append(make([]byte, 0, len(pfxSent)+32), pfxSent...), h[:]...)
}

func heightKey(h abi.ChainEpoch) []byte {
	k := make([]byte, len(pfxHeight)+8)
	copy(k, pfxHeight)
	binary.BigEndian.PutUint64(k[len(pfxHeight):], uint64(h))
	return k
}

func emitKey(id abi.ActorID) []byte {
	k := make([]byte, len(pfxEmit)+8)
	copy(k, pfxEmit)
	binary.BigEndian.PutUint64(k[len(pfxEmit):], uint64(id))
	return k
}

// Has reports whether height h is indexed with inclusion tipset incl
// (incl empty: indexed as a null round).
func (ix *Index) Has(h abi.ChainEpoch, incl types.TipSetKey) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	m, ok := ix.heights[h]
	if !ok {
		return false
	}
	if incl.IsEmpty() {
		return m.null
	}
	return !m.null && m.incl == incl
}

// Height returns the record metadata at h.
func (ix *Index) Height(h abi.ChainEpoch) (HeightRecord, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	m, ok := ix.heights[h]
	if !ok {
		return HeightRecord{}, false
	}
	return HeightRecord{Height: h, Null: m.null, Incl: m.incl, Exec: m.exec}, true
}

// Covered reports whether every height in [lo, hi] is indexed.
func (ix *Index) Covered(lo, hi abi.ChainEpoch) bool {
	if hi < lo {
		return true
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	for h := hi; h >= lo; h-- {
		if _, ok := ix.heights[h]; !ok {
			return false
		}
	}
	return true
}

// Missing returns the unindexed heights in [lo, hi], highest first, at
// most limit of them (limit <= 0: no limit).
func (ix *Index) Missing(lo, hi abi.ChainEpoch, limit int) []abi.ChainEpoch {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []abi.ChainEpoch
	for h := hi; h >= lo; h-- {
		if _, ok := ix.heights[h]; !ok {
			out = append(out, h)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}

// Count returns the number of indexed heights.
func (ix *Index) Count() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.heights)
}

// Range returns the lowest and highest indexed heights (ok=false if empty).
func (ix *Index) Range() (lo, hi abi.ChainEpoch, ok bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	first := true
	for h := range ix.heights {
		if first || h < lo {
			lo = h
		}
		if first || h > hi {
			hi = h
		}
		first = false
	}
	return lo, hi, !first
}

// Lookup returns the location of tx hash h when it is indexed on a height
// whose record is still current. ok=false means "not in the index".
func (ix *Index) Lookup(h [32]byte) (Loc, HeightRecord, bool, error) {
	raw, ok, err := ix.kv.AuxGet(txKey(h))
	if err != nil || !ok {
		return Loc{}, HeightRecord{}, false, err
	}
	loc, inclDigest, err := decodeLoc(raw)
	if err != nil {
		return Loc{}, HeightRecord{}, false, nil // corrupt: treat as miss
	}
	ix.mu.RLock()
	m, ok := ix.heights[loc.Height]
	ix.mu.RUnlock()
	if !ok || m.null || keyDigest(m.incl) != inclDigest {
		return Loc{}, HeightRecord{}, false, nil // stale (reorged / pruned)
	}
	return loc, HeightRecord{Height: loc.Height, Incl: m.incl, Exec: m.exec}, true, nil
}

// Apply records one inclusion tipset (rec, with locs keyed by tx hash) and
// the null rounds between it and its executing child. Any record already
// at those heights for a different tipset is replaced and its tx entries
// removed. Idempotent.
func (ix *Index) Apply(rec HeightRecord, locs map[[32]byte]Loc, nulls []abi.ChainEpoch) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var puts []hstore.AuxPair
	var dels [][]byte
	replace := func(h abi.ChainEpoch, keep map[[32]byte]Loc) error {
		old, ok := ix.heights[h]
		if !ok || old.null {
			return nil
		}
		raw, found, err := ix.kv.AuxGet(heightKey(h))
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		orec, err := decodeHeight(h, raw)
		if err != nil {
			return nil
		}
		oldDigest := keyDigest(old.incl)
		for _, th := range orec.Hashes {
			if _, still := keep[th]; still {
				continue // rewritten below
			}
			// Only delete the entry if it still points at this height's
			// old tipset (it may have been re-included elsewhere since).
			eraw, ok, err := ix.kv.AuxGet(txKey(th))
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if l, d, err := decodeLoc(eraw); err == nil && l.Height == h && d == oldDigest {
				dels = append(dels, txKey(th))
			}
		}
		return nil
	}
	for _, n := range nulls {
		if err := replace(n, nil); err != nil {
			return err
		}
		puts = append(puts, hstore.AuxPair{Key: heightKey(n), Value: encodeHeight(HeightRecord{Height: n, Null: true})})
	}
	if old, ok := ix.heights[rec.Height]; !ok || old.null || old.incl != rec.Incl {
		if err := replace(rec.Height, locs); err != nil {
			return err
		}
	}
	digest := keyDigest(rec.Incl)
	rec.Hashes = rec.Hashes[:0]
	hashes := make([][32]byte, 0, len(locs))
	for th, l := range locs {
		l.Height = rec.Height
		puts = append(puts, hstore.AuxPair{Key: txKey(th), Value: encodeLoc(l, digest)})
		hashes = append(hashes, th)
	}
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })
	rec.Hashes = hashes
	puts = append(puts, hstore.AuxPair{Key: heightKey(rec.Height), Value: encodeHeight(rec)})
	if err := ix.kv.AuxWrite(puts, dels); err != nil {
		return err
	}
	for _, n := range nulls {
		ix.heights[n] = heightMeta{null: true}
	}
	ix.heights[rec.Height] = heightMeta{incl: rec.Incl, exec: rec.Exec}
	return nil
}

// Prune deletes every height record below minHeight and the tx entries
// they contributed. Returns the number of heights pruned.
func (ix *Index) Prune(minHeight abi.ChainEpoch) (int, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var victims []abi.ChainEpoch
	for h := range ix.heights {
		if h < minHeight {
			victims = append(victims, h)
		}
	}
	if len(victims) == 0 {
		return 0, nil
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i] < victims[j] })
	const chunk = 64
	for i := 0; i < len(victims); i += chunk {
		end := i + chunk
		if end > len(victims) {
			end = len(victims)
		}
		var dels [][]byte
		for _, h := range victims[i:end] {
			m := ix.heights[h]
			dels = append(dels, heightKey(h))
			if m.null {
				continue
			}
			raw, ok, err := ix.kv.AuxGet(heightKey(h))
			if err != nil {
				return 0, err
			}
			if !ok {
				continue
			}
			rec, err := decodeHeight(h, raw)
			if err != nil {
				continue
			}
			digest := keyDigest(m.incl)
			for _, th := range rec.Hashes {
				eraw, ok, err := ix.kv.AuxGet(txKey(th))
				if err != nil {
					return 0, err
				}
				if !ok {
					continue
				}
				if l, d, err := decodeLoc(eraw); err == nil && l.Height == h && d == digest {
					dels = append(dels, txKey(th))
				}
			}
		}
		if err := ix.kv.AuxWrite(nil, dels); err != nil {
			return 0, err
		}
		for _, h := range victims[i:end] {
			delete(ix.heights, h)
		}
	}
	return len(victims), nil
}

// --- locally published txs ----------------------------------------------

// PutSent persists a locally published tx record.
func (ix *Index) PutSent(h [32]byte, s Sent) error {
	k := sentKey(h)
	_, exists, err := ix.kv.AuxGet(k)
	if err != nil {
		return err
	}
	if err := ix.kv.AuxWrite([]hstore.AuxPair{{Key: k, Value: encodeSent(s)}}, nil); err != nil {
		return err
	}
	if !exists {
		ix.mu.Lock()
		ix.nSent++
		over := ix.nSent > maxSent
		ix.mu.Unlock()
		if over {
			_, _ = ix.PruneSent(-1, maxSent)
		}
	}
	return nil
}

// GetSent returns the persisted record of a locally published tx.
func (ix *Index) GetSent(h [32]byte) (Sent, bool, error) {
	raw, ok, err := ix.kv.AuxGet(sentKey(h))
	if err != nil || !ok {
		return Sent{}, false, err
	}
	s, err := decodeSent(raw)
	if err != nil {
		return Sent{}, false, nil
	}
	return s, true, nil
}

// PruneSent deletes sent records older than minEpoch (minEpoch < 0: none
// by age) and, beyond that, the oldest records over keep (keep <= 0: no
// count cap).
func (ix *Index) PruneSent(minEpoch abi.ChainEpoch, keep int) (int, error) {
	type ent struct {
		k  []byte
		ep abi.ChainEpoch
	}
	var all []ent
	err := ix.kv.AuxIterate(pfxSent, func(k, v []byte) error {
		s, err := decodeSent(v)
		ep := s.SentEpoch
		if err != nil {
			ep = -1
		}
		all = append(all, ent{k: append([]byte(nil), k...), ep: ep})
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ep < all[j].ep })
	var dels [][]byte
	n := len(all)
	for i, e := range all {
		if (minEpoch >= 0 && e.ep < minEpoch) || (keep > 0 && n-i > keep) {
			dels = append(dels, e.k)
		}
	}
	if len(dels) == 0 {
		ix.mu.Lock()
		ix.nSent = n
		ix.mu.Unlock()
		return 0, nil
	}
	if err := ix.kv.AuxWrite(nil, dels); err != nil {
		return 0, err
	}
	ix.mu.Lock()
	ix.nSent = n - len(dels)
	ix.mu.Unlock()
	return len(dels), nil
}

// --- emitter address cache ------------------------------------------------

// Emitter returns the cached eth address of an event emitter actor.
func (ix *Index) Emitter(id abi.ActorID) ([20]byte, bool) {
	ix.emitMu.RLock()
	defer ix.emitMu.RUnlock()
	a, ok := ix.emit[id]
	return a, ok
}

// PutEmitter caches (and persists) the eth address of an existing actor.
// Only call for actors that exist: their eth address is then immutable.
func (ix *Index) PutEmitter(id abi.ActorID, addr [20]byte) error {
	ix.emitMu.Lock()
	if cur, ok := ix.emit[id]; ok && cur == addr {
		ix.emitMu.Unlock()
		return nil
	}
	ix.emit[id] = addr
	ix.emitMu.Unlock()
	return ix.kv.AuxWrite([]hstore.AuxPair{{Key: emitKey(id), Value: addr[:]}}, nil)
}

// --- encoding -------------------------------------------------------------

// keyDigest is a short fingerprint of a tipset key used to detect stale
// entries cheaply (an entry is only trusted when it matches the current
// record at its height).
func keyDigest(k types.TipSetKey) uint64 {
	// FNV-1a over the key bytes.
	var h uint64 = 1469598103934665603
	for _, b := range k.Bytes() {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}

func putUvarint(b []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

func putVarint(b []byte, v int64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutVarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

func putBytes(b, v []byte) []byte {
	b = putUvarint(b, uint64(len(v)))
	return append(b, v...)
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errors.New("ethtxindex: bad uvarint")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.b)
	if n <= 0 {
		r.err = errors.New("ethtxindex: bad varint")
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) bytes() []byte {
	n := r.uvarint()
	if r.err != nil {
		return nil
	}
	if uint64(len(r.b)) < n {
		r.err = errors.New("ethtxindex: short buffer")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) byte1() byte {
	if r.err != nil {
		return 0
	}
	if len(r.b) < 1 {
		r.err = errors.New("ethtxindex: short buffer")
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *reader) fixed(n int) []byte {
	if r.err != nil {
		return nil
	}
	if len(r.b) < n {
		r.err = errors.New("ethtxindex: short buffer")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) cid() cid.Cid {
	b := r.bytes()
	if r.err != nil || len(b) == 0 {
		return cid.Undef
	}
	c, err := cid.Cast(b)
	if err != nil {
		r.err = err
		return cid.Undef
	}
	return c
}

const (
	locFlagSecp    = 1 << 0
	locFlagEvBase  = 1 << 1
	locFlagReceipt = 1 << 2
)

func encodeLoc(l Loc, inclDigest uint64) []byte {
	b := make([]byte, 0, 96)
	b = append(b, encVersion)
	var d [8]byte
	binary.BigEndian.PutUint64(d[:], inclDigest)
	b = append(b, d[:]...)
	var flags byte
	if l.IsSecp {
		flags |= locFlagSecp
	}
	if l.HasEvBase {
		flags |= locFlagEvBase
	}
	if l.Receipt != nil {
		flags |= locFlagReceipt
	}
	b = append(b, flags)
	b = putUvarint(b, uint64(l.Height))
	b = putUvarint(b, uint64(l.Idx))
	b = putBytes(b, l.MsgCID.Bytes())
	if l.HasEvBase {
		b = putUvarint(b, uint64(l.EvBase))
	}
	if r := l.Receipt; r != nil {
		b = putVarint(b, r.ExitCode)
		b = putVarint(b, r.GasUsed)
		b = putBytes(b, r.Return)
		var er []byte
		if r.EventsRoot.Defined() {
			er = r.EventsRoot.Bytes()
		}
		b = putBytes(b, er)
	}
	return b
}

func decodeLoc(raw []byte) (Loc, uint64, error) {
	r := &reader{b: raw}
	if v := r.byte1(); v != encVersion {
		return Loc{}, 0, fmt.Errorf("ethtxindex: loc version %d", v)
	}
	digest := binary.BigEndian.Uint64(r.fixed(8))
	if r.err != nil {
		return Loc{}, 0, r.err
	}
	flags := r.byte1()
	var l Loc
	l.IsSecp = flags&locFlagSecp != 0
	l.Height = abi.ChainEpoch(r.uvarint())
	l.Idx = uint32(r.uvarint())
	l.MsgCID = r.cid()
	if flags&locFlagEvBase != 0 {
		l.EvBase = uint32(r.uvarint())
		l.HasEvBase = true
	}
	if flags&locFlagReceipt != 0 {
		rc := &Receipt{}
		rc.ExitCode = r.varint()
		rc.GasUsed = r.varint()
		if ret := r.bytes(); len(ret) > 0 {
			rc.Return = append([]byte(nil), ret...)
		}
		rc.EventsRoot = r.cid()
		l.Receipt = rc
	}
	if r.err != nil {
		return Loc{}, 0, r.err
	}
	return l, digest, nil
}

func encodeHeight(rec HeightRecord) []byte {
	b := make([]byte, 0, 16+len(rec.Hashes)*32+len(rec.Incl.Bytes())+len(rec.Exec.Bytes()))
	b = append(b, encVersion)
	if rec.Null {
		return append(b, 1)
	}
	b = append(b, 0)
	b = putBytes(b, rec.Incl.Bytes())
	b = putBytes(b, rec.Exec.Bytes())
	b = putUvarint(b, uint64(len(rec.Hashes)))
	for _, h := range rec.Hashes {
		b = append(b, h[:]...)
	}
	return b
}

func decodeHeight(h abi.ChainEpoch, raw []byte) (HeightRecord, error) {
	r := &reader{b: raw}
	if v := r.byte1(); v != encVersion {
		return HeightRecord{}, fmt.Errorf("ethtxindex: height version %d", v)
	}
	rec := HeightRecord{Height: h}
	if r.byte1() == 1 {
		rec.Null = true
		return rec, r.err
	}
	ib := r.bytes()
	eb := r.bytes()
	if r.err != nil {
		return HeightRecord{}, r.err
	}
	var err error
	if rec.Incl, err = types.TipSetKeyFromBytes(append([]byte(nil), ib...)); err != nil {
		return HeightRecord{}, err
	}
	if rec.Exec, err = types.TipSetKeyFromBytes(append([]byte(nil), eb...)); err != nil {
		return HeightRecord{}, err
	}
	n := r.uvarint()
	if r.err != nil || uint64(len(r.b)) < n*32 {
		return HeightRecord{}, errors.New("ethtxindex: short hash list")
	}
	rec.Hashes = make([][32]byte, n)
	for i := range rec.Hashes {
		copy(rec.Hashes[i][:], r.fixed(32))
	}
	return rec, r.err
}

func encodeSent(s Sent) []byte {
	b := make([]byte, 0, 64+len(s.Raw))
	b = append(b, encVersion)
	b = putBytes(b, s.MsgCID.Bytes())
	b = append(b, s.From[:]...)
	b = putVarint(b, int64(s.SentEpoch))
	b = putBytes(b, s.Raw)
	return b
}

func decodeSent(raw []byte) (Sent, error) {
	r := &reader{b: raw}
	if v := r.byte1(); v != encVersion {
		return Sent{}, fmt.Errorf("ethtxindex: sent version %d", v)
	}
	var s Sent
	s.MsgCID = r.cid()
	copy(s.From[:], r.fixed(20))
	s.SentEpoch = abi.ChainEpoch(r.varint())
	if b := r.bytes(); len(b) > 0 {
		s.Raw = append([]byte(nil), b...)
	}
	return s, r.err
}

// CompactReceipt builds the inlined receipt for an entry, or nil when the
// return payload is too large to inline.
func CompactReceipt(exit int64, gasUsed int64, ret []byte, events *cid.Cid) *Receipt {
	if len(ret) > maxReturnBytes {
		return nil
	}
	rc := &Receipt{ExitCode: exit, GasUsed: gasUsed}
	if len(ret) > 0 {
		rc.Return = append([]byte(nil), ret...)
	}
	if events != nil && events.Defined() {
		rc.EventsRoot = *events
	}
	return rc
}
