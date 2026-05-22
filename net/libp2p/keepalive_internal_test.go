// Unit tests for the issue #9 follow-up keepalive helpers:
//   - markDialAttempt + lastDialAttempt: 5-minute cooldown bookkeeping
//   - shufflePeers: randomized routing-table walk order
//   - keepaliveStats observability: Stuck + ClosestWalks counters
//
// These need internal-package access (helpers + struct fields are
// unexported), hence the in-package _test file.

package libp2p

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

func TestMarkDialAttempt_RoundTrip(t *testing.T) {
	h := &Host{}
	pid := peer.ID("12D3KooWtestpeer1")

	if _, ok := h.lastDialAttempt(pid); ok {
		t.Fatal("expected no dial attempt before mark")
	}

	before := time.Now()
	h.markDialAttempt(pid)
	after := time.Now()

	got, ok := h.lastDialAttempt(pid)
	if !ok {
		t.Fatal("expected dial attempt after mark")
	}
	if got.Before(before) || got.After(after.Add(time.Millisecond)) {
		t.Errorf("dial-attempt time %v not within [%v, %v]", got, before, after)
	}
}

func TestMarkDialAttempt_Pruning(t *testing.T) {
	h := &Host{}
	// Insert 128 distinct attempts to get len() crossing the > 64 bound
	// without yet triggering the modulo-64 prune (which fires at len ==
	// 128, 192, 256, ...).
	//
	// Use a unique key generator that yields zero-collision strings of
	// the form "peer-XXXX".
	key := func(i int) peer.ID {
		return peer.ID("peer-" + time.Now().UTC().Format("") + string(rune('A'+i%26)) + string(rune('A'+(i/26)%26)) + string(rune('A'+(i/676)%26)))
	}
	for i := 0; i < 128; i++ {
		h.markDialAttempt(key(i))
	}

	// Backdate every entry currently in the map. We then add one more
	// fresh entry; the modulo-64 prune at len==192 (after the next 64
	// inserts) should wipe all of them.
	h.kaLastAttemptMu.Lock()
	oldTime := time.Now().Add(-31 * time.Minute)
	for k := range h.kaLastAttempt {
		h.kaLastAttempt[k] = oldTime
	}
	h.kaLastAttemptMu.Unlock()

	// Insert 64 more fresh, distinct entries to reach len == 192 and
	// retrigger the prune.
	for i := 0; i < 64; i++ {
		h.markDialAttempt(key(128 + i))
	}

	// Every remaining entry must be fresh (older than 30 min was pruned).
	h.kaLastAttemptMu.Lock()
	defer h.kaLastAttemptMu.Unlock()
	cutoff := time.Now().Add(-30 * time.Minute)
	stale := 0
	for _, t2 := range h.kaLastAttempt {
		if t2.Before(cutoff) {
			stale++
		}
	}
	if stale != 0 {
		t.Errorf("prune left %d stale entries in the map", stale)
	}
}

func TestShufflePeers_DoesNotDropPeers(t *testing.T) {
	in := make([]peer.ID, 32)
	want := make(map[peer.ID]struct{}, 32)
	for i := range in {
		p := peer.ID(string(rune('a' + i)))
		in[i] = p
		want[p] = struct{}{}
	}
	shufflePeers(in)
	if len(in) != 32 {
		t.Fatalf("shuffle changed length: %d -> %d", 32, len(in))
	}
	for _, p := range in {
		if _, ok := want[p]; !ok {
			t.Errorf("shuffle introduced unknown peer %q", p)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Errorf("shuffle dropped peers: %v", want)
	}
}

func TestShufflePeers_ActuallyShuffles(t *testing.T) {
	// Statistical guarantee that shuffle actually permutes: over many
	// shuffles of a 16-element identity sequence, position 0 should not
	// always hold the original peer[0].
	hits := 0
	const trials = 100
	for i := 0; i < trials; i++ {
		ps := make([]peer.ID, 16)
		for j := range ps {
			ps[j] = peer.ID(string(rune('a' + j)))
		}
		shufflePeers(ps)
		if ps[0] == peer.ID("a") {
			hits++
		}
	}
	// Identity probability for any one slot is ~6.25%; over 100 trials
	// hitting 100 times means shuffle didn't shuffle. Allow generous slack.
	if hits > 40 {
		t.Errorf("shuffle barely permuted: %d/%d trials kept ps[0]='a'", hits, trials)
	}
}

func TestKeepaliveStats_StuckAndClosestWalks(t *testing.T) {
	h := &Host{}
	// Simulate keepalive recording some activity.
	h.kaCycles.Add(5)
	h.kaTriggered.Add(3)
	h.kaBootDial.Add(7)
	h.kaRouteDial.Add(20)
	h.kaStuck.Add(11)
	h.kaClosestWalks.Add(2)
	h.kaLastCount.Store(34)

	ks := h.KeepaliveStats()
	if ks.Cycles != 5 || ks.Triggered != 3 || ks.BootstrapDial != 7 || ks.RoutingDial != 20 {
		t.Errorf("base counters wrong: %+v", ks)
	}
	if ks.Stuck != 11 {
		t.Errorf("Stuck = %d, want 11", ks.Stuck)
	}
	if ks.ClosestWalks != 2 {
		t.Errorf("ClosestWalks = %d, want 2", ks.ClosestWalks)
	}
	if ks.LastPeerCount != 34 {
		t.Errorf("LastPeerCount = %d, want 34", ks.LastPeerCount)
	}
}

func TestAuditPreviousDials_CountsDisconnected(t *testing.T) {
	// We can't easily build a real libp2p Host inline, but we CAN
	// verify the early-return path when kaPrevDialed is empty.
	h := &Host{}
	h.auditPreviousDials() // empty set, no panic, no increment
	if h.kaStuck.Load() != 0 {
		t.Errorf("auditPreviousDials with empty set bumped Stuck to %d", h.kaStuck.Load())
	}
}

func TestSavePreviousDialed_RoundTrip(t *testing.T) {
	h := &Host{}
	want := map[peer.ID]struct{}{
		peer.ID("a"): {},
		peer.ID("b"): {},
		peer.ID("c"): {},
	}
	h.savePreviousDialed(want)

	h.kaPrevDialedMu.Lock()
	got := h.kaPrevDialed
	h.kaPrevDialedMu.Unlock()

	if len(got) != 3 {
		t.Errorf("savePreviousDialed stored %d, want 3", len(got))
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q from stored set", k)
		}
	}
}
