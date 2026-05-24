// ethshape.go — ETH-style reshape helpers for Filecoin types.
//
// Lantern's eth_getBlockByNumber, eth_getTransactionByHash, etc.
// translate Filecoin tipsets / blocks / messages into ETH-shaped JSON
// blobs that strict ETH client parsers (e.g. go-ethereum's
// types.Header) accept. The catch is that the Filecoin types don't
// have a direct ETH form: a `Miner` is an `f0...` actor address, a
// block `Cid` is a multihash CID, etc. We mirror the conversions
// lotus performs in chain/types/ethtypes (EthAddressFromActorID +
// EthHashFromCid) without taking a lotus dependency.
//
// Reference: github.com/filecoin-project/lotus/chain/types/ethtypes/eth_types.go
//   - EthAddressFromActorID: 0xff || zero(11) || be64(actorID)
//   - EthHashFromCid: strip the canonical 6-byte DagCBOR/Blake2b-256
//     prefix and return the 32-byte multihash digest as the ETH hash

package handlers

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// expectedHashPrefix matches lotus chain/types/ethtypes:
//
//	cid.Prefix{Version:1, Codec:DagCBOR, MhType:BLAKE2B_MIN+31, MhLength:32}.Bytes()
//
// We rebuild it once at init rather than hard-coding the bytes so the
// answer stays correct if multihash codec numbers shift.
var expectedCidHashPrefix = cid.Prefix{
	Version:  1,
	Codec:    cid.DagCBOR,
	MhType:   uint64(mh.BLAKE2B_MIN + 31),
	MhLength: 32,
}.Bytes()

// EthAddressFromFilecoinIDActor returns the 0x-prefixed ETH address
// for a Filecoin ID actor (e.g. `f0143103`). For non-ID protocols we
// return a zero ETH address; the only call site today is the miner
// field on a block, which is always an ID address.
//
// Layout: 0xff || zero(11) || be64(actorID)
func EthAddressFromFilecoinIDActor(addr address.Address) string {
	if addr.Protocol() != address.ID {
		return "0x0000000000000000000000000000000000000000"
	}
	id, err := address.IDFromAddress(addr)
	if err != nil {
		return "0x0000000000000000000000000000000000000000"
	}
	var out [20]byte
	out[0] = 0xff
	binary.BigEndian.PutUint64(out[12:], id)
	return "0x" + hex.EncodeToString(out[:])
}

// EthHashFromCid returns the 0x-prefixed 32-byte ETH-shaped hash for
// a DagCBOR+Blake2b-256 CID (the canonical Filecoin block + message
// CID shape). If the CID has a different codec or hash, we return a
// zero 32-byte hash. Strict ETH parsers (e.g. go-ethereum's
// types.Header.Hash) require a 32-byte hex value, so this is the
// safest fallback when we can't decode.
func EthHashFromCid(c cid.Cid) string {
	if c == cid.Undef {
		return "0x0000000000000000000000000000000000000000000000000000000000000000"
	}
	raw := c.Bytes()
	hash, found := bytes.CutPrefix(raw, expectedCidHashPrefix)
	if !found || len(hash) != 32 {
		// Fall back to taking the last 32 bytes of the CID's multihash
		// digest if the prefix doesn't match. This covers CID shapes
		// we don't recognise; the result isn't guaranteed to be
		// uniquely-keyed across codecs but it's deterministic and
		// fixed-length, which is all the ETH parser cares about.
		decoded, err := mh.Decode(c.Hash())
		if err != nil || len(decoded.Digest) != 32 {
			return "0x0000000000000000000000000000000000000000000000000000000000000000"
		}
		hash = decoded.Digest
	}
	return "0x" + hex.EncodeToString(hash)
}

// firstCidHash returns the ETH-shaped hash of the first CID in a slice,
// or the zero hash for an empty slice.
func firstCidHash(cids []cid.Cid) string {
	if len(cids) == 0 {
		return "0x0000000000000000000000000000000000000000000000000000000000000000"
	}
	return EthHashFromCid(cids[0])
}

// mustEthAddressStr is a tiny helper so callers can panic-with-context
// during init / development; we don't use it in the live path because
// EthAddressFromFilecoinIDActor already has a zero-address fallback.
func mustEthAddressStr(addr address.Address) string {
	out := EthAddressFromFilecoinIDActor(addr)
	if out == "0x0000000000000000000000000000000000000000" && addr.Protocol() == address.ID {
		panic(fmt.Sprintf("mustEthAddressStr: failed to convert ID actor %s", addr))
	}
	return out
}
