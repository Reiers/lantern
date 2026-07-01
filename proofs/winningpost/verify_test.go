package winningpost

import (
	"errors"
	"testing"
)

func TestVerifyNotImplemented(t *testing.T) {
	ok, err := Verify(WinningPoStVerifyInfo{})
	if ok {
		t.Fatal("expected false")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}
