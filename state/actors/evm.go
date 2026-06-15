// EVM actor loader (lantern#43 Part B, Stage 1).
//
// A deployed FEVM contract is an `evm` actor whose head CBOR decodes to:
//
//	State {
//	    Bytecode      cid.Cid   // CID of the EVM contract bytecode block
//	    BytecodeHash  [32]byte  // keccak256(bytecode bytes)
//	    ContractState cid.Cid   // root CID of the KAMT<U256,U256> storage dict
//	    ...Nonce, TransientData, Tombstone
//	}
//
// This loader fetches + CID-verifies that head block and exposes the two
// CIDs the local-eth_call path needs: the bytecode block and the storage
// (KAMT) root. It does NOT execute anything — the interpreter (Stage 3)
// consumes these. Bytecode-hash verification (BytecodeHash ==
// keccak256(bytecode)) is offered as a helper so the caller can prove the
// fetched bytecode is the one the chain state commits to.
//
// Phase scope mirrors the rest of state/actors: v17 + v18.

package actors

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ipfs/go-cid"
	"golang.org/x/crypto/sha3"

	evm17 "github.com/filecoin-project/go-state-types/builtin/v17/evm"
	evm18 "github.com/filecoin-project/go-state-types/builtin/v18/evm"

	"github.com/Reiers/lantern/state/hamt"
)

// EVMState exposes the read-only handles into a deployed EVM contract's
// on-chain state.
type EVMState interface {
	Version() int
	// BytecodeCID is the CID of the contract's EVM bytecode block.
	BytecodeCID() cid.Cid
	// BytecodeHash is the keccak256 of the bytecode the chain state commits to.
	BytecodeHash() [32]byte
	// StorageRoot is the root CID of the contract's KAMT<U256,U256> storage.
	StorageRoot() cid.Cid
	// Nonce is the CREATE/CREATE2 counter (not the account send-nonce).
	Nonce() uint64
}

// LoadEVM fetches and decodes an EVM (contract) actor's state.
//
// codeCid is the actor's code CID (used for version dispatch), headCid is
// the actor's state head. Both come from accessor.Actor{Code,Head}. The
// head block is CID-verified before decode.
func LoadEVM(ctx context.Context, codeCid, headCid cid.Cid, bg hamt.BlockGetter, reg *Registry) (EVMState, error) {
	info, ok := reg.Lookup(codeCid)
	if !ok {
		return nil, ErrUnknownCode{Code: codeCid}
	}
	if info.Kind != KindEvm {
		return nil, fmt.Errorf("LoadEVM: code %s is %s, not evm", codeCid, info.Kind)
	}
	raw, err := bg.Get(ctx, headCid)
	if err != nil {
		return nil, fmt.Errorf("fetch evm head %s: %w", headCid, err)
	}
	if err := hamt.VerifyBlockCID(headCid, raw); err != nil {
		return nil, fmt.Errorf("evm head: %w", err)
	}
	switch info.Version {
	case 17:
		var s evm17.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding evm v17: %w", err)
		}
		return &evmV17{s: &s}, nil
	case 18:
		var s evm18.State
		if err := s.UnmarshalCBOR(bytes.NewReader(raw)); err != nil {
			return nil, fmt.Errorf("decoding evm v18: %w", err)
		}
		return &evmV18{s: &s}, nil
	}
	return nil, ErrUnsupportedVersion{Kind: KindEvm, Version: info.Version}
}

// FetchBytecode fetches the contract bytecode block referenced by st and
// verifies it against the on-chain BytecodeHash. The bytecode block is an
// IPLD raw block; its CID is verified by VerifyBlockCID, and its keccak256
// is checked against st.BytecodeHash() so the caller knows the bytes are
// exactly what chain state commits to.
func FetchBytecode(ctx context.Context, st EVMState, bg hamt.BlockGetter) ([]byte, error) {
	bcCid := st.BytecodeCID()
	raw, err := bg.Get(ctx, bcCid)
	if err != nil {
		return nil, fmt.Errorf("fetch evm bytecode %s: %w", bcCid, err)
	}
	if err := hamt.VerifyBlockCID(bcCid, raw); err != nil {
		return nil, fmt.Errorf("evm bytecode block: %w", err)
	}
	var want [32]byte = st.BytecodeHash()
	got := keccak256(raw)
	if got != want {
		return nil, fmt.Errorf("evm bytecode hash mismatch: chain commits %x, fetched bytes hash %x", want, got)
	}
	return raw, nil
}

// keccak256 returns the Keccak-256 (not SHA3-256) digest, matching the EVM
// and FEVM BytecodeHash convention.
func keccak256(b []byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ----- version shims -----

type evmV17 struct{ s *evm17.State }

func (e *evmV17) Version() int           { return 17 }
func (e *evmV17) BytecodeCID() cid.Cid   { return e.s.Bytecode }
func (e *evmV17) BytecodeHash() [32]byte { return e.s.BytecodeHash }
func (e *evmV17) StorageRoot() cid.Cid   { return e.s.ContractState }
func (e *evmV17) Nonce() uint64          { return e.s.Nonce }

type evmV18 struct{ s *evm18.State }

func (e *evmV18) Version() int           { return 18 }
func (e *evmV18) BytecodeCID() cid.Cid   { return e.s.Bytecode }
func (e *evmV18) BytecodeHash() [32]byte { return e.s.BytecodeHash }
func (e *evmV18) StorageRoot() cid.Cid   { return e.s.ContractState }
func (e *evmV18) Nonce() uint64          { return e.s.Nonce }
