package handlers

import (
	"testing"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"

	"github.com/Reiers/lantern/api"
)

func mustIDAddr(id uint64) addr.Address {
	a, err := addr.NewIDAddress(id)
	if err != nil {
		panic(err)
	}
	return a
}

// Cover the canonical signing bytes function: same input -> same output,
// different fields -> different output.
func TestPaychVoucherSigningBytes_Deterministic(t *testing.T) {
	sv := &api.PaychSignedVoucher{
		ChannelAddr: mustIDAddr(1234),
		Lane:        7,
		Nonce:       3,
		Amount:      big.NewInt(1_000_000_000),
	}
	a, err := paychVoucherSigningBytes(sv)
	if err != nil {
		t.Fatalf("signing bytes: %v", err)
	}
	b, err := paychVoucherSigningBytes(sv)
	if err != nil {
		t.Fatalf("signing bytes (2): %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
}

func TestPaychVoucherSigningBytes_DifferenceMatters(t *testing.T) {
	base := &api.PaychSignedVoucher{
		ChannelAddr: mustIDAddr(1234),
		Lane:        7,
		Nonce:       3,
		Amount:      big.NewInt(1_000),
	}
	tweaked := *base
	tweaked.Nonce = 4
	a, _ := paychVoucherSigningBytes(base)
	b, _ := paychVoucherSigningBytes(&tweaked)
	if string(a) == string(b) {
		t.Errorf("expected different bytes for different nonce")
	}
}

func TestPaychVoucherSigningBytes_NilRejected(t *testing.T) {
	if _, err := paychVoucherSigningBytes(nil); err == nil {
		t.Errorf("expected error for nil voucher")
	}
}
