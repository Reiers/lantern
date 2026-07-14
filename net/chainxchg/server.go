package chainxchg

// ChainExchange server (issue #91 follow-up: the responder half that
// actually SERVES tipsets).
//
// The base Service in chainxchg.go always replies NotFound; that keeps
// us reachable on the protocol and stops real peers from trimming us
// as "dead". This file adds an optional Source: when wired, Service
// parses the incoming Request, walks the local header store backward
// from the requested head for up to Length tipsets, and returns the
// verified header chain to the peer. Messages are always null in the
// response (headers-only; matches what the client fetches under #91).
//
// Trust boundary:
//   - We only serve HEADERS. That means every field a peer receives is
//     content-addressed and CID-verifiable against the block CID; a
//     malicious server cannot fabricate a header without breaking a
//     BLS aggregate that gnark-crypto rejects.
//   - We refuse to serve out of malformed or truncated requests
//     (statusBadRequest), and downgrade to NotFound (not InternalError)
//     when a local read genuinely can't find the requested tipset.
//   - Length is clamped to MaxRequestLength = 900 (protocol cap).
//
// Wire format is the same encoder pattern used by the client tests, so
// a Lantern client speaking to a Lantern server round-trips correctly.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
	cbg "github.com/whyrusleeping/cbor-gen"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// Source is the read surface a ChainExchange server needs from the
// local header store. Small enough that store.Store satisfies it out
// of the box (see chain/header/store/store.go), and small enough that
// an in-memory test source is trivial to write.
//
// GetTipSet MUST return a *TipSet whose blocks match the given key
// exactly, or an error. Callers use ErrNotFound-style errors (or any
// non-nil error) to mean "not served"; the server responds with
// statusNotFound in that case.
type Source interface {
	GetTipSet(tsk ltypes.TipSetKey) (*ltypes.TipSet, error)
}

// SetSource wires an optional local source of tipsets. When called
// with a non-nil src, the Service will serve real header chains
// instead of NotFound. Idempotent. Passing nil reverts to the
// NotFound-only mode.
func (s *Service) SetSource(src Source) { s.src = src }

// serveResponse walks src backward from head for up to length tipsets
// and returns the encoded Response bytes (statusOK / statusPartial).
// Returns statusNotFound if the head itself can't be resolved.
func serveResponse(src Source, headCIDs []cid.Cid, length uint64) ([]byte, error) {
	if len(headCIDs) == 0 {
		return encodeServerResponse(statusBadRequest, "empty head", nil)
	}
	if length == 0 {
		length = 1
	}
	if length > MaxRequestLength {
		length = MaxRequestLength
	}

	chain := make([][]*ltypes.BlockHeader, 0, length)
	currentKey := ltypes.NewTipSetKey(headCIDs...)
	var lastHeight abi.ChainEpoch = -1
	for uint64(len(chain)) < length {
		ts, err := src.GetTipSet(currentKey)
		if err != nil {
			if len(chain) == 0 {
				return encodeServerResponse(statusNotFound, "head not in store", nil)
			}
			// Ran out of parents mid-walk: legal Partial response.
			break
		}
		if ts == nil {
			break
		}
		// Sanity: parents must strictly decrease in height (with
		// genesis being height 0). A pathological store that
		// returns a self-referential tipset would otherwise loop
		// until length is exhausted.
		if lastHeight >= 0 && ts.Height() >= lastHeight {
			return encodeServerResponse(statusInternalError, "non-decreasing parent height", chain)
		}
		lastHeight = ts.Height()
		chain = append(chain, ts.Blocks())
		if ts.Height() == 0 {
			// Reached genesis; nothing further to walk.
			break
		}
		parents := ts.Parents()
		if parents.IsEmpty() {
			break
		}
		currentKey = parents
	}

	status := statusOK
	if uint64(len(chain)) < length {
		status = statusPartial
	}
	return encodeServerResponse(status, "", chain)
}

