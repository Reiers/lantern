// EIP-1559 Ethereum transaction codec for the Filecoin FEVM write path
// (lantern#45 Stage 3). Decodes a signed raw EIP-1559 tx (0x02-prefixed
// RLP) into its fields, recovers the sender, and converts it to a
// Filecoin SignedMessage with a SigTypeDelegated signature — the exact
// shape MpoolPush / the gossipsub /fil/msgs topic expect.
//
// Ported in shape (NOT by import) from Lotus chain/types/ethtypes, which
// pulls in filecoin-ffi via the chain build and is therefore unusable
// here. We support EIP-1559 only: that's what FEVM / curio-core sends.
// Legacy + EIP-2930 are explicitly rejected.
//
// CGO-free.
package ethtx

import (
	"bytes"
	"fmt"
	mathbig "math/big"

	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/filecoin-project/go-address"
	gocrypto "github.com/filecoin-project/go-crypto"
	"github.com/filecoin-project/go-keccak"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin"
	statecrypto "github.com/filecoin-project/go-state-types/crypto"

	"github.com/Reiers/lantern/chain/types"
)

const (
	// EIP1559TxType is the type byte prefixing an EIP-1559 tx envelope.
	EIP1559TxType = 0x02
	// eip1559SigLen is the delegated signature length (r||s||v).
	eip1559SigLen = 65
)

// ethAddressManagerActorID is the EAM actor ID (f010 /
// EthereumAddressManager). Aliased from go-state-types builtin (a var,
// not a const, hence not in the const block above).
var ethAddressManagerActorID = builtin.EthereumAddressManagerActorID

// Eth1559Tx is the decoded form of an EIP-1559 transaction.
type Eth1559Tx struct {
	ChainID              uint64
	Nonce                uint64
	To                   *[20]byte // nil => contract creation (EAM CreateExternal)
	Value                big.Int
	MaxFeePerGas         big.Int
	MaxPriorityFeePerGas big.Int
	GasLimit             uint64
	Input                []byte

	// Signature components (set when decoding a signed tx).
	V big.Int
	R big.Int
	S big.Int
}

// ParseSignedEIP1559 decodes a signed raw EIP-1559 tx (the bytes a client
// passes to eth_sendRawTransaction, with or without a 0x prefix already
// stripped by the caller).
func ParseSignedEIP1559(data []byte) (*Eth1559Tx, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("ethtx: empty data")
	}
	if data[0] == 0x01 {
		return nil, fmt.Errorf("ethtx: EIP-2930 transactions are not supported")
	}
	if data[0] != EIP1559TxType {
		if data[0] > 0x7f {
			return nil, fmt.Errorf("ethtx: legacy transactions are not supported")
		}
		return nil, fmt.Errorf("ethtx: unsupported transaction type 0x%02x", data[0])
	}

	d, err := DecodeRLP(data[1:])
	if err != nil {
		return nil, err
	}
	decoded, ok := d.([]interface{})
	if !ok {
		return nil, fmt.Errorf("ethtx: EIP-1559 payload is not an RLP list")
	}
	// chainId, nonce, maxPrio, maxFee, gasLimit, to, value, input,
	// accessList, v, r, s
	if len(decoded) != 12 {
		return nil, fmt.Errorf("ethtx: EIP-1559 list must have 12 elements, got %d", len(decoded))
	}

	chainID, err := rlpUint64(decoded[0])
	if err != nil {
		return nil, fmt.Errorf("chainId: %w", err)
	}
	nonce, err := rlpUint64(decoded[1])
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	maxPrio, err := rlpBigInt(decoded[2])
	if err != nil {
		return nil, fmt.Errorf("maxPriorityFeePerGas: %w", err)
	}
	maxFee, err := rlpBigInt(decoded[3])
	if err != nil {
		return nil, fmt.Errorf("maxFeePerGas: %w", err)
	}
	gasLimit, err := rlpUint64(decoded[4])
	if err != nil {
		return nil, fmt.Errorf("gasLimit: %w", err)
	}
	to, err := rlpEthAddr(decoded[5])
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	value, err := rlpBigInt(decoded[6])
	if err != nil {
		return nil, fmt.Errorf("value: %w", err)
	}
	input, err := rlpBytes(decoded[7])
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	// Access list must be present and empty (we don't support entries).
	al, ok := decoded[8].([]interface{})
	if !ok || len(al) != 0 {
		return nil, fmt.Errorf("ethtx: only an empty access list is supported")
	}
	v, err := rlpBigInt(decoded[9])
	if err != nil {
		return nil, fmt.Errorf("v: %w", err)
	}
	r, err := rlpBigInt(decoded[10])
	if err != nil {
		return nil, fmt.Errorf("r: %w", err)
	}
	s, err := rlpBigInt(decoded[11])
	if err != nil {
		return nil, fmt.Errorf("s: %w", err)
	}
	// EIP-1559 only allows v in {0,1}.
	if !v.Equals(big.NewInt(0)) && !v.Equals(big.NewInt(1)) {
		return nil, fmt.Errorf("ethtx: EIP-1559 v must be 0 or 1")
	}

	return &Eth1559Tx{
		ChainID:              chainID,
		Nonce:                nonce,
		To:                   to,
		Value:                value,
		MaxFeePerGas:         maxFee,
		MaxPriorityFeePerGas: maxPrio,
		GasLimit:             gasLimit,
		Input:                input,
		V:                    v,
		R:                    r,
		S:                    s,
	}, nil
}

