package ethtxindex

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
)

func testCID(t testing.TB, seed string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(seed), mh.BLAKE2B_MIN+31, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.DagCBOR, h)
}

func testKey(t testing.TB, seeds ...string) types.TipSetKey {
	var cs []cid.Cid
	for _, s := range seeds {
		cs = append(cs, testCID(t, s))
	}
	return types.NewTipSetKey(cs...)
}

func txHash(seed string, i int) [32]byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(i))
	return sha256.Sum256(append([]byte(seed), b[:]...))
}

func openMem(t testing.TB) *hstore.Store {
	t.Helper()
	hs, err := hstore.Open("", hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close() })
	return hs
}

func applyHeight(t testing.TB, ix *Index, h abi.ChainEpoch, fork string, n int, nulls ...abi.ChainEpoch) map[[32]byte]Loc {
	t.Helper()
	locs := map[[32]byte]Loc{}
	for i := 0; i < n; i++ {
		ev := cid.Undef
		if i%2 == 0 {
			ev = testCID(t, fork+"ev")
		}
		locs[txHash(fork, int(h)*1000+i)] = Loc{
			Idx: uint32(i), IsSecp: i%3 == 0, MsgCID: testCID(t, fork+string(rune(i))),
			EvBase: uint32(i * 2), HasEvBase: true,
			Receipt: &Receipt{ExitCode: int64(i % 2), GasUsed: int64(1000 + i), Return: []byte{byte(i)}, EventsRoot: ev},
		}
	}
	rec := HeightRecord{Height: h, Incl: testKey(t, fork, "a"), Exec: testKey(t, fork, "child")}
	if err := ix.Apply(rec, locs, nulls); err != nil {
		t.Fatal(err)
	}
	return locs
}

func TestApplyLookupCoverage(t *testing.T) {
	ix, err := Open(openMem(t), 100)
	if err != nil {
		t.Fatal(err)
	}
	locs := applyHeight(t, ix, 10, "main", 5, 11, 12) // 11, 12 null
	applyHeight(t, ix, 13, "main", 3)
	for h, want := range locs {
		got, rec, ok, err := ix.Lookup(h)
		if err != nil || !ok {
			t.Fatalf("lookup: ok=%v err=%v", ok, err)
		}
		if got.Height != 10 || got.Idx != want.Idx || got.MsgCID != want.MsgCID || got.IsSecp != want.IsSecp || got.EvBase != want.EvBase || !got.HasEvBase {
			t.Fatalf("loc mismatch: got %+v want %+v", got, want)
		}
		if got.Receipt == nil || got.Receipt.GasUsed != want.Receipt.GasUsed || got.Receipt.ExitCode != want.Receipt.ExitCode || got.Receipt.EventsRoot != want.Receipt.EventsRoot || string(got.Receipt.Return) != string(want.Receipt.Return) {
			t.Fatalf("receipt mismatch: got %+v want %+v", got.Receipt, want.Receipt)
		}
		if rec.Incl != testKey(t, "main", "a") || rec.Exec != testKey(t, "main", "child") {
			t.Fatalf("record keys mismatch")
		}
	}
	if !ix.Covered(10, 13) {
		t.Fatal("10..13 should be covered (11, 12 null)")
	}
	if ix.Covered(9, 13) || ix.Covered(10, 14) {
		t.Fatal("coverage too wide")
	}
	if m := ix.Missing(8, 14, 0); len(m) != 3 || m[0] != 14 || m[1] != 9 || m[2] != 8 {
		t.Fatalf("missing = %v", m)
	}
	if _, _, ok, _ := ix.Lookup(txHash("nope", 1)); ok {
		t.Fatal("phantom hit")
	}
	if !ix.Has(10, testKey(t, "main", "a")) || ix.Has(10, testKey(t, "fork", "a")) || !ix.Has(11, types.TipSetKey{}) {
		t.Fatal("Has mismatch")
	}
}

