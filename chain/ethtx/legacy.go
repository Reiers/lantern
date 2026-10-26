package ethtx

// Legacy (pre EIP-2718) Ethereum transactions as Filecoin carries them:
// a SigTypeDelegated message whose signature is prefixed with a marker
// byte (0x01 homestead, 0x02 EIP-155). Lantern never builds these (the
// write path is EIP-1559 only) but they appear on chain, so the read path
// must hash and render them exactly like Lotus
// (chain/types/ethtypes/eth_legacy_{homestead,155}_transactions.go).

import (
	"fmt"

	"github.com/filecoin-project/go-keccak"
	"github.com/filecoin-project/go-state-types/big"
)

const (
	// LegacyTxType is the eth "type" of a legacy transaction.
	LegacyTxType = 0x00
	// LegacyHomesteadSigPrefix marks a homestead (unprotected) legacy tx.
	LegacyHomesteadSigPrefix = 0x01
	// Legacy155SigPrefix marks an EIP-155 (chain-id protected) legacy tx.
	Legacy155SigPrefix = 0x02
	// legacyHomesteadSigLen is prefix || r || s || v (v = 27/28).
	legacyHomesteadSigLen = 66
)

// LegacyTx is a decoded legacy transaction (homestead or EIP-155).
type LegacyTx struct {
	Nonce    uint64
	GasPrice big.Int
	GasLimit uint64
	To       *[20]byte // nil => contract creation
	Value    big.Int
	Input    []byte

	// EIP155 is true for chain-id protected txs (signature prefix 0x02).
	EIP155 bool

	V big.Int
	R big.Int
	S big.Int
}

// Legacy155SigLen returns the delegated signature length of an EIP-155
// legacy tx for chainID: prefix || r || s || be(chainID*2+recBase). Lotus
// calcEIP155TxSignatureLen.
func Legacy155SigLen(chainID uint64, recBase uint64) int {
	v := chainID*2 + recBase
	return 1 + 64 + len(trimLeft(bigEndianBytes(v)))
}

// IsLegacySigLen reports whether a delegated signature length is one of
// the legacy forms Lotus accepts for chainID.
func IsLegacySigLen(n int, chainID uint64) bool {
	return n == legacyHomesteadSigLen || n == Legacy155SigLen(chainID, 35) || n == Legacy155SigLen(chainID, 36)
}

// SetLegacySignature fills V/R/S from a prefixed legacy delegated
// signature, validating it the way Lotus InitialiseSignature does.
func (tx *LegacyTx) SetLegacySignature(sig []byte, chainID uint64) error {
	if len(sig) < legacyHomesteadSigLen {
		return fmt.Errorf("ethtx: legacy signature too short (%d bytes)", len(sig))
	}
	switch sig[0] {
	case LegacyHomesteadSigPrefix:
		if len(sig) != legacyHomesteadSigLen {
			return fmt.Errorf("ethtx: homestead signature must be %d bytes, got %d", legacyHomesteadSigLen, len(sig))
		}
		v := bytesToBigInt(sig[65:66])
		if !v.Equals(big.NewInt(27)) && !v.Equals(big.NewInt(28)) {
			return fmt.Errorf("ethtx: legacy homestead transactions only support 27 or 28 for v")
		}
		tx.EIP155 = false
		tx.R = bytesToBigInt(sig[1:33])
		tx.S = bytesToBigInt(sig[33:65])
		tx.V = v
		return nil
	case Legacy155SigPrefix:
		if len(sig) != Legacy155SigLen(chainID, 35) && len(sig) != Legacy155SigLen(chainID, 36) {
			return fmt.Errorf("ethtx: eip-155 signature has unexpected length %d", len(sig))
		}
		v := bytesToBigInt(sig[65:])
		// Lotus validateEIP155ChainId: (v - 35) / 2 must equal chainID.
		derived := big.Div(big.Sub(v, big.NewInt(35)), big.NewInt(2))
		if !derived.Equals(big.NewIntUnsigned(chainID)) {
			return fmt.Errorf("ethtx: eip-155 v %s does not encode chain id %d", v, chainID)
		}
		tx.EIP155 = true
		tx.R = bytesToBigInt(sig[1:33])
		tx.S = bytesToBigInt(sig[33:65])
		tx.V = v
		return nil
	default:
		return fmt.Errorf("ethtx: unsupported legacy transaction; first byte of signature is %d", sig[0])
	}
}

// TxHash is keccak256(rlp([nonce, gasPrice, gas, to, value, input, v, r,
// s])), identical for homestead and EIP-155 (Lotus TxHash for both).
func (tx *LegacyTx) TxHash() ([32]byte, error) {
	var out [32]byte
	enc, err := EncodeRLP([]interface{}{
		uintToRLP(tx.Nonce),
		bigToRLP(tx.GasPrice),
		uintToRLP(tx.GasLimit),
		ethAddrToRLP(tx.To),
		bigToRLP(tx.Value),
		tx.Input,
		bigToRLP(tx.V),
		bigToRLP(tx.R),
		bigToRLP(tx.S),
	})
	if err != nil {
		return out, err
	}
	h := keccak.NewLegacyKeccak256()
	h.Write(enc)
	copy(out[:], h.Sum(nil))
	return out, nil
}