// unsignedRLP builds the RLP bytes that were signed over: the type byte
// followed by the 9-element list (no v,r,s). This is the preimage for
// the keccak signing hash used in sender recovery.
func (tx *Eth1559Tx) unsignedRLP() ([]byte, error) {
	fields := []interface{}{
		uintToRLP(tx.ChainID),
		uintToRLP(tx.Nonce),
		bigToRLP(tx.MaxPriorityFeePerGas),
		bigToRLP(tx.MaxFeePerGas),
		uintToRLP(tx.GasLimit),
		ethAddrToRLP(tx.To),
		bigToRLP(tx.Value),
		tx.Input,
		[]interface{}{}, // empty access list
	}
	enc, err := EncodeRLP(fields)
	if err != nil {
		return nil, err
	}
	return append([]byte{EIP1559TxType}, enc...), nil
}

// signingHash is keccak256 of the unsigned RLP — the message ecrecover
// runs against.
func (tx *Eth1559Tx) signingHash() ([]byte, error) {
	pre, err := tx.unsignedRLP()
	if err != nil {
		return nil, err
	}
	h := keccak.NewLegacyKeccak256()
	h.Write(pre)
	return h.Sum(nil), nil
}

// sigBytes returns the 65-byte r||s||v compact recoverable signature.
func (tx *Eth1559Tx) sigBytes() []byte {
	sig := make([]byte, 0, 65)
	sig = append(sig, padLeft(tx.R.Int.Bytes(), 32)...)
	sig = append(sig, padLeft(tx.S.Int.Bytes(), 32)...)
	vb := tx.V.Int.Bytes()
	if len(vb) == 0 {
		sig = append(sig, 0)
	} else {
		sig = append(sig, vb[0])
	}
	return sig
}

// Sender recovers the eth sender address (20 bytes) from the signature.
func (tx *Eth1559Tx) Sender() ([20]byte, error) {
	var out [20]byte
	hash, err := tx.signingHash()
	if err != nil {
		return out, err
	}
	pub, err := gocrypto.EcRecover(hash, tx.sigBytes())
	if err != nil {
		return out, fmt.Errorf("ethtx: ecrecover: %w", err)
	}
	// pub is the 65-byte uncompressed pubkey (0x04 || X || Y).
	if len(pub) != 65 {
		return out, fmt.Errorf("ethtx: recovered pubkey is %d bytes, want 65", len(pub))
	}
	h := keccak.NewLegacyKeccak256()
	h.Write(pub[1:]) // drop the 0x04 prefix
	digest := h.Sum(nil)
	copy(out[:], digest[12:]) // last 20 bytes
	return out, nil
}

// SenderFilecoin returns the sender as its f4/EAM delegated Filecoin
// address.
func (tx *Eth1559Tx) SenderFilecoin() (address.Address, error) {
	eth, err := tx.Sender()
	if err != nil {
		return address.Undef, err
	}
	return address.NewDelegatedAddress(ethAddressManagerActorID, eth[:])
}

// ToSignedFilecoinMessage converts the decoded+verified tx into the
// Filecoin SignedMessage shape MpoolPush expects: an unsigned Message
// (from = recovered sender) plus a SigTypeDelegated signature carrying
// the 65-byte r||s||v.
func (tx *Eth1559Tx) ToSignedFilecoinMessage() (*types.SignedMessage, error) {
	from, err := tx.SenderFilecoin()
	if err != nil {
		return nil, err
	}
	msg, err := tx.toUnsignedMessage(from)
	if err != nil {
		return nil, err
	}
	sig := tx.sigBytes()
	if len(sig) != eip1559SigLen {
		return nil, fmt.Errorf("ethtx: delegated signature is %d bytes, want %d", len(sig), eip1559SigLen)
	}
	return &types.SignedMessage{
		Message: *msg,
		Signature: statecrypto.Signature{
			Type: statecrypto.SigTypeDelegated,
			Data: sig,
		},
	}, nil
}

