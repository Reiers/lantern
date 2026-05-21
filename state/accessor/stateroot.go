// StateRoot decodes the on-chain root tuple `[version, actorsRoot, infoRoot]`
// stored at the state root CID.
//
// Lifted from: github.com/filecoin-project/lotus/chain/state/statetree.go
// (StateRoot struct, state tree version constants). Reimplemented as a plain
// hand-rolled CBOR decoder to avoid the Lotus VM dependency.

package accessor

import (
	"bytes"
	"fmt"
	"io"

	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"
)

// StateTreeVersion is the on-chain state-tree version. Filecoin currently
// runs at version 5 (since network version 18, the FVM Stable Memory rollout).
type StateTreeVersion uint64

const (
	StateTreeVersion0 StateTreeVersion = 0
	StateTreeVersion1 StateTreeVersion = 1
	StateTreeVersion2 StateTreeVersion = 2
	StateTreeVersion3 StateTreeVersion = 3
	StateTreeVersion4 StateTreeVersion = 4
	StateTreeVersion5 StateTreeVersion = 5
)

// StateRoot is the value stored at a state-root CID for version 1+.
// (Version 0 stored the actors HAMT CID directly with no wrapper.)
type StateRoot struct {
	Version StateTreeVersion
	Actors  cid.Cid // top-level state-tree HAMT root, keyed by ID-address
	Info    cid.Cid // miscellaneous chain info; not currently used
}

// DecodeStateRoot parses the raw IPLD-DAG-CBOR bytes of a state-root block.
// Returns an error if the bytes don't conform to the documented layout.
//
// Layout: 3-element CBOR array [uint version, cid-link actors, cid-link info].
func DecodeStateRoot(raw []byte) (*StateRoot, error) {
	br := bytes.NewReader(raw)
	maj, extra, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("reading state-root header: %w", err)
	}
	if maj != cbg.MajArray {
		return nil, fmt.Errorf("state root not a CBOR array (got major %d)", maj)
	}
	if extra != 3 {
		return nil, fmt.Errorf("state root array length %d, want 3", extra)
	}

	// Field 1: version (CBOR uint).
	maj, extra, err = cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("reading version: %w", err)
	}
	if maj != cbg.MajUnsignedInt {
		return nil, fmt.Errorf("state root version not a uint (got major %d)", maj)
	}
	version := StateTreeVersion(extra)

	actors, err := readCidLink(br)
	if err != nil {
		return nil, fmt.Errorf("reading actors-root link: %w", err)
	}
	info, err := readCidLink(br)
	if err != nil {
		return nil, fmt.Errorf("reading info-root link: %w", err)
	}

	return &StateRoot{Version: version, Actors: actors, Info: info}, nil
}

// readCidLink reads a CBOR tag-42 CID link as encoded by Filecoin's DAG-CBOR
// codec: tag(42, byte-string{0x00, <multihash-prefixed CID bytes>}).
func readCidLink(r io.Reader) (cid.Cid, error) {
	maj, extra, err := cbg.CborReadHeader(r)
	if err != nil {
		return cid.Undef, err
	}
	if maj != cbg.MajTag || extra != 42 {
		return cid.Undef, fmt.Errorf("not a CID tag (got major %d, extra %d)", maj, extra)
	}
	maj, l, err := cbg.CborReadHeader(r)
	if err != nil {
		return cid.Undef, err
	}
	if maj != cbg.MajByteString {
		return cid.Undef, fmt.Errorf("CID payload not byte-string (got major %d)", maj)
	}
	buf := make([]byte, l)
	if _, err := io.ReadFull(r, buf); err != nil {
		return cid.Undef, fmt.Errorf("reading CID bytes: %w", err)
	}
	if len(buf) < 1 || buf[0] != 0 {
		return cid.Undef, fmt.Errorf("CID payload missing leading 0x00")
	}
	c, err := cid.Cast(buf[1:])
	if err != nil {
		return cid.Undef, fmt.Errorf("parsing CID: %w", err)
	}
	return c, nil
}
