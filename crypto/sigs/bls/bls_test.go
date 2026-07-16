// Tests for the pure-Go BLS implementation. Verifies against a captured
// (signature, BLS address, message) triple from Lotus' filecoin-ffi-backed
// implementation, plus standard round-trip and malformed-input cases.

package bls_test

import (
	"crypto/rand"
	"testing"

	"github.com/filecoin-project/go-address"
	crypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/crypto/sigs"
	lbls "github.com/Reiers/lantern/crypto/sigs/bls"

	bls12381 "github.com/consensys/gnark-crypto/ecc/bls12-381"
)

// TestPrivateKeyLittleEndian locks the private-key byte order to Filecoin's
// little-endian convention (blst / filecoin-ffi). The scalar 1 serializes
// to [0x01, 0x00, ...]; its public key must be exactly the G1 generator.
// A big-endian misread would treat the same bytes as 2^248 and produce a
// completely different point, so this is a byte-for-byte guard against the
// endianness regression that made real on-chain keys import as "out of
// range" and derive the wrong address.
func TestPrivateKeyLittleEndian(t *testing.T) {
	priv := make([]byte, lbls.PrivateKeyBytes)
	priv[0] = 0x01 // scalar = 1, little-endian

	pub, err := sigs.ToPublic(crypto.SigTypeBLS, priv)
	require.NoError(t, err)

	_, _, g1, _ := bls12381.Generators()
	want := g1.Bytes()
	require.Equal(t, want[:], pub, "ToPublic(scalar=1 LE) must equal the G1 generator")

	// A scalar of 2 (LE [0x02,0x00,...]) must equal 2*G1, i.e. G1 doubled.
	priv2 := make([]byte, lbls.PrivateKeyBytes)
	priv2[0] = 0x02
	pub2, err := sigs.ToPublic(crypto.SigTypeBLS, priv2)
	require.NoError(t, err)
	var dbl bls12381.G1Affine
	dbl.Double(&g1)
	want2 := dbl.Bytes()
	require.Equal(t, want2[:], pub2, "ToPublic(scalar=2 LE) must equal 2*G1")
}

// TestBLSPrivateRoundTrip proves a generated key survives the little-endian
// encode/decode round trip: generate -> pub -> sign -> verify, with the
// private bytes in the on-disk (little-endian) shape the keystore persists.
func TestBLSPrivateRoundTrip(t *testing.T) {
	priv, err := sigs.Generate(crypto.SigTypeBLS)
	require.NoError(t, err)
	require.Len(t, priv, lbls.PrivateKeyBytes)

	pub, err := sigs.ToPublic(crypto.SigTypeBLS, priv)
	require.NoError(t, err)
	addr, err := address.NewBLSAddress(pub)
	require.NoError(t, err)

	msg := []byte("little-endian round trip")
	sig, err := sigs.Sign(crypto.SigTypeBLS, priv, msg)
	require.NoError(t, err)
	require.NoError(t, sigs.Verify(sig, addr, msg))
}

