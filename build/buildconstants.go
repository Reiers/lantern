// Constants in this file are lifted from
// github.com/filecoin-project/lotus/build/buildconstants at commit
// a0ecb8687f1c60d5e66040b6de364dbc9cc4d253. See LICENSE-LOTUS.
//
// We keep only the values that chain/types and other Phase 1 packages
// actually consume. Adding more constants here is fine, but each one should
// be commented with its upstream source.

package build

import (
	"github.com/filecoin-project/go-address"
)

// BlocksPerEpoch is the expected number of block producers per epoch.
// Upstream: build/buildconstants/params_shared_vals.go
//
//	var BlocksPerEpoch = uint64(builtin2.ExpectedLeadersPerEpoch) // == 5
const BlocksPerEpoch = uint64(5)

// FilBase is the total FIL supply in whole-FIL units.
// Upstream: build/buildconstants/params_shared_vals.go
const FilBase = uint64(2_000_000_000)

// FilecoinPrecision is the number of attoFIL per whole FIL.
// Upstream: build/buildconstants/params_shared_vals.go
const FilecoinPrecision = uint64(1_000_000_000_000_000_000)

// BlockGasLimit is the per-block gas budget. Used by types.Message.ValidForBlockInclusion.
// Upstream: build/buildconstants/params_shared_vals.go
const BlockGasLimit = int64(10_000_000_000)

// ZeroAddress is the Filecoin BLS zero address.
// Upstream: build/buildconstants/params_shared_vals.go
var ZeroAddress = mustParseAddress("f3yaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaby2smx7a")

func mustParseAddress(s string) address.Address {
	a, err := address.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return a
}
