package peerheads

import (
	"testing"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	mh "github.com/multiformats/go-multihash"
)

func mkCID(t *testing.T, s string) cid.Cid {
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.DagCBOR, h)
}

func ma(t *testing.T, s string) multiaddr.Multiaddr {
	m, err := multiaddr.NewMultiaddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGroupOf(t *testing.T) {
	cases := map[string]string{
		"/ip4/8.8.4.4/tcp/1":        "ip4:8.8.0.0/16",
		"/ip4/8.8.200.1/udp/1/quic": "ip4:8.8.0.0/16",
		"/ip4/9.9.9.9/tcp/1":        "ip4:9.9.0.0/16",
		"/ip6/2001:db8:1::1/tcp/1":  "ip6:2001:db8::/32",
		"/ip4/127.0.0.1/tcp/1":      "ip:127.0.0.1",
		"/ip4/10.20.0.11/tcp/1":     "ip:10.20.0.11",
	}
	for in, want := range cases {
		if got := GroupOf(ma(t, in)); got != want {
			t.Errorf("GroupOf(%s) = %q, want %q", in, got, want)
		}
	}
	if GroupOf(nil) != "" {
		t.Error("nil addr => empty group")
	}
	if GroupOf(ma(t, "/dns4/example.com/tcp/1")) != "" {
		t.Error("non-IP addr => empty group")
	}
}

func TestBook_ObserveRecentAndMonotonic(t *testing.T) {
	addrs := map[peer.ID]multiaddr.Multiaddr{
		"p1": ma(t, "/ip4/8.8.4.4/tcp/1"),
		"p2": ma(t, "/ip4/9.9.9.9/tcp/1"),
	}
	b := New(func(p peer.ID) multiaddr.Multiaddr { return addrs[p] })
	now := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return now }

	b.Observe("p1", []cid.Cid{mkCID(t, "a")}, 100)
	b.Observe("p1", []cid.Cid{mkCID(t, "old")}, 90) // older: ignored
	b.Observe("p2", []cid.Cid{mkCID(t, "b")}, 101)
	b.Observe("p3", []cid.Cid{mkCID(t, "c")}, 101) // no address: ignored
	b.Observe("p1", nil, 200)                      // no cids: ignored

	r := b.Recent(time.Minute)
	if len(r) != 2 || r[0].Peer != "p1" || r[0].Height != abi.ChainEpoch(100) || r[1].Group != "ip4:9.9.0.0/16" {
		t.Fatalf("unexpected recent: %+v", r)
	}

	now = now.Add(2 * time.Minute)
	if len(b.Recent(time.Minute)) != 0 {
		t.Fatal("stale heads must age out")
	}
}

func TestBook_Cap(t *testing.T) {
	b := New(func(peer.ID) multiaddr.Multiaddr { return ma(t, "/ip4/8.8.4.4/tcp/1") })
	b.cap = 3
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		b.now = func() time.Time { return ts }
		b.Observe(peer.ID(rune('a'+i)), []cid.Cid{mkCID(t, "x")}, abi.ChainEpoch(i))
	}
	r := b.Recent(time.Hour)
	if len(r) != 3 || r[0].Peer != "c" {
		t.Fatalf("cap must evict oldest, got %+v", r)
	}
}
