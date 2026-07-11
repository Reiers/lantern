package mpool

// #119: durable pending-message store.
//
// The problem: #47's rebroadcast loop tracks pending signed messages in
// memory. On daemon restart the pending set is thrown away. Messages the
// user pushed pre-restart:
//   - already consumed a nonce on the sender's on-chain account view,
//   - were gossiped once but may not have been mined,
//   - are no longer tracked, no retry, no confirm poll, no OnFailed fire.
//
// If the message wasn't mined before the restart, it's silently dead. The
// user's next MpoolPush from the same account then hits an "expected
// nonce X, got X+1" gap and stalls indefinitely.
//
// #51 covers "chain state can be rebuilt from the network." Signed
// pending messages cannot be rebuilt (the key signed them once, only the
// daemon that pushed them has the bytes), so they live beside chain
// state but with the same durability property as user state.
//
// Design: append-only JSONL at <home>/<network>/mpool/pending.jsonl.
// - Publish appends an `add` line with the raw signed bytes.
// - Confirm / max-retries-fail / Forget append a `tombstone` line.
// - Rebroadcast appends a `retry` line updating the counter + anchor.
// - On Open, replay all lines; later entries win. Compact when the
//   tombstone-to-live ratio grows large.
// - Rebroadcast IS byte-identical (same nonce, same CID), so we never
//   re-sign; we just store the ORIGINAL raw bytes.
//
// Never touches:
//   - private keys (they live in <net>/secrets/keystore, this file lives
//     in <net>/mpool/, chain-side of the secrets boundary),
//   - JWT / tokens.
//
// `lantern reset --chain-state` must NOT wipe pending.jsonl: user's
// pending sends are user state, not rebuildable chain state. The reset
// allow-list only knows about `headerstore` + `bootstrap-anchor.json`,
// so this file is already safe from that path.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ipfs/go-cid"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// PersistEntry is one live pending message in the durable store. Retries
// and PublishedAt are updated in place on rebroadcast; the raw bytes and
// FirstSeenWall are captured once at publish and never mutated.
type PersistEntry struct {
	CID           cid.Cid
	Raw           []byte
	PublishedAt   int64
	Retries       int
	FirstSeenWall time.Time
}

// SignedMessage decodes the raw bytes back into a live SignedMessage.
// Returns an error if the bytes are corrupt (shouldn't happen for
// entries we wrote ourselves, but be defensive on load).
func (e *PersistEntry) SignedMessage() (*ltypes.SignedMessage, error) {
	if len(e.Raw) == 0 {
		return nil, errors.New("persist entry: empty raw bytes")
	}
	var sm ltypes.SignedMessage
	if err := sm.UnmarshalCBOR(bytes.NewReader(e.Raw)); err != nil {
		return nil, fmt.Errorf("persist entry: decode signed message: %w", err)
	}
	return &sm, nil
}

// persistLine is the on-disk shape of one journal record.
type persistLine struct {
	// Op is "add" | "retry" | "tombstone".
	Op string `json:"op"`
	// CID identifies the entry.
	CID string `json:"cid"`
	// RawB64 is base64(raw signed bytes); set only on "add".
	RawB64 string `json:"raw,omitempty"`
	// PublishedAt is the anchored epoch (set on add + retry).
	PublishedAt int64 `json:"publishedAt,omitempty"`
	// Retries is the current rebroadcast counter (set on retry).
	Retries int `json:"retries,omitempty"`
	// FirstSeenWall is the wall-clock time we first saw this cid.
	FirstSeenWall time.Time `json:"firstSeenWall,omitzero"`
}

// persistStore is the append-only journal + in-memory index.
type persistStore struct {
	path string

	mu      sync.Mutex
	f       *os.File
	entries map[cid.Cid]*PersistEntry
	// records counts total journal lines written (adds + retries +
	// tombstones + carried-over on Open). When the live-entry ratio drops
	// below compactRatio and there are > compactMinLines total, we compact.
	records int
}

// compactRatio is the live-to-total ratio above which the store rewrites
// the journal in a single compact pass. Small file, cheap rewrite; keeps
// pathological append-only growth bounded when a busy sender churns.
//
// compactMinLines is the small-file floor: below it, compaction skips
// (the file is already tiny; rewriting saves nothing). 8 lines is roughly
// one full turnaround of a small pending set.
const (
	compactRatio    = 2 // compact when records > liveEntries * compactRatio
	compactMinLines = 8
)

