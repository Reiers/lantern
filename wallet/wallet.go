// Wallet is the high-level facade combining a keystore + the sigs registry.
// All RPC handlers route through this type.

package wallet

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-keccak"
	gsbuiltin "github.com/filecoin-project/go-state-types/builtin"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"

	"github.com/Reiers/lantern/crypto/sigs"
	_ "github.com/Reiers/lantern/crypto/sigs/bls"
	_ "github.com/Reiers/lantern/crypto/sigs/delegated"
	_ "github.com/Reiers/lantern/crypto/sigs/secp"
	"github.com/Reiers/lantern/wallet/keystore"
)

// KeyType is a stable string identifier for one of the three key types.
// Mirrors Lotus' lib/wallet/key.KeyType values so import/export with Lotus
// JSON KeyInfo files works.
type KeyType string

const (
	KTBLS       KeyType = "bls"
	KTSecp256k1 KeyType = "secp256k1"
	KTDelegated KeyType = "delegated"
)

// sigType returns the go-state-types/crypto.SigType for a KeyType.
func (kt KeyType) sigType() (gscrypto.SigType, error) {
	switch kt {
	case KTBLS:
		return gscrypto.SigTypeBLS, nil
	case KTSecp256k1:
		return gscrypto.SigTypeSecp256k1, nil
	case KTDelegated:
		return gscrypto.SigTypeDelegated, nil
	}
	return 0, fmt.Errorf("unknown key type %q", kt)
}

// keyTypeFromSig returns the KeyType for a go-state-types SigType.
func keyTypeFromSig(t gscrypto.SigType) (KeyType, error) {
	switch t {
	case gscrypto.SigTypeBLS:
		return KTBLS, nil
	case gscrypto.SigTypeSecp256k1:
		return KTSecp256k1, nil
	case gscrypto.SigTypeDelegated:
		return KTDelegated, nil
	}
	return "", fmt.Errorf("unknown sig type %d", t)
}

// KeyInfo is the Lotus-compatible export shape. Lotus uses
// {Type: string, PrivateKey: []byte (base64 JSON)}.
type KeyInfo = keystore.KeyInfo

// Wallet wraps an encrypted keystore and exposes Lotus-compatible
// operations.
type Wallet struct {
	store *keystore.Store

	mu      sync.RWMutex
	pubKeys map[address.Address][]byte // cached pubkey by address
}

// New opens (or creates) a wallet at the given keystore directory.
func New(_ context.Context, dir, passphrase string) (*Wallet, error) {
	st, err := keystore.Open(dir, passphrase)
	if err != nil {
		return nil, err
	}
	return &Wallet{store: st, pubKeys: map[address.Address][]byte{}}, nil
}

// keyName returns the filename for a key.
func keyName(addr address.Address) string {
	return "wallet-" + addr.String()
}

// NewAddress generates a new key of the requested type, stores it, and
// returns its address.
func (w *Wallet) NewAddress(_ context.Context, kt KeyType) (address.Address, error) {
	st, err := kt.sigType()
	if err != nil {
		return address.Undef, err
	}
	priv, err := sigs.Generate(st)
	if err != nil {
		return address.Undef, fmt.Errorf("generate %s key: %w", kt, err)
	}
	pub, err := sigs.ToPublic(st, priv)
	if err != nil {
		return address.Undef, fmt.Errorf("derive pubkey: %w", err)
	}
	addr, err := publicKeyToAddress(kt, pub)
	if err != nil {
		return address.Undef, err
	}
	ki := &keystore.KeyInfo{Type: string(kt), PrivateKey: priv}
	if err := w.store.Put(keyName(addr), ki); err != nil {
		return address.Undef, err
	}
	w.mu.Lock()
	w.pubKeys[addr] = pub
	w.mu.Unlock()
	// Set default if first.
	if d, _ := w.store.Default(); d == "" {
		_ = w.store.SetDefault(keyName(addr))
	}
	return addr, nil
}

// Has reports whether the wallet holds the private key for addr.
func (w *Wallet) Has(_ context.Context, addr address.Address) (bool, error) {
	return w.store.Has(keyName(addr)), nil
}

