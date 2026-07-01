package crosscheck

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	ltypes "github.com/Reiers/lantern/chain/types"
)

func tCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

func tTipSet(t *testing.T, h abi.ChainEpoch, tag string) *ltypes.TipSet {
	t.Helper()
	miner, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	bh := &ltypes.BlockHeader{
		Miner:                 miner,
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t-" + tag)},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + tag)},
		Parents:               []cid.Cid{tCID(t, "parent-"+tag)},
		ParentWeight:          ltypes.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       tCID(t, "state-"+tag),
		ParentMessageReceipts: tCID(t, "rcpt-"+tag),
		Messages:              tCID(t, "msgs-"+tag),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         ltypes.NewInt(100),
	}
	ts, err := ltypes.NewTipSet([]*ltypes.BlockHeader{bh})
	require.NoError(t, err)
	return ts
}

type fakeSource struct {
	head *ltypes.TipSet
	at   map[abi.ChainEpoch]*ltypes.TipSet
}

func (f *fakeSource) Head() *ltypes.TipSet { return f.head }
func (f *fakeSource) GetTipSetByHeight(e abi.ChainEpoch) (*ltypes.TipSet, error) {
	ts, ok := f.at[e]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return ts, nil
}

type fakeBridge struct {
	respondHeight int64
	respondCids   []cid.Cid
	err           error
	calls         int
}

func (f *fakeBridge) Provenance() string { return "fake@test" }
func (f *fakeBridge) RawJSONRPC(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
	f.calls++
	if method != "Filecoin.ChainGetTipSetByHeight" {
		return nil, fmt.Errorf("unexpected method %s", method)
	}
	if f.err != nil {
		return nil, f.err
	}
	cids := make([]map[string]string, 0, len(f.respondCids))
	for _, c := range f.respondCids {
		cids = append(cids, map[string]string{"/": c.String()})
	}
	return json.Marshal(map[string]interface{}{
		"Cids":   cids,
		"Height": f.respondHeight,
		"Blocks": []map[string]interface{}{},
	})
}

// TestCheckOnce_Agree: bridge returns the same tipset key -> agree.
func TestCheckOnce_Agree(t *testing.T) {
	target := tTipSet(t, 97, "x")
	src := &fakeSource{head: tTipSet(t, 100, "head"), at: map[abi.ChainEpoch]*ltypes.TipSet{97: target}}
	br := &fakeBridge{respondHeight: 97, respondCids: target.Cids()}
	c, err := New(Config{Bridge: br, Source: src})
	require.NoError(t, err)

	c.CheckOnce(context.Background())
	st := c.Stats()
	require.Equal(t, uint64(1), st.Checks)
	require.Equal(t, uint64(1), st.Agrees)
	require.Equal(t, uint64(0), st.Diverges)
	require.Equal(t, "agree", st.LastResult)
	require.Equal(t, abi.ChainEpoch(97), st.LastCheckedEpoch)
}

// TestCheckOnce_Diverge: bridge disagrees at settled depth -> DIVERGE +
// OnDiverge hook fires. Observe-only: nothing else happens.
func TestCheckOnce_Diverge(t *testing.T) {
	ours := tTipSet(t, 97, "ours")
	other := tTipSet(t, 97, "theirs")
	src := &fakeSource{head: tTipSet(t, 100, "head"), at: map[abi.ChainEpoch]*ltypes.TipSet{97: ours}}
	br := &fakeBridge{respondHeight: 97, respondCids: other.Cids()}

	fired := false
	c, err := New(Config{Bridge: br, Source: src, OnDiverge: func(e abi.ChainEpoch, _, _ string) {
		fired = true
		require.Equal(t, abi.ChainEpoch(97), e)
	}})
	require.NoError(t, err)

	c.CheckOnce(context.Background())
	st := c.Stats()
	require.Equal(t, uint64(1), st.Diverges)
	require.Equal(t, "DIVERGE", st.LastResult)
	require.True(t, fired, "OnDiverge must fire")
}

// TestCheckOnce_BridgeLagSkips: bridge returns an older height (lag or
// null-round resolution) -> skip, never a false DIVERGE.
func TestCheckOnce_BridgeLagSkips(t *testing.T) {
	ours := tTipSet(t, 97, "ours")
	src := &fakeSource{head: tTipSet(t, 100, "head"), at: map[abi.ChainEpoch]*ltypes.TipSet{97: ours}}
	br := &fakeBridge{respondHeight: 95, respondCids: ours.Cids()}
	c, err := New(Config{Bridge: br, Source: src})
	require.NoError(t, err)

	c.CheckOnce(context.Background())
	st := c.Stats()
	require.Equal(t, uint64(0), st.Checks)
	require.Equal(t, uint64(0), st.Diverges)
	require.Equal(t, uint64(1), st.Skipped)
	require.Equal(t, "skip", st.LastResult)
}

// TestCheckOnce_BridgeDownSkips: unreachable bridge is a skip, not an
// error state; reads are unaffected by design.
func TestCheckOnce_BridgeDownSkips(t *testing.T) {
	ours := tTipSet(t, 97, "ours")
	src := &fakeSource{head: tTipSet(t, 100, "head"), at: map[abi.ChainEpoch]*ltypes.TipSet{97: ours}}
	br := &fakeBridge{err: fmt.Errorf("connection refused")}
	c, err := New(Config{Bridge: br, Source: src})
	require.NoError(t, err)

	c.CheckOnce(context.Background())
	st := c.Stats()
	require.Equal(t, uint64(1), st.Skipped)
	require.Equal(t, uint64(0), st.Diverges)
}

// TestCheckOnce_ShallowChainSkips: head shallower than depth -> skip.
func TestCheckOnce_ShallowChainSkips(t *testing.T) {
	src := &fakeSource{head: tTipSet(t, 2, "head"), at: map[abi.ChainEpoch]*ltypes.TipSet{}}
	br := &fakeBridge{}
	c, err := New(Config{Bridge: br, Source: src})
	require.NoError(t, err)

	c.CheckOnce(context.Background())
	require.Equal(t, uint64(1), c.Stats().Skipped)
	require.Equal(t, 0, br.calls, "bridge must not be called for a shallow chain")
}

// TestNew_RequiresDeps: nil bridge or source is a config error.
func TestNew_RequiresDeps(t *testing.T) {
	_, err := New(Config{})
	require.Error(t, err)
}
