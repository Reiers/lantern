package evm

import (
	"crypto/sha256"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	decdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck // EVM precompile 0x03 is defined as RIPEMD-160.
)

// Precompile addressing follows builtin-actors
// actors/evm/src/interpreter/precompiles/mod.rs: 0x00..00NN are the
// Ethereum precompiles, 0xfe00..00NN the FEVM native precompiles, plus the
// P256VERIFY special case at 0x00..0100.

const nativePrecompilePrefix = 0xfe

var p256VerifyAddress = Address{18: 0x01}

// isReservedPrecompileAddress mirrors is_reserved_precompile_address: CALLs
// to these addresses never reach an actor.
func isReservedPrecompileAddress(a Address) bool {
	if a == p256VerifyAddress {
		return true
	}
	if a[0] != 0x00 && a[0] != nativePrecompilePrefix {
		return false
	}
	for _, b := range a[1:19] {
		if b != 0 {
			return false
		}
	}
	return a[19] > 0
}

// isFEVMPrecompile mirrors Precompiles::is_precompile (a defined
// precompile, per the actors v19 tables). EXTCODE* report these as
// existing-but-empty.
func isFEVMPrecompile(a Address) bool {
	if !isReservedPrecompileAddress(a) {
		return false
	}
	if a == p256VerifyAddress {
		return true
	}
	idx := a[19]
	if a[0] == nativePrecompilePrefix {
		// resolve_address, lookup_delegated_address, call_actor,
		// (0x04 get_actor_type disabled), call_actor_id, get_randomness
		return idx >= 1 && idx <= 6 && idx != 4
	}
	// 0x01..0x09 Ethereum, 0x0a unused, 0x0b..0x11 BLS12-381
	return idx >= 1 && idx <= 0x11 && idx != 0x0a
}

// callPrecompile runs the precompile at addr. Implemented: ecrecover
// (0x01), sha256 (0x02), ripemd160 (0x03), identity (0x04), each with the
// FEVM's exact output rules (precompiles/evm.rs). Every other reserved
// address faults with ErrUnsupported so the call falls back instead of
// returning the empty success a missing actor would produce.
func (ip *interpreter) callPrecompile(addr Address, input []byte) ([]byte, bool, error) {
	if addr[0] == 0x00 && addr != p256VerifyAddress {
		switch addr[19] {
		case 0x01:
			return ecrecover(input), true, nil
		case 0x02:
			h := sha256.Sum256(input)
			return h[:], true, nil
		case 0x03:
			hh := ripemd160.New()
			hh.Write(input)
			out := make([]byte, 12, 32)
			return hh.Sum(out), true, nil
		case 0x04:
			return append([]byte(nil), input...), true, nil
		}
	}
	return nil, false, fmt.Errorf("%w: precompile %x is not implemented locally", ErrUnsupported, addr)
}

var secp256k1N = func() uint256.Int {
	var n uint256.Int
	_ = n.SetFromHex("0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141")
	return n
}()

// ecrecover implements precompile 0x01 as the FEVM does: input is read as
// hash(32) | v(32) | r(32) | s(32), zero-padded when short; v must be 27
// or 28 (as a full word), r and s in [1, N). Any failure yields empty
// output (the call itself still succeeds). Output is the recovered address
// left-padded to 32 bytes.
func ecrecover(input []byte) []byte {
	var buf [128]byte
	copy(buf[:], input)
	var v, r, s uint256.Int
	v.SetBytes(buf[32:64])
	r.SetBytes(buf[64:96])
	s.SetBytes(buf[96:128])
	if !v.IsUint64() || (v.Uint64() != 27 && v.Uint64() != 28) {
		return nil
	}
	if r.IsZero() || !r.Lt(&secp256k1N) || s.IsZero() || !s.Lt(&secp256k1N) {
		return nil
	}
	// decred compact signature: [27 + recid] || r || s (uncompressed key).
	sig := make([]byte, 65)
	sig[0] = byte(v.Uint64())
	copy(sig[1:33], buf[64:96])
	copy(sig[33:65], buf[96:128])
	pub, _, err := decdsa.RecoverCompact(sig, buf[0:32])
	if err != nil {
		return nil
	}
	return pubkeyToWord(pub)
}

func pubkeyToWord(pub *secp256k1.PublicKey) []byte {
	unc := pub.SerializeUncompressed()
	h := keccak256(unc[1:])
	out := make([]byte, 32)
	copy(out[12:], h[12:])
	return out
}
