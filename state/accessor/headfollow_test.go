package accessor

import (
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/trustedroot"
)

// cidFor returns a deterministic distinct CID for a label.
func cidFor(t *testing.T, label string) cid.Cid {
	t.Helper()
	pref := cid.NewPrefixV1(cid.DagCBOR, 0x12) // sha2-256
	c, err := pref.Sum([]byte(label))
	if err != nil {
		t.Fatalf("cidFor(%q): %v", label, err)
	}
	return c
}

func TestEffectiveStateRoot_FallsBackToBootWhenNoProvider(t *testing.T) {
	boot := cidFor(t, "boot-state-root")
	a := New(&trustedroot.TrustedRoot{StateRoot: boot}, nil)
	if got := a.effectiveStateRoot(); !got.Equals(boot) {
		t.Fatalf("no provider: effectiveStateRoot = %s, want boot %s", got, boot)
	}
}

func TestEffectiveStateRoot_UsesHeadProviderWhenAvailable(t *testing.T) {
	boot := cidFor(t, "boot-state-root")
	head := cidFor(t, "live-head-state-root")
	a := New(&trustedroot.TrustedRoot{StateRoot: boot}, nil)
	a.SetHeadStateProvider(func() (cid.Cid, bool) { return head, true })
	if got := a.effectiveStateRoot(); !got.Equals(head) {
		t.Fatalf("with head provider: effectiveStateRoot = %s, want head %s", got, head)
	}
}

func TestEffectiveStateRoot_FallsBackWhenProviderNotReady(t *testing.T) {
	boot := cidFor(t, "boot-state-root")
	a := New(&trustedroot.TrustedRoot{StateRoot: boot}, nil)
	// Provider present but signals "no live head yet" (ok=false): must fall
	// back to the boot root so a freshly-started node still serves state at
	// the boot anchor until the first head arrives.
	a.SetHeadStateProvider(func() (cid.Cid, bool) { return cid.Undef, false })
	if got := a.effectiveStateRoot(); !got.Equals(boot) {
		t.Fatalf("provider not ready: effectiveStateRoot = %s, want boot %s", got, boot)
	}
}

func TestEffectiveStateRoot_FallsBackOnUndefinedHeadRoot(t *testing.T) {
	boot := cidFor(t, "boot-state-root")
	a := New(&trustedroot.TrustedRoot{StateRoot: boot}, nil)
	// ok=true but an undefined CID must not be used (defensive: an empty
	// tipset / missing ParentStateRoot must not blank out state reads).
	a.SetHeadStateProvider(func() (cid.Cid, bool) { return cid.Undef, true })
	if got := a.effectiveStateRoot(); !got.Equals(boot) {
		t.Fatalf("undefined head root: effectiveStateRoot = %s, want boot %s", got, boot)
	}
}

func TestRebind_PreservesHeadStateProvider(t *testing.T) {
	// Regression for the lantern#87 clobber bug: rebinding the block getter
	// (as the daemon does after Bitswap comes up) must NOT drop the
	// head-state provider. Before the fix, rebindBlockGetter rebuilt the
	// accessor with accessor.New, silently re-pinning state reads to boot.
	boot := cidFor(t, "boot-state-root")
	head := cidFor(t, "live-head-state-root")
	a := New(&trustedroot.TrustedRoot{StateRoot: boot}, nil)
	a.SetHeadStateProvider(func() (cid.Cid, bool) { return head, true })
	if got := a.effectiveStateRoot(); !got.Equals(head) {
		t.Fatalf("pre-rebind: got %s want head %s", got, head)
	}
	// Rebind with a different (nil) getter; provider must survive.
	a.Rebind(nil)
	if got := a.effectiveStateRoot(); !got.Equals(head) {
		t.Fatalf("post-rebind: got %s want head %s (provider was dropped)", got, head)
	}
}

func TestEffectiveStateRoot_TracksProviderChanges(t *testing.T) {
	boot := cidFor(t, "boot-state-root")
	h1 := cidFor(t, "head-epoch-100")
	h2 := cidFor(t, "head-epoch-200")
	a := New(&trustedroot.TrustedRoot{StateRoot: boot}, nil)
	cur := h1
	a.SetHeadStateProvider(func() (cid.Cid, bool) { return cur, true })
	if got := a.effectiveStateRoot(); !got.Equals(h1) {
		t.Fatalf("first head: got %s want %s", got, h1)
	}
	// Simulate a head advance: the same accessor must now resolve to the
	// newer state root without re-wiring.
	cur = h2
	if got := a.effectiveStateRoot(); !got.Equals(h2) {
		t.Fatalf("after head advance: got %s want %s", got, h2)
	}
}
