package prefetch

import (
	"strings"
	"testing"
)

func TestBuiltinWarmSet_Mainnet(t *testing.T) {
	got := BuiltinWarmSet("mainnet")
	if len(got) != 4 {
		t.Fatalf("mainnet warm-set: want 4 addrs, got %d (%v)", len(got), got)
	}
	want := map[string]bool{
		strings.ToLower(pdpVerifierMainnet):     false,
		strings.ToLower(fwssMainnet):            false,
		strings.ToLower(serviceRegistryMainnet): false,
		strings.ToLower(usdfcMainnet):           false,
	}
	for _, a := range got {
		want[strings.ToLower(a)] = true
	}
	for addr, seen := range want {
		if !seen {
			t.Errorf("mainnet warm-set missing %s", addr)
		}
	}
}

func TestBuiltinWarmSet_CalibrationAliases(t *testing.T) {
	for _, name := range []string{"calibration", "Calibnet", "CALIBRATIONNET", "  calibration  "} {
		got := BuiltinWarmSet(name)
		if len(got) != 4 {
			t.Errorf("%q: want 4 addrs, got %d", name, len(got))
		}
		if len(got) > 0 && !strings.EqualFold(got[0], pdpVerifierCalib) {
			t.Errorf("%q: first addr = %s, want calibration PDPVerifier %s", name, got[0], pdpVerifierCalib)
		}
	}
}

func TestBuiltinWarmSet_UnknownNetwork(t *testing.T) {
	for _, name := range []string{"", "devnet", "localnet", "butterflynet"} {
		if got := BuiltinWarmSet(name); got != nil {
			t.Errorf("BuiltinWarmSet(%q) = %v, want nil", name, got)
		}
	}
}

func TestBuiltinWarmSet_ReturnsFreshCopy(t *testing.T) {
	a := BuiltinWarmSet("mainnet")
	if len(a) == 0 {
		t.Fatal("empty")
	}
	a[0] = "0xdeadbeef"
	b := BuiltinWarmSet("mainnet")
	if strings.EqualFold(b[0], "0xdeadbeef") {
		t.Fatal("BuiltinWarmSet returned a shared/mutable slice")
	}
}

func TestMergeWarmSets_UnionAndDedupe(t *testing.T) {
	builtin := BuiltinWarmSet("calibration")
	// consumer overlaps registry (different case) + adds one extra.
	extra := "0x1111111111111111111111111111111111111111"
	consumer := []string{strings.ToUpper(serviceRegistryCalib), extra}

	merged := MergeWarmSets(builtin, consumer)

	// 4 built-ins, registry overlaps with consumer -> 4 + 1 extra = 5.
	if len(merged) != 5 {
		t.Fatalf("merged len = %d, want 5 (%v)", len(merged), merged)
	}

	// De-dup by canonical form: registry must appear exactly once.
	count := 0
	for _, a := range merged {
		if dedupeKey(a) == dedupeKey(serviceRegistryCalib) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("registry appears %d times after merge, want 1", count)
	}

	// Consumer entries come first (consumer wins ordering).
	if dedupeKey(merged[0]) != dedupeKey(serviceRegistryCalib) {
		t.Errorf("merged[0] = %s, want consumer registry first", merged[0])
	}

	// The extra consumer addr survives.
	found := false
	for _, a := range merged {
		if dedupeKey(a) == dedupeKey(extra) {
			found = true
		}
	}
	if !found {
		t.Errorf("consumer extra addr %s dropped from merge", extra)
	}
}

func TestMergeWarmSets_EmptyInputs(t *testing.T) {
	if got := MergeWarmSets(nil, nil); len(got) != 0 {
		t.Errorf("merge(nil,nil) = %v, want empty", got)
	}
	only := BuiltinWarmSet("mainnet")
	if got := MergeWarmSets(only, nil); len(got) != 4 {
		t.Errorf("merge(builtin,nil) len = %d, want 4", len(got))
	}
	if got := MergeWarmSets(nil, only); len(got) != 4 {
		t.Errorf("merge(nil,consumer) len = %d, want 4", len(got))
	}
}

func TestMergeWarmSets_KeepsUnparseableVerbatim(t *testing.T) {
	consumer := []string{"not-an-address", "not-an-address", "also-bad"}
	merged := MergeWarmSets(nil, consumer)
	// Two distinct unparseable strings survive; the dup collapses.
	if len(merged) != 2 {
		t.Fatalf("merged len = %d, want 2 (%v)", len(merged), merged)
	}
}
