// Package hello implements the Filecoin `/fil/hello/1.0.0` protocol.
//
// Issue #16: Lantern peers were being dropped by remote Filecoin nodes
// within 30s of connecting because we don't speak the protocols their
// connmgr scores positively. Hello is the lowest-effort, highest-impact
// fix: every Lotus / Forest node initiates Hello on every new connection
// and treats no-Hello peers as untrusted in their score model. Speaking
// Hello (a) keeps us from getting trimmed by their connmgr, and (b) lets
// us tag remote peers as "real Filecoin nodes" in our own connmgr so we
// don't trim them either.
//
// Wire format follows Lotus byte-for-byte. The CBOR codec in cbor_gen.go
// is the upstream Lotus generated codec, verbatim (see LICENSE-LOTUS).
//
// Design intentionally narrower than Lotus's hello service:
//   - We don't trigger chain sync on receiving Hello (Lantern syncs from
//     gateway + Bitswap; we don't FetchTipSet from arbitrary peers).
//   - We don't reply with a LatencyMessage. Lotus does this for clock-skew
//     measurement; Lantern doesn't have a clock-skew use case yet.
//     Receiving a Hello and closing the stream cleanly is enough to
//     register us as "real Filecoin" in the remote peer's view.
//   - We DO tag any peer that sends us a valid (matching-genesis) Hello
//     in our local ConnManager with weight=10, label="fcpeer". This is
//     Layer 3 from the issue: peers that speak Filecoin shouldn't be
//     trimmed by our low-water-mark pass.
//   - We DO proactively SayHello to every new outbound libp2p connection
//     so the OTHER side tags us too. That's the actual fix for stuck
//     peer count.

package hello

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	cborutil "github.com/filecoin-project/go-cbor-util"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// ProtocolID is the libp2p protocol ID for Filecoin Hello.
const ProtocolID protocol.ID = "/fil/hello/1.0.0"

// PeerTag is the ConnManager tag we apply to peers who pass our Hello
// genesis check. Worth +10 score so the connmgr trim pass keeps them
// over random peers we picked up via DHT.
const PeerTag = "fcpeer"
const PeerTagWeight = 10

var (
	streamDeadline    = 10 * time.Second
	streamOpenTimeout = 30 * time.Second
)

var log = logging.Logger("lantern/hello")

// HeadProvider returns the current chain head: tipset CIDs, epoch, and
// parent weight. Lantern's daemon supplies a closure backed by the
// trusted root + (when set) the header store.
type HeadProvider func() (cids []cid.Cid, height int64, weight string)

// Service wires the Hello protocol onto a libp2p Host.
type Service struct {
	h           host.Host
	genesis     cid.Cid
	head        HeadProvider
	helloRecv   atomic.Uint64 // counter: valid Hello messages received
	helloSent   atomic.Uint64 // counter: Hello messages we sent
	helloReject atomic.Uint64 // counter: rejected (genesis mismatch / read error)
}

// Stats reports observable Hello activity. Exposed for the dashboard.
type Stats struct {
	Received uint64 `json:"received"`
	Sent     uint64 `json:"sent"`
	Rejected uint64 `json:"rejected"`
}

// Stats returns a snapshot of Hello counters.
func (s *Service) Stats() Stats {
	return Stats{
		Received: s.helloRecv.Load(),
		Sent:     s.helloSent.Load(),
		Rejected: s.helloReject.Load(),
	}
}

// NewService constructs a Hello service. Caller must call Register to
// attach the stream handler.
//
// genesis is the canonical genesis CID for this network (mainnet, calibnet).
// head is a callback that returns the current chain head; Service will call
// it on every outbound SayHello so the wire message reflects current state.
func NewService(h host.Host, genesis cid.Cid, head HeadProvider) *Service {
	return &Service{
		h:       h,
		genesis: genesis,
		head:    head,
	}
}

// Register attaches the Hello stream handler to the host. Idempotent.
func (s *Service) Register() {
	s.h.SetStreamHandler(ProtocolID, s.handleStream)
}

// handleStream is invoked when a remote peer dials us with Hello.
func (s *Service) handleStream(str network.Stream) {
	remote := str.Conn().RemotePeer()
	_ = str.SetReadDeadline(time.Now().Add(streamDeadline))

	var hmsg HelloMessage
	if err := cborutil.ReadCborRPC(str, &hmsg); err != nil {
		s.helloReject.Add(1)
		log.Debugw("failed to read Hello", "peer", remote, "err", err)
		_ = str.Conn().Close()
		return
	}
	_ = str.SetReadDeadline(time.Time{})

	if !hmsg.GenesisHash.Equals(s.genesis) {
		s.helloReject.Add(1)
		log.Debugw("Hello genesis mismatch", "peer", remote,
			"theirs", hmsg.GenesisHash, "ours", s.genesis)
		_ = str.Conn().Close()
		return
	}

	s.helloRecv.Add(1)
	// Issue #16 Layer 3: tag the peer as a real Filecoin node so our
	// connmgr trim pass keeps them.
	s.h.ConnManager().TagPeer(remote, PeerTag, PeerTagWeight)
	log.Debugw("Hello received", "peer", remote, "epoch", hmsg.HeaviestTipSetHeight)

	// Close the stream cleanly. We don't bother with a LatencyMessage
	// reply; we just need the remote to see a clean close so they
	// register us as having "spoken Filecoin." Some clients write
	// LatencyMessage and a close on either side is acceptable per the
	// protocol.
	_ = str.Close()
}