func TestReorgReplacesEntries(t *testing.T) {
	hs := openMem(t)
	ix, _ := Open(hs, 100)
	old := applyHeight(t, ix, 20, "main", 4)
	// Shared tx: present in both forks (re-included on the new fork).
	var shared [32]byte
	for h := range old {
		shared = h
		break
	}
	newLocs := map[[32]byte]Loc{shared: {Idx: 9, MsgCID: testCID(t, "sharedmsg")}}
	for i := 0; i < 2; i++ {
		newLocs[txHash("fork", i)] = Loc{Idx: uint32(i), MsgCID: testCID(t, "forkmsg"+string(rune(i)))}
	}
	if err := ix.Apply(HeightRecord{Height: 20, Incl: testKey(t, "fork", "a"), Exec: testKey(t, "fork", "c")}, newLocs, nil); err != nil {
		t.Fatal(err)
	}
	for h := range old {
		l, _, ok, _ := ix.Lookup(h)
		if h == shared {
			if !ok || l.Idx != 9 {
				t.Fatalf("shared tx should point at the new fork: ok=%v %+v", ok, l)
			}
			continue
		}
		if ok {
			t.Fatal("orphaned tx still resolves")
		}
		if _, found, _ := hs.AuxGet(txKey(h)); found {
			t.Fatal("orphaned tx entry not deleted")
		}
	}
	for i := 0; i < 2; i++ {
		if _, _, ok, _ := ix.Lookup(txHash("fork", i)); !ok {
			t.Fatal("new fork tx missing")
		}
	}
	// A height that becomes a null round on the new chain drops its txs.
	applyHeight(t, ix, 30, "main", 2)
	if err := ix.Apply(HeightRecord{Height: 29, Incl: testKey(t, "x", "a"), Exec: testKey(t, "x", "c")}, nil, []abi.ChainEpoch{30}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := ix.Lookup(txHash("main", 30*1000)); ok {
		t.Fatal("tx at a now-null height still resolves")
	}
}

func TestPruneAndReopen(t *testing.T) {
	dir := t.TempDir()
	hs, err := hstore.Open(dir, hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ix, _ := Open(hs, 10)
	var all []map[[32]byte]Loc
	for h := abi.ChainEpoch(100); h < 120; h++ {
		all = append(all, applyHeight(t, ix, h, "main", 3))
	}
	n, err := ix.Prune(110)
	if err != nil || n != 10 {
		t.Fatalf("prune: n=%d err=%v", n, err)
	}
	for i, locs := range all {
		for h := range locs {
			_, _, ok, _ := ix.Lookup(h)
			if want := i >= 10; ok != want {
				t.Fatalf("height %d: ok=%v want %v", 100+i, ok, want)
			}
			if i < 10 {
				if _, found, _ := hs.AuxGet(txKey(h)); found {
					t.Fatal("pruned entry left on disk")
				}
			}
		}
	}
	// Sent + emitter, then a restart (close + reopen the store).
	s := Sent{MsgCID: testCID(t, "sent"), From: [20]byte{1, 2, 3}, SentEpoch: 115, Raw: []byte{0x02, 0xf8}}
	if err := ix.PutSent(txHash("sent", 1), s); err != nil {
		t.Fatal(err)
	}
	if err := ix.PutEmitter(1234, [20]byte{9, 9}); err != nil {
		t.Fatal(err)
	}
	if err := hs.Close(); err != nil {
		t.Fatal(err)
	}
	hs2, err := hstore.Open(dir, hstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer hs2.Close()
	ix2, err := Open(hs2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ix2.Count() != 10 {
		t.Fatalf("reopened heights = %d, want 10", ix2.Count())
	}
	if !ix2.Covered(110, 119) {
		t.Fatal("coverage lost on reopen")
	}
	for h := range all[15] {
		if _, _, ok, _ := ix2.Lookup(h); !ok {
			t.Fatal("entry lost on reopen")
		}
	}
	got, ok, err := ix2.GetSent(txHash("sent", 1))
	if err != nil || !ok || got.MsgCID != s.MsgCID || got.From != s.From || got.SentEpoch != 115 || string(got.Raw) != string(s.Raw) {
		t.Fatalf("sent lost on reopen: %+v ok=%v err=%v", got, ok, err)
	}
	if a, ok := ix2.Emitter(1234); !ok || a != ([20]byte{9, 9}) {
		t.Fatal("emitter lost on reopen")
	}
	if n, err := ix2.PruneSent(116, 0); err != nil || n != 1 {
		t.Fatalf("prune sent: n=%d err=%v", n, err)
	}
	if _, ok, _ := ix2.GetSent(txHash("sent", 1)); ok {
		t.Fatal("sent not pruned")
	}
}

func TestSentCap(t *testing.T) {
	ix, _ := Open(openMem(t), 10)
	for i := 0; i < maxSent+10; i++ {
		if err := ix.PutSent(txHash("cap", i), Sent{MsgCID: testCID(t, "m"), SentEpoch: abi.ChainEpoch(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := ix.GetSent(txHash("cap", 0)); ok {
		t.Fatal("oldest sent record not evicted")
	}
	if _, ok, _ := ix.GetSent(txHash("cap", maxSent+9)); !ok {
		t.Fatal("newest sent record evicted")
	}
}

func TestCompactReceiptCap(t *testing.T) {
	if CompactReceipt(0, 1, make([]byte, maxReturnBytes+1), nil) != nil {
		t.Fatal("oversized return inlined")
	}
	if r := CompactReceipt(0, 1, make([]byte, maxReturnBytes), nil); r == nil {
		t.Fatal("max-size return not inlined")
	}
}

// A day of calibration-shaped load on disk: 2880 heights x 50 txs.
func buildDay(tb testing.TB, dir string, txPerHeight int) (*hstore.Store, *Index) {
	hs, err := hstore.Open(dir, hstore.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	ix, _ := Open(hs, DefaultWindow)
	for h := abi.ChainEpoch(0); h < DefaultWindow; h++ {
		applyHeight(tb, ix, h, "day", txPerHeight)
	}
	return hs, ix
}

func TestDayScaleLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	dir := t.TempDir()
	hs, ix := buildDay(t, dir, 50)
	defer hs.Close()
	var hits, misses []time.Duration
	for i := 0; i < 2000; i++ {
		h := txHash("day", (i%2880)*1000+i%50)
		s := time.Now()
		if _, _, ok, err := ix.Lookup(h); err != nil || !ok {
			t.Fatalf("lookup %d: ok=%v err=%v", i, ok, err)
		}
		hits = append(hits, time.Since(s))
		s = time.Now()
		if _, _, ok, _ := ix.Lookup(txHash("absent", i)); ok {
			t.Fatal("phantom")
		}
		_ = ix.Covered(0, DefaultWindow-1)
		misses = append(misses, time.Since(s))
	}
	var keys, bytes int64
	_ = hs.AuxIterate([]byte("etx/"), func(k, v []byte) error {
		keys++
		bytes += int64(len(k) + len(v))
		return nil
	})
	t.Logf("2880 heights x 50 txs: %d keys, %.1f MiB logical (key+value)", keys, float64(bytes)/(1<<20))
	sort.Slice(hits, func(i, j int) bool { return hits[i] < hits[j] })
	sort.Slice(misses, func(i, j int) bool { return misses[i] < misses[j] })
	p99 := func(d []time.Duration) time.Duration { return d[len(d)*99/100] }
	t.Logf("144k entries: hit p50=%v p99=%v; miss+coverage p50=%v p99=%v", hits[len(hits)/2], p99(hits), misses[len(misses)/2], p99(misses))
	if p99(hits) > 50*time.Millisecond || p99(misses) > 50*time.Millisecond {
		t.Fatalf("index read too slow")
	}
}

func BenchmarkLookupHit_Day(b *testing.B) {
	dir := b.TempDir()
	hs, ix := buildDay(b, dir, 50)
	defer hs.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok, _ := ix.Lookup(txHash("day", (i%2880)*1000+i%50)); !ok {
			b.Fatal("miss")
		}
	}
}

func BenchmarkLookupMissCovered_Day(b *testing.B) {
	dir := b.TempDir()
	hs, ix := buildDay(b, dir, 50)
	defer hs.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok, _ := ix.Lookup(txHash("absent", i)); ok {
			b.Fatal("phantom")
		}
		if !ix.Covered(0, DefaultWindow-1) {
			b.Fatal("uncovered")
		}
	}
}