// toUnsignedMessage maps To/Input to the Filecoin (to, method, params)
// triple and assembles the Message.
func (tx *Eth1559Tx) toUnsignedMessage(from address.Address) (*types.Message, error) {
	var params []byte
	if len(tx.Input) > 0 {
		buf := new(bytes.Buffer)
		if err := cbg.WriteByteArray(buf, tx.Input); err != nil {
			return nil, fmt.Errorf("ethtx: encode input params: %w", err)
		}
		params = buf.Bytes()
	}

	var to address.Address
	var method abi.MethodNum
	if tx.To == nil {
		// Contract creation -> EAM CreateExternal.
		method = builtin.MethodsEAM.CreateExternal
		to = builtin.EthereumAddressManagerActorAddr
	} else {
		method = builtin.MethodsEVM.InvokeContract
		var err error
		to, err = address.NewDelegatedAddress(ethAddressManagerActorID, tx.To[:])
		if err != nil {
			return nil, fmt.Errorf("ethtx: build recipient f4 address: %w", err)
		}
	}

	return &types.Message{
		Version:    0,
		To:         to,
		From:       from,
		Nonce:      tx.Nonce,
		Value:      tx.Value,
		GasLimit:   int64(tx.GasLimit),
		GasFeeCap:  tx.MaxFeePerGas,
		GasPremium: tx.MaxPriorityFeePerGas,
		Method:     method,
		Params:     params,
	}, nil
}

// TxHash returns the eth transaction hash = keccak256 of the full signed
// envelope (type byte || signed RLP). This is what eth_sendRawTransaction
// returns to the client.
func (tx *Eth1559Tx) TxHash() ([32]byte, error) {
	var out [32]byte
	signed, err := tx.signedRLP()
	if err != nil {
		return out, err
	}
	h := keccak.NewLegacyKeccak256()
	h.Write(signed)
	copy(out[:], h.Sum(nil))
	return out, nil
}

func (tx *Eth1559Tx) signedRLP() ([]byte, error) {
	fields := []interface{}{
		uintToRLP(tx.ChainID),
		uintToRLP(tx.Nonce),
		bigToRLP(tx.MaxPriorityFeePerGas),
		bigToRLP(tx.MaxFeePerGas),
		uintToRLP(tx.GasLimit),
		ethAddrToRLP(tx.To),
		bigToRLP(tx.Value),
		tx.Input,
		[]interface{}{},
		bigToRLP(tx.V),
		bigToRLP(tx.R),
		bigToRLP(tx.S),
	}
	enc, err := EncodeRLP(fields)
	if err != nil {
		return nil, err
	}
	return append([]byte{EIP1559TxType}, enc...), nil
}

// --- RLP scalar helpers ---------------------------------------------------

func rlpBytes(v interface{}) ([]byte, error) {
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("expected byte string, got %T", v)
	}
	return b, nil
}

func rlpUint64(v interface{}) (uint64, error) {
	b, err := rlpBytes(v)
	if err != nil {
		return 0, err
	}
	if len(b) > 8 {
		return 0, fmt.Errorf("integer is %d bytes, too large for uint64", len(b))
	}
	var out uint64
	for _, x := range b {
		out = (out << 8) | uint64(x)
	}
	return out, nil
}

func rlpBigInt(v interface{}) (big.Int, error) {
	b, err := rlpBytes(v)
	if err != nil {
		return big.Zero(), err
	}
	if len(b) == 0 {
		return big.Zero(), nil
	}
	var m mathbig.Int
	m.SetBytes(b)
	return big.NewFromGo(&m), nil
}

func rlpEthAddr(v interface{}) (*[20]byte, error) {
	b, err := rlpBytes(v)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil // contract creation
	}
	if len(b) != 20 {
		return nil, fmt.Errorf("eth address is %d bytes, want 20", len(b))
	}
	var out [20]byte
	copy(out[:], b)
	return &out, nil
}

func uintToRLP(v uint64) []byte { return trimLeft(bigEndianBytes(v)) }
func bigToRLP(v big.Int) []byte { return trimLeft(v.Int.Bytes()) }
func ethAddrToRLP(a *[20]byte) []byte {
	if a == nil {
		return []byte{}
	}
	return a[:]
}

func trimLeft(b []byte) []byte {
	i := 0
	for i < len(b) && b[i] == 0 {
		i++
	}
	return b[i:]
}

func padLeft(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}
