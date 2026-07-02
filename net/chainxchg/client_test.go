// Tests for the ChainExchange client (issue #91).
//
// A pair of real libp2p hosts: the "server" host runs a handwritten
// responder that returns canned Response CBOR built from real
// ltypes.BlockHeader values; the client fetches and must CID-verify the
// chain. Covers: happy path, partial responses, NotFound, and a
// malicious peer returning a spliced (wrong-head) chain.

package chainxchg

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
	cbg "github.com/whyrusleeping/cbor-gen"

	ltypes "github.com/Reiers/lantern/chain/types"
)

func mkTestCID(t *testing.T, s string) cid.Cid {
	t.Helper()
	hash, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.DagCBOR, hash)
}

func mkTestBlock(t *testing.T, h abi.ChainEpoch, parents []cid.Cid, tag string) *ltypes.BlockHeader {
	t.Helper()
	a, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	return &ltypes.BlockHeader{
		Miner:                 a,
		Ticket:                &ltypes.Ticket{VRFProof: []byte("t-" + tag)},
		ElectionProof:         &ltypes.ElectionProof{WinCount: 1, VRFProof: []byte("e-" + tag)},
		Parents:               parents,
		ParentWeight:          ltypes.NewInt(uint64(h)),
		Height:                h,
		ParentStateRoot:       mkTestCID(t, "state-"+tag),
		ParentMessageReceipts: mkTestCID(t, "rcpt-"+tag),
		Messages:              mkTestCID(t, "msgs-"+tag),
		Timestamp:             1_700_000_000 + uint64(h)*30,
		ParentBaseFee:         ltypes.NewInt(100),
	}
}

// mkChain builds n single-block tipsets, newest-first (chain[0] highest),
// linked via Parents.
func mkChain(t *testing.T, n int, topHeight abi.ChainEpoch) [][]*ltypes.BlockHeader {
	t.Helper()
	blocks := make([]*ltypes.BlockHeader, n)
	// Build oldest -> newest so parents link.
	prevParents := []cid.Cid{mkTestCID(t, "genesis")}
	for i := n - 1; i >= 0; i-- {
		h := topHeight - abi.ChainEpoch(i)
		b := mkTestBlock(t, h, prevParents, "c"+string(rune('a'+i)))
		blocks[i] = b
		prevParents = []cid.Cid{b.Cid()}
	}
	chain := make([][]*ltypes.BlockHeader, n)
	for i := 0; i < n; i++ {
		chain[i] = []*ltypes.BlockHeader{blocks[i]}
	}
	return chain
}

// encodeResponse writes Response{status, errMsg, chain} the way Lotus
// cbor_gen would (Messages slot = null).
func encodeResponse(t *testing.T, status uint64, errMsg string, chain [][]*ltypes.BlockHeader) []byte {
	t.Helper()
	var buf bytes.Buffer
	cw := cbg.NewCborWriter(&buf)
	require.NoError(t, cw.WriteMajorTypeHeader(cbg.MajArray, 3))
	require.NoError(t, cw.WriteMajorTypeHeader(cbg.MajUnsignedInt, status))
	require.NoError(t, cw.WriteMajorTypeHeader(cbg.MajTextString, uint64(len(errMsg))))
	if errMsg != "" {
		_, err := cw.Write([]byte(errMsg))
		require.NoError(t, err)
	}
	require.NoError(t, cw.WriteMajorTypeHeader(cbg.MajArray, uint64(len(chain))))
	for _, blocks := range chain {
		require.NoError(t, cw.WriteMajorTypeHeader(cbg.MajArray, 2))
		require.NoError(t, cw.WriteMajorTypeHeader(cbg.MajArray, uint64(len(blocks))))
		for _, b := range blocks {
			require.NoError(t, b.MarshalCBOR(cw))
		}
		_, err := cw.Write(cbg.CborNull)
		require.NoError(t, err)
	}
	return buf.Bytes()
}

// serveCanned registers a handler on h that drains the request and
// writes raw. It also captures the decoded request for assertions.
type cannedServer struct {
	raw     []byte
	gotHead []cid.Cid
	gotLen  uint64
	gotOpts uint64
}

func (cs *cannedServer) register(t *testing.T, h host.Host) {
	t.Helper()
	h.SetStreamHandler(ProtocolID, func(str network.Stream) {
		defer func() { _ = str.Close() }()
		// Decode the request so tests can assert on it.
		cr := cbg.NewCborReader(str)
		maj, n, err := cr.ReadHeader()
		if err != nil || maj != cbg.MajArray || n != 3 {
			_ = str.Reset()
			return
		}
		maj, hn, err := cr.ReadHeader()
		if err != nil || maj != cbg.MajArray {
			_ = str.Reset()
			return
		}
		for i := uint64(0); i < hn; i++ {
			c, err := cbg.ReadCid(cr)
			if err != nil {
				_ = str.Reset()
				return
			}
			cs.gotHead = append(cs.gotHead, c)
		}
		_, cs.gotLen, _ = cr.ReadHeader()
		_, cs.gotOpts, _ = cr.ReadHeader()
		// Drain until EOF (client half-closes).
		_, _ = io.Copy(io.Discard, str)
		_, _ = str.Write(cs.raw)
	})
}

