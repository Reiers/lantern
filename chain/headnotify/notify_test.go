package headnotify

import (
	"context"
	"sync"
	"testing"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/proof"

	"github.com/Reiers/lantern/api"
	hstore "github.com/Reiers/lantern/chain/header/store"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// helper to build a minimal valid BlockHeader for testing.
func mkBlock(t *testing.T, height abi.ChainEpoch, parents []cid.Cid) *ltypes.BlockHeader {
	t.Helper()
	miner, err := addr.NewIDAddress(uint64(1000 + int(height)))
	if err != nil {
		t.Fatal(err)
	}
	return &ltypes.BlockHeader{
		Miner:                 miner,
		Ticket:                &ltypes.Ticket{VRFProof: []byte("test")},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1},
		BeaconEntries:         []ltypes.BeaconEntry{},
		WinPoStProof:          []proof.PoStProof{},
		Parents:               parents,
		ParentWeight:          big.NewInt(1),
		Height:                height,
		ParentStateRoot:       genesisCID(),
		ParentMessageReceipts: genesisCID(),
		Messages:              genesisCID(),
		BLSAggregate:          &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
		Timestamp:             uint64(time.Now().Unix()),
		BlockSig:              &gscrypto.Signature{Type: gscrypto.SigTypeBLS, Data: make([]byte, 96)},
	}
}

func genesisCID() cid.Cid {
	c, _ := cid.Decode("bafy2bzaceaflvwa5ocjbhmyjgenr2xfvyvvwhxnmis4mb2ero2fh7zfbqhmd6")
	return c
}

func mkTipSet(t *testing.T, height abi.ChainEpoch, parents []cid.Cid) *ltypes.TipSet {
	bh := mkBlock(t, height, parents)
	ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{bh})
	if err != nil {
		t.Fatalf("NewTipSet: %v", err)
	}
	return ts
}

func TestSubscribeReceivesCurrent(t *testing.T) {
	d := New(nil, 8)
	ts := mkTipSet(t, 100, nil)
	d.mu.Lock()
	d.lastHead = ts
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := d.Subscribe(ctx)
	select {
	case ev := <-ch:
		if len(ev) != 1 || ev[0].Type != "current" {
			t.Fatalf("expected single 'current' event, got %+v", ev)
		}
		if ev[0].Val == nil || ev[0].Val.Height() != 100 {
			t.Fatalf("expected current at height 100, got %v", ev[0].Val)
		}
	case <-time.After(time.Second):
		t.Fatal("no current event delivered")
	}
}

func TestPublishCustomFansOut(t *testing.T) {
	d := New(nil, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := d.Subscribe(ctx)
	b := d.Subscribe(ctx)
	// Drain "current".
	<-a
	<-b

	ts := mkTipSet(t, 1, nil)
	d.PublishCustom([]api.HeadChange{{Type: "apply", Val: ts}})

	for i, ch := range []<-chan []api.HeadChange{a, b} {
		select {
		case ev := <-ch:
			if len(ev) != 1 || ev[0].Type != "apply" {
				t.Fatalf("sub %d: expected apply, got %+v", i, ev)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: no event", i)
		}
	}
}

func TestSlowSubscriberDrops(t *testing.T) {
	d := New(nil, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := d.Subscribe(ctx)
	// Don't drain — overflow.
	for i := 0; i < 20; i++ {
		d.PublishCustom([]api.HeadChange{{Type: "apply", Val: mkTipSet(t, abi.ChainEpoch(i+1), nil)}})
	}
	// Drain whatever the channel has. The buffer is bounded, so we
	// should see at most bufferSize+1 events even though we published 20.
	count := 0
	timeout := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break loop
			}
			count++
		case <-timeout:
			break loop
		}
	}
	if count > 3 { // 1 current + 2 buffered
		t.Fatalf("expected ≤3 events on a slow 2-buffer sub, got %d", count)
	}
	d.mu.Lock()
	subCount := len(d.subs)
	d.mu.Unlock()
	if subCount != 1 {
		t.Fatalf("expected sub still attached, got %d", subCount)
	}
}

func TestUnsubscribeOnCtxCancel(t *testing.T) {
	d := New(nil, 4)
	ctx, cancel := context.WithCancel(context.Background())
	ch := d.Subscribe(ctx)
	<-ch // drain current

	cancel()
	// Wait for the unsubscribe goroutine to close the channel.
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if d.SubscriberCount() != 0 {
					t.Fatalf("expected 0 subs after unsubscribe, got %d", d.SubscriberCount())
				}
				return
			}
		case <-deadline:
			t.Fatal("channel was not closed after ctx cancel")
		}
	}
}

func TestStoreOnHeadChangeFansOut(t *testing.T) {
	// In-memory store with no startup verification.
	s, err := hstore.Open("", hstore.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	d := New(s, 8)
	d.Start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := d.Subscribe(ctx)
	<-ch // current (empty)

	// Insert a genesis-like tipset and SetHead.
	gen := mkTipSet(t, 0, nil)
	if err := s.SetHead(context.Background(), gen); err != nil {
		t.Fatalf("set head genesis: %v", err)
	}
	select {
	case ev := <-ch:
		if len(ev) != 1 || ev[0].Type != "apply" || ev[0].Val.Height() != 0 {
			t.Fatalf("unexpected genesis event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event for genesis")
	}

	// Child of genesis.
	child := mkTipSet(t, 1, gen.Cids())
	if err := s.SetHead(context.Background(), child); err != nil {
		t.Fatalf("set head child: %v", err)
	}
	select {
	case ev := <-ch:
		// Should be a single apply for the child.
		if len(ev) < 1 {
			t.Fatalf("empty event: %+v", ev)
		}
		last := ev[len(ev)-1]
		if last.Type != "apply" || last.Val.Height() != 1 {
			t.Fatalf("unexpected child event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event for child")
	}
}

func TestFanoutTenSubscribers(t *testing.T) {
	d := New(nil, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const N = 10
	chans := make([]<-chan []api.HeadChange, N)
	for i := 0; i < N; i++ {
		chans[i] = d.Subscribe(ctx)
	}
	// Drain currents.
	for _, ch := range chans {
		<-ch
	}

	// Publish 5 apply events.
	for i := 0; i < 5; i++ {
		d.PublishCustom([]api.HeadChange{{Type: "apply", Val: mkTipSet(t, abi.ChainEpoch(i+1), nil)}})
	}

	var wg sync.WaitGroup
	wg.Add(N)
	for i, ch := range chans {
		ch := ch
		i := i
		go func() {
			defer wg.Done()
			got := 0
			deadline := time.After(2 * time.Second)
			for got < 5 {
				select {
				case <-ch:
					got++
				case <-deadline:
					t.Errorf("sub %d: only %d events", i, got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
