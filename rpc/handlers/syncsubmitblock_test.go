package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/Reiers/lantern/chain/types"
)

// fakeBlockPub records PublishBlock calls (BlockPublisher).
type fakeBlockPub struct {
	calls int
	err   error
}

func (f *fakeBlockPub) PublishBlock(_ context.Context, _ *types.BlockMsg) error {
	f.calls++
	return f.err
}

func mkBlockMsg() *types.BlockMsg {
	return &types.BlockMsg{Header: &types.BlockHeader{}}
}

// TestSyncSubmitBlock_GatedOff: with AllowBlockSubmit=false, submit is a
// no-op error regardless of any wired publisher (operator opt-in).
func TestSyncSubmitBlock_GatedOff(t *testing.T) {
	c := &ChainAPI{}
	bp := &fakeBlockPub{}
	c.SetBlockPublisher(bp)
	err := c.SyncSubmitBlock(context.Background(), mkBlockMsg())
	if err == nil {
		t.Fatal("expected gate error with AllowBlockSubmit=false")
	}
	if bp.calls != 0 {
		t.Fatalf("publisher called %d times while gated off", bp.calls)
	}
}

// TestSyncSubmitBlock_NoPublisher: gate on but no publisher wired => clear
// error, not a nil-deref.
func TestSyncSubmitBlock_NoPublisher(t *testing.T) {
	c := &ChainAPI{AllowBlockSubmit: true}
	if err := c.SyncSubmitBlock(context.Background(), mkBlockMsg()); err == nil {
		t.Fatal("expected error when no block publisher wired")
	}
}

// TestSyncSubmitBlock_UsesExplicitPublisher: gate on + explicit publisher
// wired => PublishBlock is invoked (the PDP/backup happy path).
func TestSyncSubmitBlock_UsesExplicitPublisher(t *testing.T) {
	c := &ChainAPI{AllowBlockSubmit: true}
	bp := &fakeBlockPub{}
	c.SetBlockPublisher(bp)
	if err := c.SyncSubmitBlock(context.Background(), mkBlockMsg()); err != nil {
		t.Fatalf("SyncSubmitBlock: %v", err)
	}
	if bp.calls != 1 {
		t.Fatalf("PublishBlock called %d times, want 1", bp.calls)
	}
}

// TestSyncSubmitBlock_PropagatesPublishError.
func TestSyncSubmitBlock_PropagatesPublishError(t *testing.T) {
	c := &ChainAPI{AllowBlockSubmit: true}
	sentinel := errors.New("topic publish failed")
	c.SetBlockPublisher(&fakeBlockPub{err: sentinel})
	if err := c.SyncSubmitBlock(context.Background(), mkBlockMsg()); !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped publish error, got %v", err)
	}
}
