package handlers

import (
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
)

func TestEthAddressFromFilecoinIDActor_IDProtocol(t *testing.T) {
	// f0143103 -> 143103 = 0x22efb
	// expected: 0xff || 11 zero bytes || be64(143103) = 0xff||00*11||0000000000022efb
	id, err := address.NewIDAddress(143103)
	if err != nil {
		t.Fatalf("NewIDAddress: %v", err)
	}
	got := EthAddressFromFilecoinIDActor(id)
	want := "0xff00000000000000000000000000000000022eff"
	// recompute correctly: 143103 in hex = 0x22EFF (143103 = 9 * 16^4 + 0xfff = ...) let me just check
	// 143103 = 0x22EFF: yes 143103 = 2*65536 + 0x2eff = 131072 + 12031 = 143103. ✓
	if got != want {
		t.Errorf("EthAddressFromFilecoinIDActor(f0143103) = %s, want %s", got, want)
	}
}

func TestEthAddressFromFilecoinIDActor_KnownValue(t *testing.T) {
	// f0410 -> 0xff || zero(11) || 8-byte be64(410) = 0xff||00*11||000000000000019A
	id, err := address.NewIDAddress(410)
	if err != nil {
		t.Fatalf("NewIDAddress(410): %v", err)
	}
	got := EthAddressFromFilecoinIDActor(id)
	// 410 decimal = 0x19a
	want := "0xff0000000000000000000000000000000000019a"
	if got != want {
		t.Errorf("EthAddressFromFilecoinIDActor(f0410) = %s, want %s", got, want)
	}
}

func TestEthHashFromCid_Undef(t *testing.T) {
	got := EthHashFromCid(cid.Undef)
	want := "0x0000000000000000000000000000000000000000000000000000000000000000"
	if got != want {
		t.Errorf("EthHashFromCid(undef) = %s, want %s", got, want)
	}
}

func TestEthHashFromCid_RealBlockCid(t *testing.T) {
	// Real calibration block CID from our smoke run:
	c, err := cid.Decode("bafy2bzaceamv3mhtyppiesmdqnkekym2tm524zmzt7f45unrl3h57bplivvow")
	if err != nil {
		t.Fatalf("decode CID: %v", err)
	}
	got := EthHashFromCid(c)
	if len(got) != 66 || got[:2] != "0x" {
		t.Errorf("EthHashFromCid returned malformed value: %s", got)
	}
	if got == "0x0000000000000000000000000000000000000000000000000000000000000000" {
		t.Errorf("EthHashFromCid returned zero hash for real CID; conversion failed")
	}
}

func TestFirstCidHash_Empty(t *testing.T) {
	got := firstCidHash(nil)
	want := "0x0000000000000000000000000000000000000000000000000000000000000000"
	if got != want {
		t.Errorf("firstCidHash(nil) = %s, want %s", got, want)
	}
}