// List returns all addresses in the wallet.
func (w *Wallet) List(_ context.Context) ([]address.Address, error) {
	names, err := w.store.List()
	if err != nil {
		return nil, err
	}
	var out []address.Address
	for _, n := range names {
		if !startsWith(n, "wallet-") {
			continue
		}
		s := n[len("wallet-"):]
		a, err := address.NewFromString(s)
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// Sign signs raw bytes with addr's key. msg is hashed/encoded by the
// appropriate signer per Filecoin convention (BLS hash-to-G2, secp/keccak
// over blake2b, etc.).
func (w *Wallet) Sign(_ context.Context, addr address.Address, msg []byte) (*gscrypto.Signature, error) {
	ki, err := w.store.Get(keyName(addr))
	if err != nil {
		return nil, err
	}
	kt := KeyType(ki.Type)
	st, err := kt.sigType()
	if err != nil {
		return nil, err
	}
	return sigs.Sign(st, ki.PrivateKey, msg)
}

// Export returns a Lotus-compatible KeyInfo blob for backup/migration.
func (w *Wallet) Export(_ context.Context, addr address.Address) (*KeyInfo, error) {
	return w.store.Get(keyName(addr))
}

// Import inserts a Lotus KeyInfo, returns the derived address.
func (w *Wallet) Import(_ context.Context, ki *KeyInfo) (address.Address, error) {
	kt := KeyType(ki.Type)
	st, err := kt.sigType()
	if err != nil {
		return address.Undef, err
	}
	pub, err := sigs.ToPublic(st, ki.PrivateKey)
	if err != nil {
		return address.Undef, err
	}
	addr, err := publicKeyToAddress(kt, pub)
	if err != nil {
		return address.Undef, err
	}
	if err := w.store.Put(keyName(addr), ki); err != nil {
		return address.Undef, err
	}
	w.mu.Lock()
	w.pubKeys[addr] = pub
	w.mu.Unlock()
	return addr, nil
}

// Delete removes addr's key from the store.
func (w *Wallet) Delete(_ context.Context, addr address.Address) error {
	return w.store.Delete(keyName(addr))
}

// SetDefault marks addr as the default address.
func (w *Wallet) SetDefault(_ context.Context, addr address.Address) error {
	return w.store.SetDefault(keyName(addr))
}

// Default returns the default address, or ErrNoDefault if none is set.
func (w *Wallet) Default(_ context.Context) (address.Address, error) {
	name, err := w.store.Default()
	if err != nil {
		return address.Undef, err
	}
	if name == "" {
		// fall back to first available
		all, err := w.List(context.Background())
		if err != nil || len(all) == 0 {
			return address.Undef, ErrNoDefault
		}
		return all[0], nil
	}
	if !startsWith(name, "wallet-") {
		return address.Undef, fmt.Errorf("invalid default name %q", name)
	}
	return address.NewFromString(name[len("wallet-"):])
}

// ErrNoDefault is returned when Default() is called with an empty wallet.
var ErrNoDefault = errors.New("wallet: no default address set")

// publicKeyToAddress derives the Filecoin address from a (kt, pubkey) pair.
func publicKeyToAddress(kt KeyType, pub []byte) (address.Address, error) {
	switch kt {
	case KTBLS:
		return address.NewBLSAddress(pub)
	case KTSecp256k1:
		return address.NewSecp256k1Address(pub)
	case KTDelegated:
		// Mirror lotus/lib/sigs/delegated.Verify: keccak256 of the
		// (uncompressed-stripped) pubkey, low 20 bytes as the Ethereum
		// address payload under the EAM (Ethereum Address Manager) actor.
		stripped := pub
		if len(stripped) > 0 && stripped[0] == 0x04 {
			stripped = stripped[1:]
		}
		h := keccak.NewLegacyKeccak256()
		h.Write(stripped)
		sum := h.Sum(nil)
		return address.NewDelegatedAddress(gsbuiltin.EthereumAddressManagerActorID, sum[12:])
	}
	return address.Undef, fmt.Errorf("address derivation: unsupported key type %q", kt)
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// SignatureType reports the signature type for the key at addr (used by
// callers that need to pick BLS aggregate vs secp single-sig path).
func (w *Wallet) SignatureType(_ context.Context, addr address.Address) (gscrypto.SigType, error) {
	ki, err := w.store.Get(keyName(addr))
	if err != nil {
		return 0, err
	}
	return KeyType(ki.Type).sigType()
}
