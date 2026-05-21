// Package keystore provides a disk-backed, passphrase-encrypted key store.
//
// On-disk layout: one JSON file per key, name is the key's identifier
// (`wallet-<address>`), contents are AES-GCM-wrapped CBOR-encoded
// crypto.PrivateKey from go-state-types/crypto plus the SigType.
//
// The format is intentionally close to Lotus' lib/keystore: a JSON object
// `{Type: "bls"|"secp256k1"|"delegated", PrivateKey: <base64>}` is
// AES-GCM-encrypted under a key derived from the user's passphrase via
// scrypt. The wrapping envelope is itself JSON:
//
//	{"v":1,"salt":<base64>,"nonce":<base64>,"ct":<base64>}
//
// This means Lantern keystores cannot be opened by raw Lotus, but the
// underlying KeyInfo shape is identical and a one-line export tool can
// produce Lotus-compatible KeyInfo blobs (see Wallet.Export).
package keystore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/scrypt"
)

// KeyInfo is the in-memory representation of a stored key. The PrivateKey
// bytes are raw scalar (secp/delegated: 32-byte d, BLS: 32-byte scalar).
// Type matches go-state-types/crypto.SigType values.
//
// JSON shape matches Lotus' KeyInfo (lib/keystore) so Export/Import is
// directly compatible.
type KeyInfo struct {
	Type       string `json:"Type"`
	PrivateKey []byte `json:"PrivateKey"`
}

// MarshalJSON renders PrivateKey as base64, matching Lotus.
func (k KeyInfo) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type       string `json:"Type"`
		PrivateKey string `json:"PrivateKey"`
	}{
		Type:       k.Type,
		PrivateKey: base64.StdEncoding.EncodeToString(k.PrivateKey),
	})
}

// UnmarshalJSON accepts either a base64 string (Lotus shape) or a raw
// byte-array (Go default).
func (k *KeyInfo) UnmarshalJSON(b []byte) error {
	// Try the Lotus base64 shape first.
	var aux struct {
		Type       string `json:"Type"`
		PrivateKey string `json:"PrivateKey"`
	}
	if err := json.Unmarshal(b, &aux); err == nil && aux.PrivateKey != "" {
		pk, err := base64.StdEncoding.DecodeString(aux.PrivateKey)
		if err != nil {
			return fmt.Errorf("base64-decoding PrivateKey: %w", err)
		}
		k.Type = aux.Type
		k.PrivateKey = pk
		return nil
	}
	// Fallback: raw byte array.
	var raw struct {
		Type       string `json:"Type"`
		PrivateKey []byte `json:"PrivateKey"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	k.Type = raw.Type
	k.PrivateKey = raw.PrivateKey
	return nil
}

// Store is a passphrase-protected, disk-backed key store. Concurrent
// callers can safely share a *Store.
type Store struct {
	dir        string
	passphrase []byte
	mu         sync.RWMutex
}

// Envelope wraps an encrypted KeyInfo on disk.
type envelope struct {
	V     int    `json:"v"`
	Salt  []byte `json:"salt"`
	Nonce []byte `json:"nonce"`
	CT    []byte `json:"ct"`
}

// scryptParams are the KDF parameters used to derive the AES-256 key from
// the passphrase. These match values used by Lotus' keystore in 2025.
const (
	scryptN = 1 << 15 // 32k iterations — modest cost, fast UX
	scryptR = 8
	scryptP = 1
)

// Open opens an existing keystore at dir, creating the directory if it
// doesn't exist. Passphrase is required for every operation.
func Open(dir, passphrase string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("keystore: dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create dir %s: %w", dir, err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("keystore: %s is not a directory", dir)
	}
	// Tighten perms on the directory.
	_ = os.Chmod(dir, 0o700)
	return &Store{dir: dir, passphrase: []byte(passphrase)}, nil
}

// Dir returns the on-disk path.
func (s *Store) Dir() string { return s.dir }

// Put writes ki under name. Overwrites any existing entry.
func (s *Store) Put(name string, ki *KeyInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	plain, err := json.Marshal(ki)
	if err != nil {
		return err
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("salt: %w", err)
	}
	dk, err := scrypt.Key(s.passphrase, salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return fmt.Errorf("scrypt: %w", err)
	}
	block, err := aes.NewCipher(dk)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plain, nil)

	env, err := json.Marshal(envelope{V: 1, Salt: salt, Nonce: nonce, CT: ct})
	if err != nil {
		return err
	}
	path := s.pathFor(name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, env, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// Get returns the KeyInfo stored under name.
func (s *Store) Get(name string) (*KeyInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.pathFor(name)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope %s: %w", name, err)
	}
	if env.V != 1 {
		return nil, fmt.Errorf("unsupported envelope version %d", env.V)
	}
	dk, err := scrypt.Key(s.passphrase, env.Salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, fmt.Errorf("scrypt: %w", err)
	}
	block, err := aes.NewCipher(dk)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, env.Nonce, env.CT, nil)
	if err != nil {
		return nil, ErrBadPassphrase
	}
	var ki KeyInfo
	if err := json.Unmarshal(plain, &ki); err != nil {
		return nil, fmt.Errorf("unmarshal keyinfo: %w", err)
	}
	return &ki, nil
}

// Has returns true if name exists in the store.
func (s *Store) Has(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := os.Stat(s.pathFor(name))
	return err == nil
}

// Delete removes name.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.pathFor(name))
	if os.IsNotExist(err) {
		return ErrKeyNotFound
	}
	return err
}

// List enumerates all key names in the store.
func (s *Store) List() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// SetDefault marks a key as the default by writing a symlink-style pointer
// file `default`.
func (s *Store) SetDefault(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.pathFor(name)); err != nil {
		return ErrKeyNotFound
	}
	return os.WriteFile(filepath.Join(s.dir, "default"), []byte(name), 0o600)
}

// Default returns the current default key name, or "" if unset.
func (s *Store) Default() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, err := os.ReadFile(filepath.Join(s.dir, "default"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// pathFor returns the on-disk path for a given key name.
func (s *Store) pathFor(name string) string {
	return filepath.Join(s.dir, name)
}

// ReadRaw returns the raw envelope file for `name`, mostly for tests
// asserting on-disk encryption.
func (s *Store) ReadRaw(name string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return os.ReadFile(s.pathFor(name))
}

// Errors.
var (
	ErrKeyNotFound   = errors.New("keystore: key not found")
	ErrBadPassphrase = errors.New("keystore: bad passphrase")
)

// EnsureKeystoreDir creates dir with 0700 perms if missing. Returns the
// absolute path.
func EnsureKeystoreDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return abs, nil
}

// Compile-time check.
var _ io.Closer = (*nopCloser)(nil)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
