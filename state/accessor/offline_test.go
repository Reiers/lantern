package accessor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	addr "github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/state/hamt"
)

// TestAccessor_OfflineFromFixtures walks the state tree using ONLY the
// blocks pinned in testdata/. It picks the captured state-root from the
// fixture filenames + the meta.json (if present), or falls back to the
// well-known known-good f01 actor ID lookup.
//
// This proves the proof-recording chain works without any network access,
// using the pinned mainnet bytes as a deterministic snapshot.
func TestAccessor_OfflineFromFixtures(t *testing.T) {
	mem := hamt.NewMemBlockStore()

	// Load every .block under testdata/ into memory.
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Skipf("no fixture dir: %v", err)
	}
	loaded := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".block") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".block")
		c, err := cid.Parse(name)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(fixtureDir, e.Name()))
		if err != nil {
			continue
		}
		if err := mem.PutVerify(c, raw); err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		loaded++
	}
	if loaded == 0 {
		t.Skip("no fixtures present; run TestAccessor_MainnetActors with LANTERN_FIXTURE_REFRESH=1 first")
	}
	t.Logf("loaded %d offline fixture blocks", loaded)

	// Find the state-root: any block that decodes as a StateRoot tuple.
	var stateRoot cid.Cid
	for _, c := range mem.CIDs() {
		raw, _ := mem.Get(context.Background(), c)
		if _, err := DecodeStateRoot(raw); err == nil {
			stateRoot = c
			break
		}
	}
	if !stateRoot.Defined() {
		t.Fatalf("no state-root tuple found in fixtures")
	}
	t.Logf("offline state root: %s", stateRoot)

	tr := &trustedroot.TrustedRoot{StateRoot: stateRoot}
	acc := New(tr, mem)

	// Walk to f01 (Init actor) — same address used in the online test.
	f01, _ := addr.NewIDAddress(1)
	actor, proof, err := acc.GetActorByID(context.Background(), f01)
	if err != nil {
		t.Fatalf("offline GetActor(f01): %v", err)
	}
	t.Logf("offline f01 code=%s head=%s nonce=%d balance=%s proofLen=%d",
		actor.Code, actor.Head, actor.Nonce, actor.Balance, len(proof))

	if !actor.Code.Defined() || !actor.Head.Defined() {
		t.Fatalf("decoded actor has undefined CIDs: %+v", actor)
	}
	if len(proof) < 2 {
		t.Fatalf("proof path too short (%d), expected at least state-root + 1 HAMT node", len(proof))
	}

	// Independently re-verify the proof via a fresh thin BlockGetter that
	// holds ONLY the proof blocks.
	thin := hamt.NewMemBlockStore()
	for _, c := range proof {
		raw, _ := mem.Get(context.Background(), c)
		thin.Put(c, raw)
	}
	thinAcc := New(tr, thin)
	actor2, _, err := thinAcc.GetActorByID(context.Background(), f01)
	if err != nil {
		t.Fatalf("re-verification with thin proof bundle failed: %v", err)
	}
	if !actor2.Code.Equals(actor.Code) || !actor2.Head.Equals(actor.Head) {
		t.Fatalf("re-verified actor differs from original")
	}
	t.Log("proof self-contained: re-verification with only the proof blocks succeeded")
}
