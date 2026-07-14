package chainxchg

// ChainExchange server tests.
//
// The unit tests exercise serveResponse against an in-memory source that
// serves a synthetic 5-tipset chain. The integration test wires a real
// Lantern client to a real Lantern server on a libp2p host pair and
// round-trips a request end-to-end: it is the closest thing to a
// bridge-off calibration soak we can run locally.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// mapSource is a tiny in-memory Source, keyed by TipSetKey.String().
type mapSource struct {
	byKey map[string]*ltypes.TipSet
	err   error
}

func newMapSource(chain [][]*ltypes.BlockHeader) *mapSource {
	m := &mapSource{byKey: make(map[string]*ltypes.TipSet, len(chain))}
	for _, blocks := range chain {
		ts, err := ltypes.NewTipSet(blocks)
		if err != nil {
			panic(err)
		}
		m.byKey[ts.Key().String()] = ts
	}
	return m
}

func (m *mapSource) GetTipSet(tsk ltypes.TipSetKey) (*ltypes.TipSet, error) {
	if m.err != nil {
		return nil, m.err
	}
	ts, ok := m.byKey[tsk.String()]
	if !ok {
		return nil, errors.New("not found")
	}
	return ts, nil
}

func TestServeResponse_HappyPath(t *testing.T) {
	chain := mkChain(t, 5, 100)
	src := newMapSource(chain)

	headKey := headCids(chain[0])
	resp, err := serveResponse(src, headKey, 5)
	if err != nil {
		t.Fatalf("serveResponse: %v", err)
	}
	gotChain, status, errMsg, err := readResponse(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if status != statusOK {
		t.Fatalf("status = %d, want OK (%d): %q", status, statusOK, errMsg)
	}
	if len(gotChain) != 5 {
		t.Fatalf("chain length = %d, want 5", len(gotChain))
	}
	// Heights walk parent-ward: chain[0] is the head, chain[4] is deepest.
	for i, blocks := range gotChain {
		wantH := abi.ChainEpoch(100 - i)
		if blocks[0].Height != wantH {
			t.Errorf("chain[%d].Height = %d, want %d", i, blocks[0].Height, wantH)
		}
	}
}

func TestServeResponse_PartialWhenChainRunsShort(t *testing.T) {
	chain := mkChain(t, 3, 100)
	src := newMapSource(chain)

	// Ask for 10 but only 3 exist -> statusPartial with 3 entries.
	resp, err := serveResponse(src, headCids(chain[0]), 10)
	if err != nil {
		t.Fatalf("serveResponse: %v", err)
	}
	gotChain, status, _, err := readResponse(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status != statusPartial {
		t.Fatalf("status = %d, want Partial (%d)", status, statusPartial)
	}
	if len(gotChain) != 3 {
		t.Fatalf("partial chain length = %d, want 3", len(gotChain))
	}
}

func TestServeResponse_HeadNotFound(t *testing.T) {
	// Source that has NOTHING; head cannot be resolved.
	src := newMapSource(nil)
	resp, err := serveResponse(src, []cid.Cid{mkTestCID(t, "no such head")}, 5)
	if err != nil {
		t.Fatalf("serveResponse: %v", err)
	}
	_, status, errMsg, err := readResponse(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status != statusNotFound {
		t.Fatalf("status = %d, want NotFound (%d): %q", status, statusNotFound, errMsg)
	}
}

func TestServeResponse_LengthClampedAtMax(t *testing.T) {
	chain := mkChain(t, 5, 100)
	src := newMapSource(chain)

	// Asking for length > MaxRequestLength should not panic and should
	// return at most the available chain (5 here) as Partial.
	resp, err := serveResponse(src, headCids(chain[0]), MaxRequestLength+10_000)
	if err != nil {
		t.Fatalf("serveResponse: %v", err)
	}
	gotChain, status, _, err := readResponse(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status != statusPartial {
		t.Fatalf("status = %d, want Partial (source shorter than clamp)", status)
	}
	if len(gotChain) != 5 {
		t.Fatalf("chain length = %d, want 5", len(gotChain))
	}
}

func TestServeResponse_EmptyHeadRejected(t *testing.T) {
	resp, err := serveResponse(newMapSource(nil), nil, 5)
	if err != nil {
		t.Fatalf("serveResponse: %v", err)
	}
	_, status, _, err := readResponse(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status != statusBadRequest {
		t.Fatalf("status = %d, want BadRequest for empty head", status)
	}
}

func TestReadServerRequest_RoundTripsWriteRequest(t *testing.T) {
	head := []cid.Cid{mkTestCID(t, "one"), mkTestCID(t, "two")}
	var buf bytes.Buffer
	if err := writeRequest(&buf, head, 42, optHeaders); err != nil {
		t.Fatalf("writeRequest: %v", err)
	}
	gotHead, gotLen, gotOpts, err := readServerRequest(&buf)
	if err != nil {
		t.Fatalf("readServerRequest: %v", err)
	}
	if gotLen != 42 {
		t.Errorf("length = %d, want 42", gotLen)
	}
	if gotOpts != optHeaders {
		t.Errorf("options = %d, want %d", gotOpts, optHeaders)
	}
	if len(gotHead) != 2 || !gotHead[0].Equals(head[0]) || !gotHead[1].Equals(head[1]) {
		t.Errorf("head mismatch: got %v, want %v", gotHead, head)
	}
}

func TestSetSource_ReplacesNotFoundBehavior(t *testing.T) {
	// End-to-end: real libp2p host pair, Lantern client requests from
	// Lantern server that DOES have a source. Verify the round-trip
	// returns the chain and the server counts one received request.
	//
	// If the Service still had the NotFound-only handler, the client
	// would see zero verified tipsets and error out.
	clientHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("client host: %v", err)
	}
	defer clientHost.Close()
	serverHost, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	defer serverHost.Close()

	// Register the source-backed handler BEFORE the hosts identify each
	// other, so protocol advertisement carries our ProtocolID from the
	// first identify round-trip.
	chain := mkChain(t, 4, 500)
	src := newMapSource(chain)
	svc := NewService(serverHost)
	svc.SetSource(src)
	svc.Register()

	if err := clientHost.Connect(context.Background(), peer.AddrInfo{ID: serverHost.ID(), Addrs: serverHost.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitProtocol(ctx, t, clientHost, serverHost.ID())

	cli := NewClient(clientHost)
	cli.SetPreferredPeers(func() []peer.ID { return []peer.ID{serverHost.ID()} })

	got, err := cli.FetchTipsetChain(ctx, headCids(chain[0]), 4)
	if err != nil {
		t.Fatalf("FetchTipsetChain: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("client got %d tipsets, want 4", len(got))
	}
	// Chain walk must be parent-ward.
	for i, blocks := range got {
		wantH := abi.ChainEpoch(500 - i)
		if blocks[0].Height != wantH {
			t.Errorf("client got[%d].Height = %d, want %d", i, blocks[0].Height, wantH)
		}
	}
	if r := svc.Stats().Received; r != 1 {
		t.Errorf("server Received = %d, want 1", r)
	}
}

// headCids extracts the block CIDs for a tipset's constituent blocks
// so we can use them as a Request.Head.
func headCids(blocks []*ltypes.BlockHeader) []cid.Cid {
	out := make([]cid.Cid, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Cid())
	}
	return out
}

// waitProtocol spins until the client sees the server advertise
// ProtocolID (identify propagation), up to the ctx deadline.
func waitProtocol(ctx context.Context, t *testing.T, cli host.Host, sid peer.ID) {
	t.Helper()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if ok, _ := cli.Peerstore().SupportsProtocols(sid, ProtocolID); len(ok) > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s to advertise %s", sid, ProtocolID)
		case <-tick.C:
		}
	}
}