// SayHello dials the remote peer's Hello protocol and sends our
// HelloMessage. Idempotent (libp2p multiplexes streams). Bounded by
// streamOpenTimeout + streamDeadline.
func (s *Service) SayHello(ctx context.Context, pid peer.ID) error {
	if s.head == nil {
		return fmt.Errorf("hello: no head provider configured")
	}
	cids, height, weight := s.head()
	if len(cids) == 0 || !s.genesis.Defined() {
		return fmt.Errorf("hello: head not yet known")
	}

	sctx, cancel := context.WithTimeout(ctx, streamOpenTimeout)
	defer cancel()
	str, err := s.h.NewStream(sctx, pid, ProtocolID)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	w, _ := parseBig(weight)
	hmsg := &HelloMessage{
		HeaviestTipSet:       cids,
		HeaviestTipSetHeight: chainEpoch(height),
		HeaviestTipSetWeight: w,
		GenesisHash:          s.genesis,
	}

	_ = str.SetWriteDeadline(time.Now().Add(streamDeadline))
	if err := cborutil.WriteCborRPC(str, hmsg); err != nil {
		_ = str.Close()
		return fmt.Errorf("write Hello: %w", err)
	}
	_ = str.SetWriteDeadline(time.Time{})
	if err := str.CloseWrite(); err != nil {
		log.Debugw("CloseWrite", "peer", pid, "err", err)
	}
	s.helloSent.Add(1)
	// Best-effort discard of any reply (LatencyMessage from Lotus peers).
	// We don't act on it; just drain so the remote's write completes
	// cleanly.
	go func() {
		_ = str.SetReadDeadline(time.Now().Add(streamDeadline))
		var lat LatencyMessage
		_ = cborutil.ReadCborRPC(str, &lat)
		_ = str.Close()
	}()
	return nil
}

// WatchNewConns spawns a goroutine that listens for new outbound libp2p
// connections and proactively SayHello to each remote. This is the
// loop that actually keeps peer count up over time: every new peer we
// dial gets a Hello within the first second, so remote connmgrs score
// us positively from the start.
//
// Cancellation: stops when ctx is done.
func (s *Service) WatchNewConns(ctx context.Context) {
	type connEvent struct {
		pid peer.ID
		dir network.Direction
	}
	ch := make(chan connEvent, 64)
	notifiee := &connNotifiee{
		onConnect: func(c network.Conn) {
			select {
			case ch <- connEvent{pid: c.RemotePeer(), dir: c.Stat().Direction}:
			default:
				// channel full; skip. The keepalive loop will eventually
				// SayHello to the peer via the periodic sweep below.
			}
		},
	}
	s.h.Network().Notify(notifiee)
	defer s.h.Network().StopNotify(notifiee)

	// Periodic sweep: SayHello to every connected peer we haven't said
	// hello to recently. Catches peers we connected to BEFORE WatchNewConns
	// started, and peers we dropped from the channel above.
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			// Only proactively Hello on outbound connections. Inbound
			// peers will Hello us; if they don't, they're not running
			// the protocol and there's nothing we can do.
			if ev.dir == network.DirOutbound {
				go s.helloOne(ctx, ev.pid)
			}
		case <-tick.C:
			s.helloSweep(ctx)
		}
	}
}

// helloOne dials a single peer and SayHello. Logged at debug to avoid
// spamming logs.
func (s *Service) helloOne(ctx context.Context, pid peer.ID) {
	hctx, cancel := context.WithTimeout(ctx, streamOpenTimeout+streamDeadline)
	defer cancel()
	if err := s.SayHello(hctx, pid); err != nil {
		log.Debugw("SayHello failed", "peer", pid, "err", err)
	}
}

// helloSweep picks up peers that connected before WatchNewConns started
// or peers the connNotifiee channel dropped. Cheap: a no-op when we've
// already greeted everyone.
func (s *Service) helloSweep(ctx context.Context) {
	for _, pid := range s.h.Network().Peers() {
		// Skip peers we've already tagged (they've already Hello'd us
		// or we've already Hello'd them).
		if t := s.h.ConnManager().GetTagInfo(pid); t != nil {
			if _, ok := t.Tags[PeerTag]; ok {
				continue
			}
		}
		s.helloOne(ctx, pid)
	}
}

// ---- internal helpers ----

// chainEpoch wraps the int64 -> abi.ChainEpoch cast so callers don't
// need to import go-state-types/abi just to call SayHello.
func chainEpoch(h int64) chainEpochT { return chainEpochT(h) }

// parseBig parses a parent weight as decimal. Lotus stores it as
// big.Int; we receive it from Lantern's TrustedRoot.ParentWeight stringer.
// Returns big.Zero() on parse failure (Hello peers tolerate a wrong
// weight; they only fail-close on genesis mismatch).
func parseBig(s string) (bigT, error) {
	return parseBigImpl(s)
}

// connNotifiee adapts a function callback into a libp2p Notifiee.
type connNotifiee struct {
	onConnect func(network.Conn)
}

func (n *connNotifiee) Listen(network.Network, multiaddrT)      {}
func (n *connNotifiee) ListenClose(network.Network, multiaddrT) {}
func (n *connNotifiee) Connected(net network.Network, c network.Conn) {
	if n.onConnect != nil {
		n.onConnect(c)
	}
}
func (n *connNotifiee) Disconnected(network.Network, network.Conn) {}
