package handlers

import (
	"context"
	"sync"
	"testing"

	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/wallet"
)

// stubPublisher records published messages and satisfies MpoolPublisher.
type stubPublisher struct {
	mu        sync.Mutex
	published []*types.SignedMessage
}

func (s *stubPublisher) Publish(_ context.Context, sm *types.SignedMessage) (cid.Cid, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published = append(s.published, sm)
	return sm.Cid(), nil
}

func newWalletCAPI(t *testing.T) (*ChainAPI, *wallet.Wallet, *stubPublisher) {
	t.Helper()
	w, err := wallet.New(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("wallet.New: %v", err)
	}
	c := newCAPI()
	c.Wallet = w
	pub := &stubPublisher{}
	c.Mpool = pub
	return c, w, pub
}

// lantern#146: MpoolPushMessage must not mutate the caller's message.
// The old implementation did `*msg = *estim`, writing the estimated gas
// values (and filled nonce) back through the caller's pointer.
func TestMpoolPushMessage_DoesNotMutateCaller(t *testing.T) {
	c, w, pub := newWalletCAPI(t)
	from, err := w.NewAddress(context.Background(), wallet.KTSecp256k1)
	if err != nil {
		t.Fatalf("new address: %v", err)
	}

	msgIn := &types.Message{
		From:  from,
		To:    from,
		Value: big.Zero(),
		Nonce: 7, // preset: skips the nonce lookup (no accessor in this test)
		// GasLimit deliberately 0: the estimator fills it on the copy.
		GasLimit:   0,
		GasFeeCap:  big.NewInt(200),
		GasPremium: big.NewInt(100),
		Method:     0,
	}

	sm, err := c.MpoolPushMessage(context.Background(), msgIn, nil)
	if err != nil {
		t.Fatalf("MpoolPushMessage: %v", err)
	}
	if msgIn.GasLimit != 0 {
		t.Fatalf("caller's message was mutated: GasLimit=%d, want 0", msgIn.GasLimit)
	}
	if sm.Message.GasLimit == 0 {
		t.Fatal("signed message should carry the estimated GasLimit")
	}
	if sm.Message.Nonce != 7 {
		t.Fatalf("signed nonce = %d, want 7", sm.Message.Nonce)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(pub.published))
	}
}

// lantern#146: GasPremium > GasFeeCap after estimation must be rejected
// (such a message is invalid; lotus errors the same way).
func TestMpoolPushMessage_PremiumAboveFeeCapRejected(t *testing.T) {
	c, w, pub := newWalletCAPI(t)
	from, err := w.NewAddress(context.Background(), wallet.KTSecp256k1)
	if err != nil {
		t.Fatalf("new address: %v", err)
	}

	msgIn := &types.Message{
		From:       from,
		To:         from,
		Value:      big.Zero(),
		Nonce:      1,
		GasLimit:   1_000_000,
		GasFeeCap:  big.NewInt(100),
		GasPremium: big.NewInt(200), // > fee cap
		Method:     0,
	}

	if _, err := c.MpoolPushMessage(context.Background(), msgIn, nil); err == nil {
		t.Fatal("expected error for GasPremium > GasFeeCap")
	}
	if len(pub.published) != 0 {
		t.Fatalf("nothing must be published on rejection, got %d", len(pub.published))
	}
}

// lantern#146: concurrent pushes from the same sender must serialize on
// the per-sender lock (run with -race; the old code had no lock at all).
func TestMpoolPushMessage_ConcurrentSameSender(t *testing.T) {
	c, w, pub := newWalletCAPI(t)
	from, err := w.NewAddress(context.Background(), wallet.KTSecp256k1)
	if err != nil {
		t.Fatalf("new address: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(nonce uint64) {
			defer wg.Done()
			msg := &types.Message{
				From:       from,
				To:         from,
				Value:      big.Zero(),
				Nonce:      nonce + 1, // preset nonces (no accessor)
				GasLimit:   1_000_000,
				GasFeeCap:  big.NewInt(200),
				GasPremium: big.NewInt(100),
				Method:     0,
			}
			if _, err := c.MpoolPushMessage(context.Background(), msg, nil); err != nil {
				t.Errorf("push nonce %d: %v", nonce+1, err)
			}
		}(uint64(i))
	}
	wg.Wait()
	if len(pub.published) != n {
		t.Fatalf("expected %d published, got %d", n, len(pub.published))
	}
}