// hostPair spins up two connected hosts and waits until the client side
// sees the server advertise the protocol (identify propagation).
func hostPair(t *testing.T) (client host.Host, server host.Host) {
	t.Helper()
	c, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	s, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, c.Connect(context.Background(), peer.AddrInfo{ID: s.ID(), Addrs: s.Addrs()}))
	// Wait for identify to propagate protocol support.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		protos, err := c.Peerstore().SupportsProtocols(s.ID(), ProtocolID)
		if err == nil && len(protos) > 0 {
			return c, s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("identify never propagated protocol support")
	return nil, nil
}

func TestClient_FetchChain_HappyPath(t *testing.T) {
	chain := mkChain(t, 3, 100)
	head := []cid.Cid{chain[0][0].Cid()}

	// Register the responder on the server host FIRST so identify
	// advertises the protocol.
	cs := &cannedServer{raw: encodeResponse(t, statusOK, "", chain)}
	chost, shost := func() (host.Host, host.Host) {
		c, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		s, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return c, s
	}()
	cs.register(t, shost)
	require.NoError(t, chost.Connect(context.Background(), peer.AddrInfo{ID: shost.ID(), Addrs: shost.Addrs()}))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		protos, err := chost.Peerstore().SupportsProtocols(shost.ID(), ProtocolID)
		if err == nil && len(protos) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cl := NewClient(chost)
	got, err := cl.FetchTipsetChain(context.Background(), head, 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, chain[0][0].Cid(), got[0][0].Cid())
	require.Equal(t, chain[2][0].Cid(), got[2][0].Cid())
	// Request wire assertions.
	require.Equal(t, head, cs.gotHead)
	require.Equal(t, uint64(3), cs.gotLen)
	require.Equal(t, optHeaders, cs.gotOpts)
	st := cl.ClientStats()
	require.Equal(t, uint64(1), st.Succeeded)
	require.Equal(t, uint64(0), st.Failed)
}

func TestClient_PartialAccepted(t *testing.T) {
	chain := mkChain(t, 3, 200)
	head := []cid.Cid{chain[0][0].Cid()}
	cs := &cannedServer{raw: encodeResponse(t, statusPartial, "", chain[:1])}
	chost, shost := hostPairWith(t, cs)
	cl := NewClient(chost)
	got, err := cl.FetchTipsetChain(context.Background(), head, 3)
	require.NoError(t, err)
	require.Len(t, got, 1)
	_ = shost
}

func TestClient_NotFoundFails(t *testing.T) {
	cs := &cannedServer{raw: encodeResponse(t, statusNotFound, "nope", nil)}
	chost, _ := hostPairWith(t, cs)
	cl := NewClient(chost)
	_, err := cl.FetchTipsetChain(context.Background(), []cid.Cid{mkTestCID(t, "whatever")}, 2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "NotFound")
	require.Equal(t, uint64(1), cl.ClientStats().Failed)
}

func TestClient_SplicedChainRejected(t *testing.T) {
	// Server returns a valid-looking chain that does NOT match the
	// requested head — must be rejected by verification.
	chain := mkChain(t, 2, 300)
	cs := &cannedServer{raw: encodeResponse(t, statusOK, "", chain)}
	chost, _ := hostPairWith(t, cs)
	cl := NewClient(chost)
	_, err := cl.FetchTipsetChain(context.Background(), []cid.Cid{mkTestCID(t, "different-head")}, 2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification")
}

func TestClient_NoPeers(t *testing.T) {
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	cl := NewClient(h)
	_, err = cl.FetchTipsetChain(context.Background(), []cid.Cid{mkTestCID(t, "x")}, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no connected peers")
}

// hostPairWith wires a canned server + connected client host, waiting
// for identify.
func hostPairWith(t *testing.T, cs *cannedServer) (host.Host, host.Host) {
	t.Helper()
	c, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	s, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	cs.register(t, s)
	require.NoError(t, c.Connect(context.Background(), peer.AddrInfo{ID: s.ID(), Addrs: s.Addrs()}))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		protos, err := c.Peerstore().SupportsProtocols(s.ID(), ProtocolID)
		if err == nil && len(protos) > 0 {
			return c, s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("identify never propagated protocol support")
	return nil, nil
}
