// Phase 10: NetInfo adapter exposing the live libp2p Host through the
// narrow surface rpc/handlers.NetInfo expects.
//
// This file is the single point where rpc/handlers consumes libp2p APIs.
// Anything else in net/libp2p stays library-internal.

package libp2p

import (
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/rpc/handlers"
)

// NetInfo returns a handlers.NetInfo wrapper around h. The returned value
// holds h by reference: it's cheap to construct and safe to share.
func (h *Host) NetInfo() handlers.NetInfo {
	return netInfoAdapter{h: h}
}

type netInfoAdapter struct {
	h *Host
}

func (a netInfoAdapter) Peers() []handlers.NetInfoPeer {
	if a.h == nil || a.h.H == nil {
		return nil
	}
	peers := a.h.H.Network().Peers()
	out := make([]handlers.NetInfoPeer, 0, len(peers))
	ps := a.h.H.Peerstore()
	for _, p := range peers {
		addrs := ps.Addrs(p)
		strs := make([]string, 0, len(addrs))
		for _, ma := range addrs {
			strs = append(strs, ma.String())
		}
		out = append(out, handlers.NetInfoPeer{
			ID:    p.String(),
			Addrs: strs,
		})
	}
	return out
}

func (a netInfoAdapter) AgentVersion(peerID string) string {
	if a.h == nil || a.h.H == nil {
		return ""
	}
	pid, err := peer.Decode(peerID)
	if err != nil {
		return ""
	}
	v, err := a.h.H.Peerstore().Get(pid, "AgentVersion")
	if err != nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (a netInfoAdapter) Connectedness(peerID string) int {
	if a.h == nil || a.h.H == nil {
		return 0
	}
	pid, err := peer.Decode(peerID)
	if err != nil {
		return 0
	}
	return int(a.h.H.Network().Connectedness(pid))
}

func (a netInfoAdapter) Listening() bool {
	if a.h == nil || a.h.H == nil {
		return false
	}
	return len(a.h.H.Network().ListenAddresses()) > 0
}

func (a netInfoAdapter) BandwidthTotals() api.NetBandwidthStats {
	if a.h == nil || a.h.BW == nil {
		return api.NetBandwidthStats{}
	}
	s := a.h.BW.GetBandwidthTotals()
	return api.NetBandwidthStats{
		TotalIn:  s.TotalIn,
		TotalOut: s.TotalOut,
		RateIn:   s.RateIn,
		RateOut:  s.RateOut,
	}
}

func (a netInfoAdapter) AutoNatStatus() api.NatInfo {
	if a.h == nil || a.h.H == nil {
		return api.NatInfo{}
	}
	return api.NatInfo{
		Reachability: int(a.h.Reachability()),
		PublicAddrs:  a.h.PublicAddrs(),
	}
}
