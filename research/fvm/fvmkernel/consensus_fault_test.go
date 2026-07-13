package fvmkernel

// Tests for consensus-fault detection (lantern#130 Tier 1).

import (
	"testing"

	"github.com/ipfs/go-cid"
)

func minerID(id uint64) Address { return IDAddress(id) }

func fakeCID(seed byte) cid.Cid {
	data := make([]byte, 8)
	for i := range data {
		data[i] = seed + byte(i)
	}
	c, err := cidOfBlock(codecDagCBOR, data)
	if err != nil {
		panic(err)
	}
	return c
}

func TestDoubleForkMining(t *testing.T) {
	h1 := &BlockHeader{Miner: minerID(1000), Parents: []cid.Cid{fakeCID(1)}, Height: 100, BLSSignature: []byte{0x01}}
	h2 := &BlockHeader{Miner: minerID(1000), Parents: []cid.Cid{fakeCID(2)}, Height: 100, BLSSignature: []byte{0x02}}

	got := VerifyConsensusFault(h1, h2, nil, AcceptAllVerifier{})
	if got.FaultType != ConsensusFaultDoubleForkMining {
		t.Fatalf("fault type %d, want DoubleForkMining", got.FaultType)
	}
	if got.Target != 1000 || got.Epoch != 100 {
		t.Fatalf("target %d epoch %d, want 1000/100", got.Target, got.Epoch)
	}
}

func TestTimeOffsetMining(t *testing.T) {
	parents := []cid.Cid{fakeCID(5), fakeCID(6)}
	h1 := &BlockHeader{Miner: minerID(2000), Parents: parents, Height: 100, BLSSignature: []byte{0x01}}
	h2 := &BlockHeader{Miner: minerID(2000), Parents: parents, Height: 105, BLSSignature: []byte{0x02}}

	got := VerifyConsensusFault(h1, h2, nil, AcceptAllVerifier{})
	if got.FaultType != ConsensusFaultTimeOffsetMining {
		t.Fatalf("fault %d, want TimeOffsetMining", got.FaultType)
	}
	if got.Target != 2000 || got.Epoch != 105 {
		t.Fatalf("target %d epoch %d, want 2000/105", got.Target, got.Epoch)
	}
}

func TestParentGrinding(t *testing.T) {
	// extra at epoch 100 by miner 3000.
	extra := &BlockHeader{
		Miner:        minerID(3000),
		Parents:      []cid.Cid{fakeCID(1)},
		Height:       100,
		BLSSignature: []byte{0xEE},
	}
	extraCID := blockCID(extra)

	// h1: same miner at epoch 101 with parents INCLUDING extra -- honest.
	h1 := &BlockHeader{
		Miner:        minerID(3000),
		Parents:      []cid.Cid{extraCID, fakeCID(9)},
		Height:       101,
		BLSSignature: []byte{0x01},
	}
	// h2: same miner at epoch 101 with parents NOT INCLUDING extra -- grinding.
	h2 := &BlockHeader{
		Miner:        minerID(3000),
		Parents:      []cid.Cid{fakeCID(9)},
		Height:       101,
		BLSSignature: []byte{0x02},
	}

	got := VerifyConsensusFault(h1, h2, extra, AcceptAllVerifier{})
	// h1 and h2 have different parents at same epoch -> DoubleForkMining
	// wins before the ParentGrinding check. That's actually the correct
	// filecoin behavior (double-fork is checked first). So this test as
	// written should trigger DoubleFork; ParentGrinding needs a scenario
	// where h1==h2 in some fields. Reconstruct:
	if got.FaultType != ConsensusFaultDoubleForkMining {
		t.Fatalf("expected DoubleFork (both at height 101), got %d", got.FaultType)
	}

	// Real ParentGrinding: h1 at epoch 200 (well past extra), h2 at
	// epoch 101 (right after extra) with parents NOT including extra.
	h1p := &BlockHeader{
		Miner:        minerID(3000),
		Parents:      []cid.Cid{fakeCID(50)},
		Height:       200,
		BLSSignature: []byte{0x01},
	}
	h2p := &BlockHeader{
		Miner:        minerID(3000),
		Parents:      []cid.Cid{fakeCID(9)}, // does NOT include extraCID
		Height:       101,
		BLSSignature: []byte{0x02},
	}
	got = VerifyConsensusFault(h1p, h2p, extra, AcceptAllVerifier{})
	if got.FaultType != ConsensusFaultParentGrinding {
		t.Fatalf("expected ParentGrinding, got %d", got.FaultType)
	}
	if got.Target != 3000 || got.Epoch != 101 {
		t.Fatalf("target %d epoch %d, want 3000/101", got.Target, got.Epoch)
	}
}

func TestNoFaultForDifferentMiners(t *testing.T) {
	h1 := &BlockHeader{Miner: minerID(1000), Parents: []cid.Cid{fakeCID(1)}, Height: 100}
	h2 := &BlockHeader{Miner: minerID(1001), Parents: []cid.Cid{fakeCID(1)}, Height: 100}
	got := VerifyConsensusFault(h1, h2, nil, AcceptAllVerifier{})
	if got.FaultType != ConsensusFaultNone {
		t.Fatalf("expected None for different miners, got %d", got.FaultType)
	}
}

func TestNoFaultForIdenticalHeaders(t *testing.T) {
	h := &BlockHeader{Miner: minerID(1000), Parents: []cid.Cid{fakeCID(1)}, Height: 100, BLSSignature: []byte{0x01}}
	got := VerifyConsensusFault(h, h, nil, AcceptAllVerifier{})
	if got.FaultType != ConsensusFaultNone {
		t.Fatalf("expected None for identical headers, got %d", got.FaultType)
	}
}

func TestRejectAllVerifierBlocksFaultReporting(t *testing.T) {
	// Even a genuine DoubleFork must NOT be reported if the default
	// verifier rejects the signatures (the prototype's safe posture).
	h1 := &BlockHeader{Miner: minerID(1000), Parents: []cid.Cid{fakeCID(1)}, Height: 100, BLSSignature: []byte{0x01}}
	h2 := &BlockHeader{Miner: minerID(1000), Parents: []cid.Cid{fakeCID(2)}, Height: 100, BLSSignature: []byte{0x02}}
	got := VerifyConsensusFault(h1, h2, nil, RejectAllVerifier{})
	if got.FaultType != ConsensusFaultNone {
		t.Fatalf("RejectAll must suppress fault reporting; got %d", got.FaultType)
	}
}

func TestConsensusFaultResultBytesLayout(t *testing.T) {
	r := ConsensusFaultResult{Epoch: 42, Target: 1234, FaultType: 2}
	b := r.Bytes()
	if len(b) != 24 {
		t.Fatalf("packed layout %d bytes, want 24", len(b))
	}
	// Little-endian read-back.
	if e := int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16 | int64(b[3])<<24 | int64(b[4])<<32 | int64(b[5])<<40 | int64(b[6])<<48 | int64(b[7])<<56; e != 42 {
		t.Errorf("epoch bytes wrong: %d", e)
	}
	if b[16] != 2 {
		t.Errorf("fault-type byte wrong: %d", b[16])
	}
}
