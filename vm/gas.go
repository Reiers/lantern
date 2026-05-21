// Pure-Go gas schedule for Filecoin's built-in actor methods.
//
// The constants are the v15 price list (network version 17+) from
// lotus/chain/vm/gas_v15.go. They are stable across the v17/v18 actor
// upgrades that Lantern targets in Phase 7.
//
// We track only the categories Lantern's VM shell actually charges:
//   - `OnChainMessage`: per-message base cost (depends on message size).
//   - `OnChainReturnValue`: per-byte cost for the message return value.
//   - `OnMethodInvocation`: per-method-invocation base cost (depends on
//     whether the call transfers value).
//   - `OnIpldGet` / `OnIpldPut`: per-block IPLD store costs (charged for
//     state reads inside actor methods; in our shell, we approximate
//     these as a fixed per-method count).
//   - `OnHashing` / `OnVerifySignature`: when the message touches a
//     signature-protected actor (BLS / secp / delegated).
//
// All numbers are gas units. Final fee = GasUsed × GasFeeCap (capped by
// GasPremium), exactly as Lotus computes it.

package vm

import (
	"github.com/filecoin-project/go-state-types/big"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
)

// PriceList is the gas schedule applied to a single message.
type PriceList struct {
	// OnChainMessageComputeBase is the constant component of per-message
	// gas. Lotus v15: 38863.
	OnChainMessageComputeBase int64

	// OnChainMessageStorageBase is the per-message base storage cost.
	// Lotus v15: 36 gas per byte over the encoded message length.
	OnChainMessageStoragePerByte int64

	// OnChainReturnValuePerByte is charged per byte returned to the caller.
	// Lotus v15: 1.
	OnChainReturnValuePerByte int64

	// OnMethodInvocation is the per-call base cost. Lotus v15: 75000.
	OnMethodInvocation int64

	// OnMethodInvocationValue is added when the call transfers value
	// (i.e. msg.Value > 0). Lotus v15: 39750.
	OnMethodInvocationValue int64

	// OnIpldGet / OnIpldPut are per-block costs charged inside actor
	// methods. We use the average cost of a HAMT load.
	OnIpldGetBase int64
	OnIpldPutBase int64

	// OnHashingBase is added once per cryptographic hashing operation.
	OnHashingBase int64

	// OnVerifySignatureBLS / Secp / Delegated.
	OnVerifySignatureBLS       int64
	OnVerifySignatureSecp256k1 int64
	OnVerifySignatureDelegated int64
}

// V15PriceList returns the network-version-≥17 gas schedule.
//
// Numbers transcribed from
// `github.com/filecoin-project/lotus/chain/vm/gas_v15.go` at commit
// a0ecb8687 (Lotus 1.36). They are deliberately constants here, not
// imported, to keep this package CGo-free and Lotus-import-free.
func V15PriceList() PriceList {
	return PriceList{
		OnChainMessageComputeBase:    38863,
		OnChainMessageStoragePerByte: 36,
		OnChainReturnValuePerByte:    1,

		OnMethodInvocation:      75000,
		OnMethodInvocationValue: 39750,

		OnIpldGetBase: 75242,
		OnIpldPutBase: 84070,

		OnHashingBase: 31355,

		OnVerifySignatureBLS:       16598605,
		OnVerifySignatureSecp256k1: 1637292,
		OnVerifySignatureDelegated: 1637292,
	}
}

// OnChainMessage computes the per-message base cost (compute + storage).
// `encodedLen` is the CBOR-encoded length of the SignedMessage.
func (p PriceList) OnChainMessage(encodedLen int) int64 {
	return p.OnChainMessageComputeBase +
		p.OnChainMessageStoragePerByte*int64(encodedLen)
}

// OnChainReturnValue charges per byte of the return blob.
func (p PriceList) OnChainReturnValue(retLen int) int64 {
	return p.OnChainReturnValuePerByte * int64(retLen)
}

// OnInvoke returns the per-call base cost. `valueXfer` indicates whether
// the call transfers FIL.
func (p PriceList) OnInvoke(valueXfer bool) int64 {
	g := p.OnMethodInvocation
	if valueXfer {
		g += p.OnMethodInvocationValue
	}
	return g
}

// OnSignature returns the per-signature verification cost for the given
// signature type.
func (p PriceList) OnSignature(sig gscrypto.SigType) int64 {
	switch sig {
	case gscrypto.SigTypeBLS:
		return p.OnVerifySignatureBLS
	case gscrypto.SigTypeSecp256k1:
		return p.OnVerifySignatureSecp256k1
	case gscrypto.SigTypeDelegated:
		return p.OnVerifySignatureDelegated
	default:
		return 0
	}
}

// MaxBlockGas is the per-block gas budget (10 billion units).
//
// This matches `build.BlockGasLimit` in Lotus 1.36.
const MaxBlockGas int64 = 10_000_000_000

// FilBigInt aliases the FIL-token bigint type used by gas math.
type FilBigInt = big.Int
