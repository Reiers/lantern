package keystore

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	ki := &KeyInfo{Type: "bls", PrivateKey: []byte("0123456789abcdef0123456789abcdef")}
	if err := s.Put("k1", ki); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("k1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "bls" || !bytes.Equal(got.PrivateKey, ki.PrivateKey) {
		t.Fatalf("mismatch: %+v vs %+v", got, ki)
	}
}

func TestEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "pw")
	secret := []byte("verysecretkeymaterial0123456789a")
	if err := s.Put("k", &KeyInfo{Type: "secp256k1", PrivateKey: secret}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.ReadRaw("k")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, secret) {
		t.Fatalf("plaintext key material leaked on disk")
	}
	if !strings.Contains(string(raw), `"ct"`) {
		t.Fatalf("envelope shape unexpected: %s", string(raw))
	}
}

func TestBadPassphrase(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Open(dir, "right")
	s1.Put("k", &KeyInfo{Type: "bls", PrivateKey: []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")})

	s2, _ := Open(dir, "wrong")
	_, err := s2.Get("k")
	if err != ErrBadPassphrase {
		t.Fatalf("want ErrBadPassphrase, got %v", err)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Open(dir, "pw")
	s1.Put("a", &KeyInfo{Type: "bls", PrivateKey: []byte("11111111111111111111111111111111")})
	s1.Put("b", &KeyInfo{Type: "secp256k1", PrivateKey: []byte("22222222222222222222222222222222")})
	if err := s1.SetDefault("a"); err != nil {
		t.Fatal(err)
	}

	s2, _ := Open(dir, "pw")
	names, err := s2.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 2 {
		t.Fatalf("want >=2 names, got %v", names)
	}
	got, err := s2.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "bls" {
		t.Fatalf("type roundtrip: %s", got.Type)
	}
	def, _ := s2.Default()
	if def != "a" {
		t.Fatalf("default: %s", def)
	}
}

func TestDelete(t *testing.T) {
	s, _ := Open(t.TempDir(), "pw")
	s.Put("x", &KeyInfo{Type: "bls", PrivateKey: bytes.Repeat([]byte{1}, 32)})
	if !s.Has("x") {
		t.Fatal("Has=false")
	}
	if err := s.Delete("x"); err != nil {
		t.Fatal(err)
	}
	if s.Has("x") {
		t.Fatal("still present")
	}
}
