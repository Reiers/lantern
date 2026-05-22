// Package chainxchg implements the responder side of the Filecoin
// ChainExchange protocol (/fil/chain/xchg/0.0.1) used by Lotus / Forest
// peers to backfill blocks they missed via gossipsub.
//
// Issue #17 (parent: #16): real Filecoin peers drop us within ~30s of
// connecting because we don't speak the protocols their connmgr scores
// positively. Hello (issue #16) confirmed wire-format correctness but
// didn't move stuck-rate. The next hypothesis is that ChainExchange
// queries failing against us look like a dead-node signal and accelerate
// their trim pass.
//
// Minimum-viable scope: respond to every incoming request with a
// well-formed CBOR Response{Status: NotFound, ErrorMessage: "", Chain:
// []}. We don't have to actually serve any data; being REACHABLE on the
// protocol is what matters. A real Filecoin node looking at us sees
// "doesn't have this tipset" (normal node behaviour) instead of
// "doesn't speak the protocol" (suspicious).
//
// Why handwritten CBOR: the upstream Lotus exchange package's
// cbor_gen.go pulls in BlockHeader + Message + SignedMessage + multiple
// helper types. We don't need any of that for the NotFound-only
// responder. A 5-byte hardcoded response keeps the dependency surface
// at zero and the wire format easy to verify by inspection.

package chainxchg

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// ProtocolID is the libp2p protocol ID for Filecoin ChainExchange.
const ProtocolID protocol.ID = "/fil/chain/xchg/0.0.1"

// Status codes from Lotus chain/exchange/protocol.go. We only ever
// emit NotFound.
const (
	statusOK            uint64 = 0
	statusNotFound      uint64 = 201
	statusBadRequest    uint64 = 204
	statusInternalError uint64 = 203
)

// notFoundResponse is the CBOR encoding of:
//
//	Response{Status: 201 (NotFound), ErrorMessage: "", Chain: []}
//
// Wire format breakdown:
//
//	0x83       major type 4 (array), length 3
//	0x18 0xC9  major type 0 (uint), 1-byte payload, value 201
//	0x60       major type 3 (text string), length 0
//	0x80       major type 4 (array), length 0
//
// Verified against upstream cbor_gen.go Response.MarshalCBOR layout.
var notFoundResponse = []byte{0x83, 0x18, 0xC9, 0x60, 0x80}

var (
	streamReadDeadline  = 10 * time.Second
	streamWriteDeadline = 5 * time.Second
	// MaxRequestBytes caps how much we'll read off the wire before
	// giving up. Real ChainExchange requests are small (a few hundred
	// bytes for typical Length + Head). 64 KiB is generous and still
	// bounds memory regardless of malicious senders.
	maxRequestBytes = 64 * 1024
)

var log = logging.Logger("lantern/chainxchg")

// Service is the ChainExchange responder bound to a libp2p host.
type Service struct {
	h        host.Host
	received atomic.Uint64 // requests we processed (drained + responded)
	rejected atomic.Uint64 // requests we dropped (read error, oversized)
}

// Stats reports observable activity. Exposed for the dashboard.
type Stats struct {
	Received uint64 `json:"received"`
	Rejected uint64 `json:"rejected"`
}

// Stats returns a snapshot.
func (s *Service) Stats() Stats {
	return Stats{
		Received: s.received.Load(),
		Rejected: s.rejected.Load(),
	}
}

// NewService constructs a Service. Caller must call Register() to attach
// the stream handler.
func NewService(h host.Host) *Service {
	return &Service{h: h}
}

// Register attaches the ChainExchange stream handler to the host.
// Idempotent.
func (s *Service) Register() {
	s.h.SetStreamHandler(ProtocolID, s.handleStream)
}

// handleStream reads a request (and discards it), then writes a
// well-formed NotFound response. Bounded by read + write deadlines so a
// slow peer can't tie up resources.
func (s *Service) handleStream(str network.Stream) {
	defer func() { _ = str.Close() }()

	_ = str.SetReadDeadline(time.Now().Add(streamReadDeadline))
	if err := drainBounded(str, maxRequestBytes); err != nil {
		s.rejected.Add(1)
		remote := str.Conn().RemotePeer()
		log.Debugw("chainxchg: failed to read request", "peer", remote, "err", err)
		_ = str.Reset()
		return
	}
	_ = str.SetReadDeadline(time.Time{})

	_ = str.SetWriteDeadline(time.Now().Add(streamWriteDeadline))
	if _, err := str.Write(notFoundResponse); err != nil {
		s.rejected.Add(1)
		log.Debugw("chainxchg: failed to write response",
			"peer", str.Conn().RemotePeer(), "err", err)
		return
	}
	_ = str.SetWriteDeadline(time.Time{})
	s.received.Add(1)
}

// drainBounded reads from r until EOF or n bytes, returning any read
// error other than EOF. We don't parse the request; we just need to
// consume it so the remote's write completes and we can reply.
// CloseWrite from the peer (typical Lotus client behaviour) ends the
// read with EOF.
func drainBounded(r io.Reader, n int) error {
	buf := make([]byte, 4096)
	read := 0
	for {
		take := len(buf)
		if remaining := n - read; remaining < take {
			take = remaining
		}
		if take <= 0 {
			return errors.New("request exceeded max bytes")
		}
		got, err := r.Read(buf[:take])
		read += got
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// silence the linter when context is unused at top level; reserved for
// future enhancement (responding with real tipsets from a store).
var _ = context.Background
var _ = statusOK
var _ = statusBadRequest
var _ = statusInternalError
