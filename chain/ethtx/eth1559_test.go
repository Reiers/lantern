package ethtx

import (
	"bytes"
	"testing"

	cbg "github.com/whyrusleeping/cbor-gen"

	gocrypto "github.com/filecoin-project/go-crypto"
	"github.com/filecoin-project/go-keccak"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin"
	statecrypto "github.com/filecoin-project/go-state-types/crypto"
)

// ethAddrFromPriv derives the 20-byte eth address for a secp256k1 private
// key (keccak256(uncompressed_pubkey[1:])[12:]).
func ethAddrFromPriv(t *testing.T, sk []byte) [20]byte {
	t.Helper()
	pub := gocrypto.PublicKey(sk) // 65-byte uncompressed
	if len(pub) != 65 {
		t.Fatalf("pubkey len %d", len(pub))
	}
	h := keccak.NewLegacyKeccak256()
	h.Write(pub[1:])
	d := h.Sum(nil)
	var a [20]byte
	copy(a[:], d[12:])
	return a
}

// signTx fills V/R/S on tx by signing its unsigned RLP with sk, mirroring
// what a wallet does. go-crypto.Sign returns a 65-byte [R||S||V] recoverable
// signature; we split it into the tx's V/R/S big.Ints.
func signTx(t *testing.T, tx *Eth1559Tx, sk []byte) {
	t.Helper()
	hash, err := tx.signingHash()
	if err != nil {
		t.Fatalf("signingHash: %v", err)
	}
	sig, err := gocrypto.Sign(sk, hash)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if len(sig) != 65 {
		t.Fatalf("sig len %d", len(sig))
	}
	tx.R = bytesToBig(sig[0:32])
	tx.S = bytesToBig(sig[32:64])
	tx.V = big.NewIntUnsigned(uint64(sig[64]))
}

func bytesToBig(b []byte) big.Int {
	var m big.Int
	_ = m
	return mustBig(b)
}

func mustBig(b []byte) big.Int {
	v, err := rlpBigInt(b)
	if err != nil {
		panic(err)
	}
	return v
}

// TestRoundTrip_SenderRecovery is the core correctness test: build a tx,
// sign it with a known key, re-encode to signed RLP, decode it back, and
// confirm the recovered sender equals the key's address. This proves the
// RLP codec + signing-hash + ecrecover path are all mutually consistent
// and match the real eth signing scheme.
func TestRoundTrip_SenderRecovery(t *testing.T) {
	sk, err := gocrypto.GenerateKey()
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	want := ethAddrFromPriv(t, sk)

	to := [20]byte{0xac, 0xc0, 0xa0, 0xcf, 0x13, 0x57, 0x1d, 0x30, 0xb4, 0xb8,
		0x63, 0x79, 0x96, 0xf5, 0xd6, 0xd7, 0x74, 0xd4, 0xfd, 0x62}
	tx := &Eth1559Tx{
		ChainID:              314159, // calibration
		Nonce:                0xb854,
		To:                   &to,
		Value:                big.NewInt(0),
		MaxFeePerGas:         big.NewInt(1_500_000_000),
		MaxPriorityFeePerGas: big.NewInt(100_000),
		GasLimit:             5_000_000,
		Input:                []byte{0x12, 0x34, 0x56, 0x78},
	}
	signTx(t, tx, sk)

	// Encode signed -> decode -> recover.
	signed, err := tx.signedRLP()
	if err != nil {
		t.Fatalf("signedRLP: %v", err)
	}
	dec, err := ParseSignedEIP1559(signed)
	if err != nil {
		t.Fatalf("ParseSignedEIP1559: %v", err)
	}

	// Field round-trip.
	if dec.ChainID != tx.ChainID || dec.Nonce != tx.Nonce || dec.GasLimit != tx.GasLimit {
		t.Fatalf("scalar mismatch: got chain=%d nonce=%d gas=%d", dec.ChainID, dec.Nonce, dec.GasLimit)
	}
	if dec.To == nil || *dec.To != to {
		t.Fatalf("To mismatch: %v", dec.To)
	}
	if !bytes.Equal(dec.Input, tx.Input) {
		t.Fatalf("input mismatch: %x", dec.Input)
	}
	if !dec.MaxFeePerGas.Equals(tx.MaxFeePerGas) || !dec.MaxPriorityFeePerGas.Equals(tx.MaxPriorityFeePerGas) {
		t.Fatalf("fee mismatch")
	}

	// The critical assertion: recovered sender == key's address.
	got, err := dec.Sender()
	if err != nil {
		t.Fatalf("Sender: %v", err)
	}
	if got != want {
		t.Fatalf("sender mismatch:\n got  0x%x\n want 0x%x", got, want)
	}
}

