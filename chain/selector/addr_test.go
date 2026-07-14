package selector

// Test helpers for constructing stable synthetic block headers. Kept in a
// _test.go file so the production selector package has no address dep.

import "github.com/filecoin-project/go-address"

// addressLike is address.Address; aliased so the selector_test.go
// syntheticTipSet helper (declared before this file alphabetically) can
// reference the type without a full address-package import in that file.
type addressLike = address.Address

func newIDAddr(id uint64) address.Address {
	a, err := address.NewIDAddress(id)
	if err != nil {
		panic(err)
	}
	return a
}