// Test vector lifted from
// github.com/filecoin-project/lotus/lib/sigs/bls/bls_test.go
// TestUncompressedFails. The "compressed" signature is a real filecoin-ffi-
// produced BLS signature on the message "potato" under the given address.
// If our pure-Go verifier agrees, the BLS DST + serialization + pairing all
// match filecoin-ffi byte-for-byte.
func TestLotusReferenceVector_Potato(t *testing.T) {
	sig := []byte{
		0x99, 0x27, 0x44, 0x4b, 0xfc, 0xff, 0xdc, 0xa3, 0x4a, 0xf5, 0x7b, 0x78,
		0x75, 0x7b, 0x9b, 0x90, 0xf1, 0xcd, 0x28, 0xd2, 0xa3, 0xae, 0xed, 0x2a,
		0xa6, 0xbd, 0xe2, 0x99, 0xf8, 0xbb, 0xb9, 0x18, 0x47, 0x56, 0xf2, 0x28,
		0x7b, 0x5, 0x88, 0xe6, 0xd3, 0xf2, 0x86, 0xd, 0x2b, 0xb2, 0x6, 0x6e,
		0xc, 0x59, 0x77, 0x8c, 0x1e, 0x64, 0x4f, 0xb2, 0xcf, 0xb3, 0x5f, 0xba,
		0x8f, 0x9, 0xfa, 0x82, 0x4a, 0x9e, 0xd8, 0x25, 0x10, 0x8c, 0x82, 0xff,
		0x4b, 0xf6, 0x34, 0xc1, 0x3, 0x7e, 0xea, 0xf1, 0x85, 0xf4, 0x56, 0x73,
		0xd4, 0xa1, 0xc1, 0xc6, 0xee, 0xb7, 0x12, 0xb7, 0xd7, 0x2a, 0x54, 0x98,
	}
	addr, err := address.NewFromString("f3tcgq5scpfhdwh4dbalwktzf6mbv3ng2nw7tyzni5cyrsgvineid6jybnweecpa6misa6lk4tvwtxj2gkwpzq")
	require.NoError(t, err)
	msg := []byte("potato")

	// Direct API
	require.NoError(t, lbls.Verify(sig, addr.Payload(), msg), "direct Verify")

	// Through the sigs registry
	require.NoError(t, sigs.Verify(&crypto.Signature{Type: crypto.SigTypeBLS, Data: sig}, addr, msg),
		"sigs.Verify via dispatcher")

	// Flip one byte — should reject.
	bad := append([]byte(nil), sig...)
	bad[40] ^= 0x10
	require.Error(t, lbls.Verify(bad, addr.Payload(), msg), "tampered sig must fail")

	// Different message — should reject.
	require.Error(t, lbls.Verify(sig, addr.Payload(), []byte("tomato")), "wrong message must fail")
}

func TestSignVerifyRoundTrip(t *testing.T) {
	priv, err := sigs.Generate(crypto.SigTypeBLS)
	require.NoError(t, err)
	require.Len(t, priv, lbls.PrivateKeyBytes)

	pub, err := sigs.ToPublic(crypto.SigTypeBLS, priv)
	require.NoError(t, err)
	require.Len(t, pub, lbls.PublicKeyBytes)

	addr, err := address.NewBLSAddress(pub)
	require.NoError(t, err)

	msg := []byte("lantern test message")
	sig, err := sigs.Sign(crypto.SigTypeBLS, priv, msg)
	require.NoError(t, err)
	require.Equal(t, crypto.SigTypeBLS, sig.Type)
	require.Len(t, sig.Data, lbls.SignatureBytes)

	require.NoError(t, sigs.Verify(sig, addr, msg))

	// Tamper with message — should fail.
	require.Error(t, sigs.Verify(sig, addr, []byte("not the same")))
}

func TestAggregateAndHashVerify(t *testing.T) {
	const n = 5
	pubs := make([][]byte, n)
	sigsList := make([][]byte, n)
	msgs := make([][]byte, n)

	for i := 0; i < n; i++ {
		priv, err := sigs.Generate(crypto.SigTypeBLS)
		require.NoError(t, err)
		pub, err := sigs.ToPublic(crypto.SigTypeBLS, priv)
		require.NoError(t, err)
		pubs[i] = pub

		// Each signer signs a different message.
		var buf [16]byte
		_, _ = rand.Read(buf[:])
		msgs[i] = buf[:]
		s, err := sigs.Sign(crypto.SigTypeBLS, priv, msgs[i])
		require.NoError(t, err)
		sigsList[i] = s.Data
	}

	agg, err := lbls.Aggregate(sigsList...)
	require.NoError(t, err)
	require.Len(t, agg, lbls.SignatureBytes)

	require.NoError(t, lbls.HashVerify(agg, msgs, pubs))

	// Tamper with one message — aggregate must fail.
	msgs[2] = append([]byte(nil), msgs[2]...)
	msgs[2][0] ^= 0xff
	require.Error(t, lbls.HashVerify(agg, msgs, pubs))
}