// openPersistStore opens (or creates) the durable pending journal at
// path. The parent directory must already exist. Returns a store with
// the current live set already loaded into memory.
//
// If the journal is corrupt at some tail line, prior valid lines are
// still loaded; the corrupt tail is dropped and the file is compacted
// on open so subsequent writes don't inherit the damage.
func openPersistStore(path string) (*persistStore, error) {
	if path == "" {
		return nil, errors.New("mpool persist: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mpool persist: mkdir: %w", err)
	}
	s := &persistStore{
		path:    path,
		entries: make(map[cid.Cid]*PersistEntry),
	}
	if err := s.loadAndOpen(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadAndOpen replays the existing journal into memory, then keeps the
// file open in append mode for subsequent writes. Compacts on open when
// the journal is dominated by tombstones (or corrupt at the tail).
func (s *persistStore) loadAndOpen() error {
	f, err := os.OpenFile(s.path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("mpool persist: open read: %w", err)
	}

	corruptTail := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // signed messages fit well under 4 MiB
	lines := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		lines++
		var rec persistLine
		if err := json.Unmarshal(line, &rec); err != nil {
			// Corrupt tail (partial write, disk corruption). Drop it +
			// mark for compaction so we don't inherit the damage.
			corruptTail = true
			log.Warnw("mpool persist: dropping corrupt journal line", "path", s.path, "err", err)
			continue
		}
		if err := s.applyRecord(rec); err != nil {
			corruptTail = true
			log.Warnw("mpool persist: dropping invalid journal record", "path", s.path, "err", err)
		}
	}
	if err := sc.Err(); err != nil {
		// Scanner error mid-stream (usually EOF at partial line, which
		// bufio surfaces as ErrUnexpectedEOF via the scanner). Treat as
		// corrupt tail: keep whatever we managed to load, compact.
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			log.Warnw("mpool persist: journal scanner error, keeping partial load", "path", s.path, "err", err)
		}
		corruptTail = true
	}
	_ = f.Close()
	s.records = lines

	// Decide whether to compact. Rewrite in place when corrupt tail was
	// observed or when tombstones dominate the file.
	shouldCompact := corruptTail || (lines >= compactMinLines && lines > len(s.entries)*compactRatio)
	if shouldCompact {
		if err := s.compactLocked(); err != nil {
			return fmt.Errorf("mpool persist: compact on open: %w", err)
		}
	}

	// Reopen for append (create if we just compacted / cold-open).
	appendF, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("mpool persist: open append: %w", err)
	}
	s.f = appendF
	return nil
}

// applyRecord folds one journal line into the in-memory index.
func (s *persistStore) applyRecord(rec persistLine) error {
	c, err := cid.Parse(rec.CID)
	if err != nil {
		return fmt.Errorf("parse cid %q: %w", rec.CID, err)
	}
	switch rec.Op {
	case "add":
		raw, derr := base64.StdEncoding.DecodeString(rec.RawB64)
		if derr != nil {
			return fmt.Errorf("decode raw: %w", derr)
		}
		s.entries[c] = &PersistEntry{
			CID:           c,
			Raw:           raw,
			PublishedAt:   rec.PublishedAt,
			Retries:       rec.Retries,
			FirstSeenWall: rec.FirstSeenWall,
		}
	case "retry":
		if e, ok := s.entries[c]; ok {
			e.PublishedAt = rec.PublishedAt
			e.Retries = rec.Retries
		}
		// A retry for a cid we never saw is discarded (tombstone was
		// applied earlier, or add line was corrupt). Benign.
	case "tombstone":
		delete(s.entries, c)
	default:
		return fmt.Errorf("unknown op %q", rec.Op)
	}
	return nil
}

