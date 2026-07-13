package fvmkernel

// Consensus-fault detection in pure Go (lantern#130 Tier 1).
//
// The FVM's `crypto.verify_consensus_fault` syscall is called by the
// miner actor to slash a miner that produced two conflicting blocks.
// A consensus fault is one of three shapes:
//
//   1. DoubleForkMining  -- two blocks by the same miner at the SAME
//                           epoch (regardless of parent tipset).
//   2. TimeOffsetMining  -- two blocks by the same miner with the SAME
//                           parent tipset but different epochs (a way
//                           to grind on the ticket / election proof).
//   3. ParentGrinding    -- with an EXTRA header at parent-epoch H-1,
//                           a block at epoch H whose parent tipset does
//                           NOT include the extra block, even though
//                           the same miner produced it.
//
// Header parsing (CBOR) + the three condition checks are pure Go and
// live here. BLS signature verification of both headers under the
// miner's worker key -- required for consensus safety -- is delegated
// to a SignatureVerifier interface, defaulting to a strict rejecter
// that treats any check as "not yet supported." Filling that in is
// #130 Tier 2 (shares the crypto core with #88).

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/ipfs/go-cid"
)

// Consensus-fault type IDs, matching fvm_shared::consensus::fault::ConsensusFaultType.
const (
	ConsensusFaultNone             uint32 = 0
	ConsensusFaultDoubleForkMining uint32 = 1
	ConsensusFaultTimeOffsetMining uint32 = 2
	ConsensusFaultParentGrinding   uint32 = 3
)

// ConsensusFaultResult mirrors fvm_shared::sys::out::crypto::VerifyConsensusFault.
// Layout (packed C):
//
//	epoch    i64
//	target   u64
//	fault_ty u32
//	_padding u32
type ConsensusFaultResult struct {
	Epoch     int64
	Target    uint64
	FaultType uint32
}

// Bytes returns the 24-byte packed encoding used by the syscall.
func (r ConsensusFaultResult) Bytes() []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint64(b[0:], uint64(r.Epoch))
	binary.LittleEndian.PutUint64(b[8:], r.Target)
	binary.LittleEndian.PutUint32(b[16:], r.FaultType)
	// b[20:24] pad zeros
	return b
}

// BlockHeader is the minimal parsed shape needed for fault detection.
// It intentionally does NOT model every field; only what the fault-type
// tests read (miner, height, parent tipset, and signature bytes for
// verify) is kept. Fields we don't need are skipped during CBOR parsing.
type BlockHeader struct {
	Miner        Address   // f0/f1 address of the block producer
	Parents      []cid.Cid // parent tipset CIDs
	Height       int64     // epoch
	BLSSignature []byte    // signature block; verified with miner's worker key
}

// SignatureVerifier verifies a signed block header. #130 Tier 1 ships a
// strict rejecter (RejectAllVerifier); a real verifier lives with the
// crypto core in #88 and gets wired for Tier 2.
type SignatureVerifier interface {
	// VerifyBlockHeader returns nil if `hdr`'s signature is valid under
	// the worker key of `hdr.Miner`. Returns an error otherwise.
	VerifyBlockHeader(hdr *BlockHeader) error
}

// RejectAllVerifier fails every check. Used as the safe default so the
// prototype cannot report a fault without an explicit real verifier.
type RejectAllVerifier struct{}

func (RejectAllVerifier) VerifyBlockHeader(_ *BlockHeader) error {
	return fmt.Errorf("signature verification not yet wired (shares crypto core with #88)")
}

// AcceptAllVerifier accepts every check. TEST-ONLY -- lets tests drive
// the fault-detection logic in isolation from the signature layer.
type AcceptAllVerifier struct{}

func (AcceptAllVerifier) VerifyBlockHeader(_ *BlockHeader) error { return nil }

