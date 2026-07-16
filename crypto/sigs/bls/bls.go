// Lantern pure-Go BLS12-381 signature verification, written for this project.
// Not copied from Lotus or filecoin-ffi.
//
// Scheme: Filecoin uses the "minimum-pubkey-size" variant of the IETF BLS
// draft. Public keys are 48-byte compressed G1 points, signatures are
// 96-byte compressed G2 points, messages are hashed to G2 via SSWU with the
// domain separation tag:
//
//	BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_
//
// This matches go-f3/internal/gnark and filecoin-ffi.

package bls

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"

	"github.com/filecoin-project/go-address"
	crypto2 "github.com/filecoin-project/go-state-types/crypto"

	"github.com/Reiers/lantern/crypto/sigs"

	bls12381 "github.com/consensys/gnark-crypto/ecc/bls12-381"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"
)

// DST is the Filecoin BLS domain separation tag.
const DST = "BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_"

// Sizes of compressed serialized group elements, matching filecoin-ffi.
const (
	PrivateKeyBytes = 32
	PublicKeyBytes  = 48
	SignatureBytes  = 96
)

// Verify checks that sig is a valid Filecoin BLS signature on msg under
// pubkey. pubkey is the compressed 48-byte G1 encoding, sig is the
// compressed 96-byte G2 encoding.
func Verify(sig []byte, pubkey []byte, msg []byte) error {
	if len(sig) != SignatureBytes {
		return fmt.Errorf("bls: invalid signature length %d, want %d", len(sig), SignatureBytes)
	}
	if len(pubkey) != PublicKeyBytes {
		return fmt.Errorf("bls: invalid pubkey length %d, want %d", len(pubkey), PublicKeyBytes)
	}

	var pk bls12381.G1Affine
	if _, err := pk.SetBytes(pubkey); err != nil {
		return fmt.Errorf("bls: invalid pubkey: %w", err)
	}
	if pk.IsInfinity() {
		return errors.New("bls: pubkey is the identity")
	}

	var sigPt bls12381.G2Affine
	if _, err := sigPt.SetBytes(sig); err != nil {
		return fmt.Errorf("bls: invalid signature point: %w", err)
	}

	hashed, err := bls12381.HashToG2(msg, []byte(DST))
	if err != nil {
		return fmt.Errorf("bls: hash to G2: %w", err)
	}

	return pairingCheckOne(pk, sigPt, hashed)
}

// HashVerify checks an aggregated BLS signature over a sequence of
// (message, pubkey) pairs. Returns nil iff
//
//	e(G1, sig) == prod_i e(pk_i, H(msg_i)).
//
// This mirrors filecoin-ffi's HashVerify, which is what Lotus uses to verify
// the BlockHeader.BLSAggregate field over the in-block BLS messages.
func HashVerify(sig []byte, msgs [][]byte, pubkeys [][]byte) error {
	if len(msgs) != len(pubkeys) {
		return fmt.Errorf("bls: msg count %d != pubkey count %d", len(msgs), len(pubkeys))
	}
	if len(msgs) == 0 {
		return nil
	}
	if len(sig) != SignatureBytes {
		return fmt.Errorf("bls: invalid signature length %d", len(sig))
	}

	var sigPt bls12381.G2Affine
	if _, err := sigPt.SetBytes(sig); err != nil {
		return fmt.Errorf("bls: invalid signature point: %w", err)
	}

	_, _, g1Aff, _ := bls12381.Generators()

	// LHS: e(-G1, sig).
	var negG1 bls12381.G1Affine
	negG1.Neg(&g1Aff)

	g1Pts := make([]bls12381.G1Affine, 0, len(msgs)+1)
	g2Pts := make([]bls12381.G2Affine, 0, len(msgs)+1)
	g1Pts = append(g1Pts, negG1)
	g2Pts = append(g2Pts, sigPt)

	for i, msg := range msgs {
		if len(pubkeys[i]) != PublicKeyBytes {
			return fmt.Errorf("bls: pubkey %d has invalid length %d", i, len(pubkeys[i]))
		}
		var pk bls12381.G1Affine
		if _, err := pk.SetBytes(pubkeys[i]); err != nil {
			return fmt.Errorf("bls: pubkey %d invalid: %w", i, err)
		}
		if pk.IsInfinity() {
			return fmt.Errorf("bls: pubkey %d is identity", i)
		}
		h, err := bls12381.HashToG2(msg, []byte(DST))
		if err != nil {
			return fmt.Errorf("bls: hash to G2 for msg %d: %w", i, err)
		}
		g1Pts = append(g1Pts, pk)
		g2Pts = append(g2Pts, h)
	}

	ok, err := bls12381.PairingCheck(g1Pts, g2Pts)
	if err != nil {
		return fmt.Errorf("bls: pairing check failed: %w", err)
	}
	if !ok {
		return errors.New("bls: aggregate signature did not verify")
	}
	return nil
}

