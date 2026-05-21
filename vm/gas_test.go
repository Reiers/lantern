package vm

import (
	"testing"

	gscrypto "github.com/filecoin-project/go-state-types/crypto"
)

func TestPriceListV15Defaults(t *testing.T) {
	pl := V15PriceList()
	// Smoke check the headline constants — these are the numbers Lotus
	// has shipped since nv17.
	if pl.OnChainMessageComputeBase != 38863 {
		t.Errorf("OnChainMessageComputeBase: want 38863, got %d", pl.OnChainMessageComputeBase)
	}
	if pl.OnMethodInvocation != 75000 {
		t.Errorf("OnMethodInvocation: want 75000, got %d", pl.OnMethodInvocation)
	}
	if pl.OnVerifySignatureBLS != 16598605 {
		t.Errorf("OnVerifySignatureBLS: want 16598605, got %d", pl.OnVerifySignatureBLS)
	}
}

func TestOnChainMessageScales(t *testing.T) {
	pl := V15PriceList()
	for _, n := range []int{0, 100, 1000, 10000} {
		got := pl.OnChainMessage(n)
		want := pl.OnChainMessageComputeBase + int64(n)*pl.OnChainMessageStoragePerByte
		if got != want {
			t.Errorf("OnChainMessage(%d): want %d, got %d", n, want, got)
		}
	}
}

func TestOnInvokeValueXfer(t *testing.T) {
	pl := V15PriceList()
	bare := pl.OnInvoke(false)
	withv := pl.OnInvoke(true)
	if withv-bare != pl.OnMethodInvocationValue {
		t.Errorf("value-xfer surcharge: want %d, got %d", pl.OnMethodInvocationValue, withv-bare)
	}
}

func TestOnSignatureSwitch(t *testing.T) {
	pl := V15PriceList()
	tests := []struct {
		t    gscrypto.SigType
		want int64
	}{
		{gscrypto.SigTypeBLS, pl.OnVerifySignatureBLS},
		{gscrypto.SigTypeSecp256k1, pl.OnVerifySignatureSecp256k1},
		{gscrypto.SigTypeDelegated, pl.OnVerifySignatureDelegated},
		{gscrypto.SigType(99), 0},
	}
	for _, tc := range tests {
		if got := pl.OnSignature(tc.t); got != tc.want {
			t.Errorf("OnSignature(%v): want %d, got %d", tc.t, tc.want, got)
		}
	}
}
