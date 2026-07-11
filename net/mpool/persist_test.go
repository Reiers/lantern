// Unit tests for the durable pending-message store (#119).
//
// These are pure filesystem tests — no libp2p host, no gossipsub. They
// exercise the JSONL append + tombstone + compact + reopen paths in
// isolation from the Pool integration.
package mpool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

// mkCID makes a deterministic CID from a small string, so tests are
// readable without wiring the CBOR-encoding path.
func mkCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.Raw, h)
}

// mkEntry returns a PersistEntry with valid-shape raw bytes. Real raw
// bytes come from ltypes.SignedMessage.Serialize in production; here we
// just need something the store can round-trip.
func mkEntry(t *testing.T, s string) *PersistEntry {
	t.Helper()
	return &PersistEntry{
		CID:           mkCID(t, s),
		Raw:           []byte("raw-bytes-for-" + s),
		PublishedAt:   0,
		Retries:       0,
		FirstSeenWall: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}
}

func TestPersist_AddAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	s, err := openPersistStore(path)
	require.NoError(t, err)

	require.NoError(t, s.Add(mkEntry(t, "one")))
	require.NoError(t, s.Add(mkEntry(t, "two")))
	require.NoError(t, s.Add(mkEntry(t, "three")))
	require.NoError(t, s.Close())

	// Reopen: the three entries must come back.
	s2, err := openPersistStore(path)
	require.NoError(t, err)
	defer s2.Close()

	all := s2.All()
	require.Len(t, all, 3)
	seen := map[string]bool{}
	for _, e := range all {
		seen[e.CID.String()] = true
		require.Contains(t, string(e.Raw), "raw-bytes-for-")
	}
	require.True(t, seen[mkCID(t, "one").String()])
	require.True(t, seen[mkCID(t, "two").String()])
	require.True(t, seen[mkCID(t, "three").String()])
}

func TestPersist_RemoveThenReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	s, err := openPersistStore(path)
	require.NoError(t, err)

	e1 := mkEntry(t, "a")
	e2 := mkEntry(t, "b")
	require.NoError(t, s.Add(e1))
	require.NoError(t, s.Add(e2))
	require.NoError(t, s.Remove(e1.CID))
	require.NoError(t, s.Close())

	s2, err := openPersistStore(path)
	require.NoError(t, err)
	defer s2.Close()

	all := s2.All()
	require.Len(t, all, 1)
	require.Equal(t, e2.CID, all[0].CID)
}

func TestPersist_UpdateOnRebroadcastReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	s, err := openPersistStore(path)
	require.NoError(t, err)

	e := mkEntry(t, "retryable")
	require.NoError(t, s.Add(e))
	require.NoError(t, s.UpdateOnRebroadcast(e.CID, 3, 4242))
	require.NoError(t, s.Close())

	s2, err := openPersistStore(path)
	require.NoError(t, err)
	defer s2.Close()

	all := s2.All()
	require.Len(t, all, 1)
	require.Equal(t, 3, all[0].Retries)
	require.Equal(t, int64(4242), all[0].PublishedAt)
}

// TestPersist_RemoveOnMissingCID is a no-op — nothing to journal.
func TestPersist_RemoveOnMissingCID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	s, err := openPersistStore(path)
	require.NoError(t, err)
	defer s.Close()

	// Remove without any Add: no error, journal stays empty.
	require.NoError(t, s.Remove(mkCID(t, "ghost")))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(0), info.Size(), "no-op Remove must not journal a line")
}

// TestPersist_UpdateOnRebroadcastMissingCID is a no-op.
func TestPersist_UpdateOnRebroadcastMissingCID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	s, err := openPersistStore(path)
	require.NoError(t, err)
	defer s.Close()

	// Retry-update on an unknown cid must not journal a line.
	require.NoError(t, s.UpdateOnRebroadcast(mkCID(t, "ghost"), 5, 100))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(0), info.Size())
}

// TestPersist_CompactAfterTombstoneChurn: churn > compactMinLines total
// records with tombstone-dominant ratio, then reopen and verify the file
// was compacted (line count matches the live set).
func TestPersist_CompactAfterTombstoneChurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	s, err := openPersistStore(path)
	require.NoError(t, err)

	// 20 adds + 15 removes: 35 lines total, 5 live. records/live > 2.
	entries := make([]*PersistEntry, 20)
	for i := range entries {
		entries[i] = mkEntry(t, "entry-"+string(rune('a'+i)))
		require.NoError(t, s.Add(entries[i]))
	}
	for _, e := range entries[:15] {
		require.NoError(t, s.Remove(e.CID))
	}
	// Compaction should have fired inside Remove. Verify by counting
	// lines in the file.
	require.NoError(t, s.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := 0
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines++
		}
	}
	require.LessOrEqual(t, lines, 6, "after compact, journal must contain roughly the live set (5), got %d lines", lines)

	// Reload: exactly 5 live entries.
	s2, err := openPersistStore(path)
	require.NoError(t, err)
	defer s2.Close()
	require.Len(t, s2.All(), 5)
}

// TestPersist_CorruptTailIsRecovered writes an intentionally malformed
// last line, then verifies open logs the drop, keeps the earlier live
// entries, and compacts the file so the corruption doesn't survive.
func TestPersist_CorruptTailIsRecovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	s, err := openPersistStore(path)
	require.NoError(t, err)

	e := mkEntry(t, "keeper")
	require.NoError(t, s.Add(e))
	require.NoError(t, s.Close())

	// Append a corrupt tail line to simulate a torn write.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("{ this is not valid json\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Reopen: the keeper survives, the corrupt line is dropped.
	s2, err := openPersistStore(path)
	require.NoError(t, err)
	defer s2.Close()
	require.Len(t, s2.All(), 1)
	require.Equal(t, e.CID, s2.All()[0].CID)

	// After the corrupt-tail-triggered compaction, the file must not
	// contain the malformed line anymore.
	raw, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	require.NotContains(t, string(raw), "not valid json")
}

// TestPersist_JournalLineShape sanity-checks the on-disk format, so a
// future refactor that changes field names surfaces here instead of at
// upgrade time on user machines.
func TestPersist_JournalLineShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.jsonl")
	s, err := openPersistStore(path)
	require.NoError(t, err)

	e := mkEntry(t, "shape")
	require.NoError(t, s.Add(e))
	require.NoError(t, s.Close())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	line := strings.TrimSpace(strings.Split(string(raw), "\n")[0])

	var rec persistLine
	require.NoError(t, json.Unmarshal([]byte(line), &rec))
	require.Equal(t, "add", rec.Op)
	require.Equal(t, e.CID.String(), rec.CID)
	require.NotEmpty(t, rec.RawB64)
}

// TestPersist_EmptyPathRejected: empty path is a programmer error.
func TestPersist_EmptyPathRejected(t *testing.T) {
	_, err := openPersistStore("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty path")
}

// TestPersist_MkdirParent verifies the store creates its parent
// directory (matches the <home>/<network>/mpool/ convention where the
// mpool dir may not exist on first Pool.New).
func TestPersist_MkdirParent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "does-not-exist-yet", "mpool", "pending.jsonl")

	s, err := openPersistStore(path)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.Add(mkEntry(t, "in-a-fresh-dir")))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(0))
}