// TestToSignedFilecoinMessage_Shape proves the Filecoin message mapping:
// InvokeContract for a contract call, params = CBOR(input), delegated sig.
func TestToSignedFilecoinMessage_Shape(t *testing.T) {
	sk, _ := gocrypto.GenerateKey()
	to := [20]byte{1, 2, 3}
	tx := &Eth1559Tx{
		ChainID: 314159, Nonce: 7, To: &to,
		Value:        big.NewInt(0),
		MaxFeePerGas: big.NewInt(1000), MaxPriorityFeePerGas: big.NewInt(10),
		GasLimit: 2_000_000, Input: []byte{0xde, 0xad, 0xbe, 0xef},
	}
	signTx(t, tx, sk)

	smsg, err := tx.ToSignedFilecoinMessage()
	if err != nil {
		t.Fatalf("ToSignedFilecoinMessage: %v", err)
	}
	if smsg.Message.Method != builtin.MethodsEVM.InvokeContract {
		t.Fatalf("method = %d, want InvokeContract %d", smsg.Message.Method, builtin.MethodsEVM.InvokeContract)
	}
	if smsg.Message.Nonce != 7 {
		t.Fatalf("nonce = %d", smsg.Message.Nonce)
	}
	if smsg.Signature.Type != statecrypto.SigTypeDelegated {
		t.Fatalf("sig type = %d, want delegated", smsg.Signature.Type)
	}
	if len(smsg.Signature.Data) != 65 {
		t.Fatalf("sig data len = %d, want 65", len(smsg.Signature.Data))
	}
	// Params must be CBOR(input): decode it back.
	r := bytes.NewReader(smsg.Message.Params)
	got, err := cbg.ReadByteArray(r, uint64(len(smsg.Message.Params)))
	if err != nil {
		t.Fatalf("params not CBOR byte-array: %v", err)
	}
	if !bytes.Equal(got, tx.Input) {
		t.Fatalf("params decode mismatch: %x", got)
	}
	// From must be the f4/EAM delegated sender.
	wantFrom, _ := tx.SenderFilecoin()
	if smsg.Message.From != wantFrom {
		t.Fatalf("from mismatch")
	}
}

// TestContractCreation maps a nil-To tx to EAM CreateExternal.
func TestContractCreation(t *testing.T) {
	sk, _ := gocrypto.GenerateKey()
	tx := &Eth1559Tx{
		ChainID: 314159, Nonce: 1, To: nil,
		Value:        big.NewInt(0),
		MaxFeePerGas: big.NewInt(1000), MaxPriorityFeePerGas: big.NewInt(10),
		GasLimit: 3_000_000, Input: []byte{0x60, 0x80, 0x60, 0x40},
	}
	signTx(t, tx, sk)
	smsg, err := tx.ToSignedFilecoinMessage()
	if err != nil {
		t.Fatalf("ToSignedFilecoinMessage: %v", err)
	}
	if smsg.Message.Method != builtin.MethodsEAM.CreateExternal {
		t.Fatalf("method = %d, want CreateExternal %d", smsg.Message.Method, builtin.MethodsEAM.CreateExternal)
	}
	if smsg.Message.To != builtin.EthereumAddressManagerActorAddr {
		t.Fatalf("to = %s, want EAM addr", smsg.Message.To)
	}
}

// TestRejectsUnsupportedTypes ensures legacy/2930 are refused.
func TestRejectsUnsupportedTypes(t *testing.T) {
	if _, err := ParseSignedEIP1559([]byte{0x01, 0x00}); err == nil {
		t.Fatal("expected EIP-2930 rejection")
	}
	if _, err := ParseSignedEIP1559([]byte{0xf8, 0x00}); err == nil {
		t.Fatal("expected legacy rejection")
	}
	if _, err := ParseSignedEIP1559(nil); err == nil {
		t.Fatal("expected empty rejection")
	}
}

// TestTxHashStable: TxHash is deterministic and 32 bytes.
func TestTxHashStable(t *testing.T) {
	sk, _ := gocrypto.GenerateKey()
	to := [20]byte{9}
	tx := &Eth1559Tx{
		ChainID: 314159, Nonce: 3, To: &to, Value: big.NewInt(0),
		MaxFeePerGas: big.NewInt(1), MaxPriorityFeePerGas: big.NewInt(1),
		GasLimit: 21000, Input: nil,
	}
	signTx(t, tx, sk)
	h1, err := tx.TxHash()
	if err != nil {
		t.Fatalf("TxHash: %v", err)
	}
	h2, _ := tx.TxHash()
	if h1 != h2 {
		t.Fatal("TxHash not deterministic")
	}
}