// encodeServerResponse writes the same wire shape as the client-side
// encoder helper (see client_test.go::encodeResponse), so a Lantern
// server round-trips against a Lantern client without divergence.
//
// Envelope: array(3){status, errMsg, chain []BSTipSet} where each
// BSTipSet is array(2){blocks []BlockHeader, messages}. Messages is
// always CborNull for a headers-only server.
func encodeServerResponse(status uint64, errMsg string, chain [][]*ltypes.BlockHeader) ([]byte, error) {
	var buf bytes.Buffer
	cw := cbg.NewCborWriter(&buf)
	if err := cw.WriteMajorTypeHeader(cbg.MajArray, 3); err != nil {
		return nil, err
	}
	if err := cw.WriteMajorTypeHeader(cbg.MajUnsignedInt, status); err != nil {
		return nil, err
	}
	if err := cw.WriteMajorTypeHeader(cbg.MajTextString, uint64(len(errMsg))); err != nil {
		return nil, err
	}
	if errMsg != "" {
		if _, err := cw.Write([]byte(errMsg)); err != nil {
			return nil, err
		}
	}
	if err := cw.WriteMajorTypeHeader(cbg.MajArray, uint64(len(chain))); err != nil {
		return nil, err
	}
	for _, blocks := range chain {
		if err := cw.WriteMajorTypeHeader(cbg.MajArray, 2); err != nil {
			return nil, err
		}
		if err := cw.WriteMajorTypeHeader(cbg.MajArray, uint64(len(blocks))); err != nil {
			return nil, err
		}
		for _, b := range blocks {
			if err := b.MarshalCBOR(cw); err != nil {
				return nil, err
			}
		}
		if _, err := cw.Write(cbg.CborNull); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// readServerRequest parses an incoming Request{Head []cid, Length,
// Options} off the wire. Reads at most maxRequestBytes; refuses
// malformed input. Options is returned so callers can decide whether
// to serve messages too (we ignore Messages option today; headers-only).
func readServerRequest(r io.Reader) (head []cid.Cid, length, options uint64, err error) {
	cr := cbg.NewCborReader(io.LimitReader(r, int64(maxRequestBytes)))
	maj, n, err := cr.ReadHeader()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("request envelope: %w", err)
	}
	if maj != cbg.MajArray || n != 3 {
		return nil, 0, 0, fmt.Errorf("request envelope: want array(3), got maj=%d n=%d", maj, n)
	}
	maj, hn, err := cr.ReadHeader()
	if err != nil || maj != cbg.MajArray {
		return nil, 0, 0, fmt.Errorf("request head header: maj=%d err=%v", maj, err)
	}
	if hn == 0 {
		return nil, 0, 0, errors.New("request head: empty")
	}
	if hn > uint64(maxBlocksPerTipset) {
		return nil, 0, 0, fmt.Errorf("request head: %d blocks exceeds cap %d", hn, maxBlocksPerTipset)
	}
	head = make([]cid.Cid, 0, hn)
	for i := uint64(0); i < hn; i++ {
		c, err := cbg.ReadCid(cr)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("request head cid %d: %w", i, err)
		}
		head = append(head, c)
	}
	maj, length, err = cr.ReadHeader()
	if err != nil || maj != cbg.MajUnsignedInt {
		return nil, 0, 0, fmt.Errorf("request length: maj=%d err=%v", maj, err)
	}
	maj, options, err = cr.ReadHeader()
	if err != nil || maj != cbg.MajUnsignedInt {
		return nil, 0, 0, fmt.Errorf("request options: maj=%d err=%v", maj, err)
	}
	return head, length, options, nil
}

// serveStream is the source-backed handler wired by handleStream when
// Service.src is set. Any parse or serve error is folded into an
// on-wire response (BadRequest / NotFound / InternalError) rather than
// closing the stream, so a peer never has to distinguish "not speaking
// the protocol" from "no such tipset".
func (s *Service) serveStream(str network.Stream) {
	defer func() { _ = str.Close() }()

	_ = str.SetReadDeadline(time.Now().Add(streamReadDeadline))
	head, length, _, err := readServerRequest(str)
	_ = str.SetReadDeadline(time.Time{})

	var resp []byte
	if err != nil {
		s.rejected.Add(1)
		remote := str.Conn().RemotePeer()
		log.Debugw("chainxchg server: bad request", "peer", remote, "err", err)
		resp, _ = encodeServerResponse(statusBadRequest, "malformed request", nil)
	} else {
		resp, err = serveResponse(s.src, head, length)
		if err != nil {
			s.rejected.Add(1)
			log.Debugw("chainxchg server: encode failed", "err", err)
			resp, _ = encodeServerResponse(statusInternalError, "encode failed", nil)
		}
	}

	_ = str.SetWriteDeadline(time.Now().Add(streamWriteDeadline))
	if _, werr := str.Write(resp); werr != nil {
		s.rejected.Add(1)
		log.Debugw("chainxchg server: write failed", "err", werr)
		return
	}
	_ = str.SetWriteDeadline(time.Time{})
	s.received.Add(1)
}
