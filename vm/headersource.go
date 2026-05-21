// Adapters that bridge Lantern's chain/header/store.Store to the
// BaseFeeSource and PremiumSource interfaces required by GasEstimator.

package vm

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"

	hstore "github.com/Reiers/lantern/chain/header/store"
	"github.com/Reiers/lantern/chain/types"
)

// HeaderStoreFeeSource implements BaseFeeSource against a persistent
// header store. CurrentBaseFee reads the latest tipset's ParentBaseFee.
type HeaderStoreFeeSource struct {
	Store *hstore.Store
}

// CurrentBaseFee returns the head tipset's ParentBaseFee. If the store
// is empty, returns build.MinimumBaseFee.
func (h *HeaderStoreFeeSource) CurrentBaseFee(_ context.Context) (big.Int, error) {
	if h == nil || h.Store == nil {
		return big.NewInt(100), nil
	}
	head := h.Store.Head()
	if head == nil || len(head.Blocks()) == 0 {
		return big.NewInt(100), nil
	}
	return head.Blocks()[0].ParentBaseFee, nil
}

// MempoolPremiumSource implements PremiumSource against an in-memory
// gossipsub mempool. Lantern's `net/mpool` package exposes a Pending()
// list of locally observed SignedMessages; we sample their GasPremium
// values directly.
type MempoolPremiumSource struct {
	Pending func() []*types.SignedMessage
}

// RecentPremiums returns the GasPremium values of all currently-pending
// messages, up to `samples`. `lookback` is unused — gossipsub already
// constrains the freshness window.
func (m *MempoolPremiumSource) RecentPremiums(_ context.Context, _ int64, samples int) ([]big.Int, error) {
	if m == nil || m.Pending == nil {
		return nil, nil
	}
	pending := m.Pending()
	out := make([]big.Int, 0, len(pending))
	for _, sm := range pending {
		if sm == nil {
			continue
		}
		out = append(out, sm.Message.GasPremium)
		if len(out) >= samples {
			break
		}
	}
	return out, nil
}

// EpochAt computes how recent a tipset is relative to head. Returns -1
// if epoch is in the future or store is empty.
func EpochAt(s *hstore.Store, ep abi.ChainEpoch) (int64, error) {
	if s == nil {
		return -1, fmt.Errorf("nil header store")
	}
	head := s.HeadEpoch()
	if head < 0 || ep > head {
		return -1, nil
	}
	return int64(head - ep), nil
}