// VerifyConsensusFault runs the fault-detection algorithm over two
// (optionally three) parsed block headers. Returns the fault type +
// target + epoch, or ConsensusFaultNone if no fault is present or if
// signature verification fails.
//
// Ordering rule: h1 and h2 are the two supposedly-conflicting blocks;
// `extra` (may be nil) is the parent-grinding candidate at epoch H-1.
func VerifyConsensusFault(h1, h2, extra *BlockHeader, verifier SignatureVerifier) ConsensusFaultResult {
	if h1 == nil || h2 == nil {
		return ConsensusFaultResult{}
	}
	// The two headers must claim the same miner and NOT be the same block.
	if h1.Miner.String() != h2.Miner.String() {
		return ConsensusFaultResult{}
	}
	if headersEqual(h1, h2) {
		return ConsensusFaultResult{}
	}
	// Signature check on both -- required for consensus safety.
	if err := verifier.VerifyBlockHeader(h1); err != nil {
		return ConsensusFaultResult{}
	}
	if err := verifier.VerifyBlockHeader(h2); err != nil {
		return ConsensusFaultResult{}
	}

	// (1) DoubleForkMining: same miner, same height.
	if h1.Height == h2.Height {
		id, _ := h1.Miner.IDValue()
		return ConsensusFaultResult{
			Epoch:     h1.Height,
			Target:    id,
			FaultType: ConsensusFaultDoubleForkMining,
		}
	}
	// (2) TimeOffsetMining: same miner, same parents, different heights.
	if sameParents(h1, h2) {
		id, _ := h1.Miner.IDValue()
		// Report the later of the two epochs (the offset attempt).
		ep := h1.Height
		if h2.Height > ep {
			ep = h2.Height
		}
		return ConsensusFaultResult{
			Epoch:     ep,
			Target:    id,
			FaultType: ConsensusFaultTimeOffsetMining,
		}
	}
	// (3) ParentGrinding: with `extra` at some epoch H, one of h1/h2 is
	// at H+1 by the same miner, but its parent tipset does NOT include
	// `extra`. That is a grinding attempt where the miner discarded its
	// own earlier block.
	if extra != nil && extra.Miner.String() == h1.Miner.String() {
		if err := verifier.VerifyBlockHeader(extra); err == nil {
			extraCID := blockCID(extra)
			for _, hdr := range []*BlockHeader{h1, h2} {
				if hdr.Height == extra.Height+1 && !containsCID(hdr.Parents, extraCID) {
					id, _ := hdr.Miner.IDValue()
					return ConsensusFaultResult{
						Epoch:     hdr.Height,
						Target:    id,
						FaultType: ConsensusFaultParentGrinding,
					}
				}
			}
		}
	}
	return ConsensusFaultResult{}
}

// blockCID computes a stable identifier for a block header. In real
// Filecoin this is the CID of the DagCBOR-encoded header; for the
// prototype we hash the parsed-shape bytes so ParentGrinding tests
// can construct headers directly without full round-trip encoding.
// Real deployments must use the on-wire CID -- follow-on with #88.
func blockCID(h *BlockHeader) cid.Cid {
	var b bytes.Buffer
	b.Write(h.Miner.Bytes())
	for _, p := range h.Parents {
		b.Write(p.Bytes())
	}
	var hb [8]byte
	binary.LittleEndian.PutUint64(hb[:], uint64(h.Height))
	b.Write(hb[:])
	// Reuse the block CID codec (dag-cbor) + blake2b-256 multihash for
	// determinism; real path uses go-ipld-cbor.MarshalCBOR of the whole
	// header struct.
	c, err := cidOfBlock(codecDagCBOR, b.Bytes())
	if err != nil {
		return cid.Undef
	}
	return c
}

func headersEqual(a, b *BlockHeader) bool {
	if a.Height != b.Height {
		return false
	}
	if a.Miner.String() != b.Miner.String() {
		return false
	}
	if !sameParents(a, b) {
		return false
	}
	return bytes.Equal(a.BLSSignature, b.BLSSignature)
}

func sameParents(a, b *BlockHeader) bool {
	if len(a.Parents) != len(b.Parents) {
		return false
	}
	for i := range a.Parents {
		if !a.Parents[i].Equals(b.Parents[i]) {
			return false
		}
	}
	return true
}

func containsCID(list []cid.Cid, c cid.Cid) bool {
	for _, x := range list {
		if x.Equals(c) {
			return true
		}
	}
	return false
}