// Aggregate combines a list of BLS signatures (each 96-byte compressed G2)
// into a single 96-byte aggregate signature via point addition on G2.
func Aggregate(sigs ...[]byte) ([]byte, error) {
	if len(sigs) == 0 {
		return nil, errors.New("bls: cannot aggregate empty signature set")
	}
	var acc bls12381.G2Affine
	var accJac bls12381.G2Jac
	for i, s := range sigs {
		if len(s) != SignatureBytes {
			return nil, fmt.Errorf("bls: signature %d has invalid length %d", i, len(s))
		}
		var p bls12381.G2Affine
		if _, err := p.SetBytes(s); err != nil {
			return nil, fmt.Errorf("bls: signature %d invalid: %w", i, err)
		}
		if i == 0 {
			accJac.FromAffine(&p)
		} else {
			accJac.AddMixed(&p)
		}
	}
	acc.FromJacobian(&accJac)
	out := acc.Bytes()
	return out[:], nil
}

// pairingCheckOne checks e(G1, sig) == e(pubkey, H) using PairingCheck on
// {(-G1, sig), (pubkey, H)}.
func pairingCheckOne(pk bls12381.G1Affine, sig bls12381.G2Affine, h bls12381.G2Affine) error {
	_, _, g1Aff, _ := bls12381.Generators()
	var negG1 bls12381.G1Affine
	negG1.Neg(&g1Aff)

	g1Pts := []bls12381.G1Affine{negG1, pk}
	g2Pts := []bls12381.G2Affine{sig, h}

	ok, err := bls12381.PairingCheck(g1Pts, g2Pts)
	if err != nil {
		return fmt.Errorf("bls: pairing check: %w", err)
	}
	if !ok {
		return errors.New("bls: signature did not verify")
	}
	return nil
}

// blsSigner implements sigs.SigShim for SigTypeBLS, plugging into Lantern's
// dispatcher so wallet and chain validation code can call sigs.Verify(...) /
// sigs.Sign(...) uniformly.
type blsSigner struct{}

// scalarFromPrivate decodes a 32-byte Filecoin BLS private key into its
// field scalar. Filecoin serializes the scalar in LITTLE-endian byte
// order (the blst_scalar_from_lendian convention shared by blst and
// filecoin-ffi, and therefore by every BLS key on the network). gnark's
// big.Int arithmetic is big-endian, so we reverse the bytes before
// interpreting them. Getting this wrong yields a different scalar, hence
// a different public key / address and signatures that never verify
// on-chain (it also trips the range check as a near-certain "out of
// range" for real keys).
func scalarFromPrivate(priv []byte) (*big.Int, error) {
	if len(priv) != PrivateKeyBytes {
		return nil, fmt.Errorf("bls: invalid private key length %d, want %d", len(priv), PrivateKeyBytes)
	}
	le := make([]byte, PrivateKeyBytes)
	for i := 0; i < PrivateKeyBytes; i++ {
		le[i] = priv[PrivateKeyBytes-1-i]
	}
	k := new(big.Int).SetBytes(le)
	if k.Sign() == 0 || k.Cmp(fr.Modulus()) >= 0 {
		return nil, errors.New("bls: private key out of range")
	}
	return k, nil
}

// privateFromScalar serializes a scalar back into Filecoin's little-endian
// 32-byte private key encoding, the inverse of scalarFromPrivate. Keeping
// generation and parsing on the same convention is what lets a key minted
// here round-trip through `wallet export` into any other Filecoin tool.
func privateFromScalar(k *big.Int) []byte {
	be := make([]byte, PrivateKeyBytes)
	k.FillBytes(be) // big-endian, left-padded to 32 bytes
	out := make([]byte, PrivateKeyBytes)
	for i := 0; i < PrivateKeyBytes; i++ {
		out[i] = be[PrivateKeyBytes-1-i]
	}
	return out
}

func (blsSigner) GenPrivate() ([]byte, error) {
	// Sample a uniformly random scalar in [1, r-1]; r is the BLS12-381 G2
	// scalar field order. Rejection sampling on 32 random bytes, then emit
	// the scalar in Filecoin's little-endian private-key encoding.
	mod := fr.Modulus()
	for attempt := 0; attempt < 16; attempt++ {
		var buf [PrivateKeyBytes]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, fmt.Errorf("bls: gen private key: %w", err)
		}
		k := new(big.Int).SetBytes(buf[:])
		if k.Sign() == 0 || k.Cmp(mod) >= 0 {
			continue
		}
		return privateFromScalar(k), nil
	}
	return nil, errors.New("bls: failed to sample private key after retries")
}

func (blsSigner) ToPublic(priv []byte) ([]byte, error) {
	k, err := scalarFromPrivate(priv)
	if err != nil {
		return nil, err
	}
	_, _, g1Aff, _ := bls12381.Generators()
	var pk bls12381.G1Affine
	pk.ScalarMultiplication(&g1Aff, k)
	out := pk.Bytes()
	return out[:], nil
}

func (blsSigner) Sign(priv []byte, msg []byte) ([]byte, error) {
	k, err := scalarFromPrivate(priv)
	if err != nil {
		return nil, err
	}
	h, err := bls12381.HashToG2(msg, []byte(DST))
	if err != nil {
		return nil, fmt.Errorf("bls: hash to G2: %w", err)
	}
	var sig bls12381.G2Affine
	sig.ScalarMultiplication(&h, k)
	out := sig.Bytes()
	return out[:], nil
}

func (blsSigner) Verify(sig []byte, a address.Address, msg []byte) error {
	if a.Protocol() != address.BLS {
		return fmt.Errorf("bls: address is not BLS protocol: %s", a)
	}
	return Verify(sig, a.Payload(), msg)
}

func init() {
	sigs.RegisterSignature(crypto2.SigTypeBLS, blsSigner{})
}
