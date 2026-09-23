// Package peerheads records the chain heads libp2p peers announce to us
// (#154), so a bridge-off Lantern (--no-fallback-rpc) has a continuous,
// RPC-free head quorum instead of none.
//
// Feeders:
//   - Hello (/fil/hello/1.0.0): the peer's heaviest tipset at connect time.
//   - Gossipsub /fil/blocks: every block a peer forwards (continuous, fresh).
//
// Honest boundary: peer IDs are Sybil-cheap, so peers are grouped by
// network prefix (IPv4 /16, IPv6 /32). A swarm of sybils in one prefix is
// one voter. Peers without an IP address (relayed / unknown) are ignored.
// This raises the cost of an eclipse from "N peer IDs" to "N networks";
// finality (F3) is what fully closes the unfinalized tip.
package peerheads

import (
	"net"
	"sort"
	"sync"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// DefaultCap bounds how many peers the book remembers.
const DefaultCap = 512

// Head is the latest head one peer announced.
type Head struct {
	Peer   peer.ID
	Group  string // network prefix voter key, never empty
	Cids   []cid.Cid
	Height abi.ChainEpoch
	// Parents of the announced block (gossip only; nil for Hello). Lets
	// the head quorum check the peer is on OUR chain, not just our height.
	Parents []cid.Cid
	At      time.Time
}

// Book is a bounded, concurrency-safe per-peer head record.
type Book struct {
	addrOf func(peer.ID) multiaddr.Multiaddr
	now    func() time.Time
	cap    int

	mu    sync.Mutex
	heads map[peer.ID]Head
}

// New builds a Book. addrOf resolves a peer's current remote address (the
// daemon passes the first live connection's RemoteMultiaddr).
func New(addrOf func(peer.ID) multiaddr.Multiaddr) *Book {
	return &Book{addrOf: addrOf, now: time.Now, cap: DefaultCap, heads: map[peer.ID]Head{}}
}

// Observe records that peer p announced a tipset (cids) at height h. Only
// moves forward per peer. Peers with no IP-derived group are dropped.
func (b *Book) Observe(p peer.ID, cids []cid.Cid, h abi.ChainEpoch) {
	b.observe(p, cids, h, nil)
}

// ObserveBlock records a gossiped block (CID, height, parents) from peer p.
func (b *Book) ObserveBlock(p peer.ID, c cid.Cid, h abi.ChainEpoch, parents []cid.Cid) {
	b.observe(p, []cid.Cid{c}, h, parents)
}

func (b *Book) observe(p peer.ID, cids []cid.Cid, h abi.ChainEpoch, parents []cid.Cid) {
	if b == nil || p == "" || len(cids) == 0 {
		return
	}
	var group string
	if b.addrOf != nil {
		group = GroupOf(b.addrOf(p))
	}
	if group == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.heads[p]; ok && cur.Height > h {
		return
	}
	b.heads[p] = Head{Peer: p, Group: group, Cids: append([]cid.Cid(nil), cids...), Height: h,
		Parents: append([]cid.Cid(nil), parents...), At: b.now()}
	if len(b.heads) > b.cap {
		b.evictOldestLocked()
	}
}

func (b *Book) evictOldestLocked() {
	var oldest peer.ID
	var t time.Time
	for p, h := range b.heads {
		if oldest == "" || h.At.Before(t) {
			oldest, t = p, h.At
		}
	}
	delete(b.heads, oldest)
}

// Recent returns heads observed within maxAge, sorted by peer ID.
func (b *Book) Recent(maxAge time.Duration) []Head {
	if b == nil {
		return nil
	}
	cutoff := b.now().Add(-maxAge)
	b.mu.Lock()
	out := make([]Head, 0, len(b.heads))
	for _, h := range b.heads {
		if !h.At.Before(cutoff) {
			out = append(out, h)
		}
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	return out
}

// GroupOf returns the network-prefix voter key for an address: IPv4 /16,
// IPv6 /32. Loopback/private addresses keep their full IP (they are
// distinct local operators in a lab, and never reachable from outside).
// Non-IP or nil addresses return "".
func GroupOf(ma multiaddr.Multiaddr) string {
	if ma == nil {
		return ""
	}
	ip, err := manet.ToIP(ma)
	if err != nil || ip == nil {
		return ""
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return "ip:" + ip.String()
	}
	if v4 := ip.To4(); v4 != nil {
		return "ip4:" + (&net.IPNet{IP: v4.Mask(net.CIDRMask(16, 32)), Mask: net.CIDRMask(16, 32)}).String()
	}
	return "ip6:" + (&net.IPNet{IP: ip.Mask(net.CIDRMask(32, 128)), Mask: net.CIDRMask(32, 128)}).String()
}
