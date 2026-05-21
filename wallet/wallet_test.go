package wallet

import (
	"bytes"
	"context"
	"testing"

	"github.com/filecoin-project/go-address"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"

	"github.com/Reiers/lantern/crypto/sigs"
)

func newTestWallet(t *testing.T) *Wallet {
	t.Helper()
	w, err := New(context.Background(), t.TempDir(), "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRoundtripBLS(t *testing.T) {
	w := newTestWallet(t)
	addr, err := w.NewAddress(context.Background(), KTBLS)
	if err != nil {
		t.Fatal(err)
	}
	if addr.Protocol() != address.BLS {
		t.Fatalf("expected BLS protocol, got %v", addr.Protocol())
	}
	msg := []byte("hello lantern")
	sig, err := w.Sign(context.Background(), addr, msg)
	if err != nil {
		t.Fatal(err)
	}
	if sig.Type != gscrypto.SigTypeBLS {
		t.Fatalf("sig type: %v", sig.Type)
	}
	if err := sigs.Verify(sig, addr, msg); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// negative — flipped byte should fail.
	bad := make([]byte, len(sig.Data))
	copy(bad, sig.Data)
	bad[0] ^= 0xff
	if err := sigs.Verify(&gscrypto.Signature{Type: sig.Type, Data: bad}, addr, msg); err == nil {
		t.Fatal("expected verify failure on tampered sig")
	}
}

func TestRoundtripSecp(t *testing.T) {
	w := newTestWallet(t)
	addr, err := w.NewAddress(context.Background(), KTSecp256k1)
	if err != nil {
		t.Fatal(err)
	}
	if addr.Protocol() != address.SECP256K1 {
		t.Fatalf("expected SECP protocol, got %v", addr.Protocol())
	}
	msg := []byte("hello secp")
	sig, err := w.Sign(context.Background(), addr, msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sigs.Verify(sig, addr, msg); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestRoundtripDelegated(t *testing.T) {
	w := newTestWallet(t)
	addr, err := w.NewAddress(context.Background(), KTDelegated)
	if err != nil {
		t.Fatal(err)
	}
	if addr.Protocol() != address.Delegated {
		t.Fatalf("expected Delegated protocol, got %v", addr.Protocol())
	}
	msg := []byte("hello f4")
	sig, err := w.Sign(context.Background(), addr, msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := sigs.Verify(sig, addr, msg); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestExportImport(t *testing.T) {
	w := newTestWallet(t)
	addr, _ := w.NewAddress(context.Background(), KTBLS)
	ki, err := w.Export(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	w2 := newTestWallet(t)
	got, err := w2.Import(context.Background(), ki)
	if err != nil {
		t.Fatal(err)
	}
	if got != addr {
		t.Fatalf("addr roundtrip: %s vs %s", got, addr)
	}
	msg := []byte("compat")
	sig, _ := w2.Sign(context.Background(), got, msg)
	if err := sigs.Verify(sig, got, msg); err != nil {
		t.Fatal(err)
	}
}

func TestListAndDefault(t *testing.T) {
	w := newTestWallet(t)
	a, _ := w.NewAddress(context.Background(), KTBLS)
	b, _ := w.NewAddress(context.Background(), KTSecp256k1)
	addrs, err := w.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 {
		t.Fatalf("len=%d, want 2", len(addrs))
	}
	def, _ := w.Default(context.Background())
	if def != a {
		t.Fatalf("default=%s want %s", def, a)
	}
	if err := w.SetDefault(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	def, _ = w.Default(context.Background())
	if def != b {
		t.Fatalf("default=%s want %s", def, b)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	w1, _ := New(context.Background(), dir, "pw")
	a, _ := w1.NewAddress(context.Background(), KTBLS)
	ki, _ := w1.Export(context.Background(), a)

	w2, _ := New(context.Background(), dir, "pw")
	got, _ := w2.Export(context.Background(), a)
	if !bytes.Equal(got.PrivateKey, ki.PrivateKey) {
		t.Fatal("key did not survive restart")
	}
	addrs, _ := w2.List(context.Background())
	if len(addrs) != 1 || addrs[0] != a {
		t.Fatalf("list mismatch: %v", addrs)
	}
}