// compactLocked rewrites the journal to contain exactly one "add" line
// per live entry. Must be called with s.mu held OR before s.f is set.
func (s *persistStore) compactLocked() error {
	tmpPath := s.path + ".compact"
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open compact tmp: %w", err)
	}
	w := bufio.NewWriter(tmp)
	written := 0
	for _, e := range s.entries {
		rec := persistLine{
			Op:            "add",
			CID:           e.CID.String(),
			RawB64:        base64.StdEncoding.EncodeToString(e.Raw),
			PublishedAt:   e.PublishedAt,
			Retries:       e.Retries,
			FirstSeenWall: e.FirstSeenWall,
		}
		b, merr := json.Marshal(&rec)
		if merr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("marshal compact rec: %w", merr)
		}
		if _, werr := w.Write(append(b, '\n')); werr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fmt.Errorf("write compact rec: %w", werr)
		}
		written++
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("flush compact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("fsync compact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close compact tmp: %w", err)
	}
	// Close the current append handle before rename (Windows-safe;
	// darwin/linux tolerant either way).
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("rename compact: %w", err)
	}
	s.records = written
	// Caller (loadAndOpen or writeLocked) reopens the append handle.
	return nil
}

// writeLineLocked appends one line to the journal and fsyncs. Must be
// called with s.mu held.
func (s *persistStore) writeLineLocked(rec persistLine) error {
	if s.f == nil {
		return errors.New("mpool persist: store closed")
	}
	b, err := json.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	b = append(b, '\n')
	if _, err := s.f.Write(b); err != nil {
		return fmt.Errorf("append record: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("fsync record: %w", err)
	}
	s.records++
	return nil
}

// maybeCompactLocked triggers a compact when tombstones dominate the
// file. Must be called with s.mu held.
func (s *persistStore) maybeCompactLocked() {
	if s.records < compactMinLines {
		return
	}
	live := len(s.entries)
	if live == 0 {
		// Everything's tombstoned; rewrite to an empty file.
		if err := s.compactLocked(); err != nil {
			log.Warnw("mpool persist: compact-empty failed", "path", s.path, "err", err)
			return
		}
		f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			log.Warnw("mpool persist: reopen after compact-empty failed", "path", s.path, "err", err)
			return
		}
		s.f = f
		return
	}
	if s.records > live*compactRatio {
		if err := s.compactLocked(); err != nil {
			log.Warnw("mpool persist: compact failed", "path", s.path, "err", err)
			return
		}
		f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			log.Warnw("mpool persist: reopen after compact failed", "path", s.path, "err", err)
			return
		}
		s.f = f
	}
}

// Add records a new pending message. On success the fsync has landed.
func (s *persistStore) Add(e *PersistEntry) error {
	if e == nil {
		return errors.New("nil entry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := persistLine{
		Op:            "add",
		CID:           e.CID.String(),
		RawB64:        base64.StdEncoding.EncodeToString(e.Raw),
		PublishedAt:   e.PublishedAt,
		Retries:       e.Retries,
		FirstSeenWall: e.FirstSeenWall,
	}
	if err := s.writeLineLocked(rec); err != nil {
		return err
	}
	// Copy so caller mutations don't leak into the in-memory index.
	cpy := *e
	s.entries[e.CID] = &cpy
	return nil
}

// Remove tombstones an entry. Idempotent on a missing cid (writes a
// tombstone line so a subsequent compact drops the empty entry cleanly).
func (s *persistStore) Remove(c cid.Cid) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Only journal a tombstone when the entry is actually present, so
	// we don't grow the file with no-op tombstones on repeated Forget.
	if _, ok := s.entries[c]; !ok {
		return nil
	}
	rec := persistLine{
		Op:  "tombstone",
		CID: c.String(),
	}
	if err := s.writeLineLocked(rec); err != nil {
		return err
	}
	delete(s.entries, c)
	s.maybeCompactLocked()
	return nil
}

// UpdateOnRebroadcast bumps the retries counter + anchored epoch for an
// existing entry. No-op (with no journal line) when the cid isn't
// present (already tombstoned by a concurrent Reconcile pass).
func (s *persistStore) UpdateOnRebroadcast(c cid.Cid, retries int, publishedAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[c]
	if !ok {
		return nil
	}
	rec := persistLine{
		Op:          "retry",
		CID:         c.String(),
		Retries:     retries,
		PublishedAt: publishedAt,
	}
	if err := s.writeLineLocked(rec); err != nil {
		return err
	}
	e.Retries = retries
	e.PublishedAt = publishedAt
	s.maybeCompactLocked()
	return nil
}

// All returns a stable snapshot of live entries at call time.
func (s *persistStore) All() []*PersistEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*PersistEntry, 0, len(s.entries))
	for _, e := range s.entries {
		cpy := *e
		out = append(out, &cpy)
	}
	return out
}

// Close flushes and closes the underlying file. Safe to call multiple
// times.
func (s *persistStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
