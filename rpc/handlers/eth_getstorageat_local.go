package handlers

// Local eth_getStorageAt (lantern#75). Same motivation as #74: a
// bridge-off node (stock Curio / maxboom, filecoin-project/curio#1311)
// embeds Lantern over plain JSON-RPC and must read contract storage
// slots from local state instead of forwarding to a VMBridge.
//
// Resolution mirrors evmBackend.GetStorage: ETH addr -> EVM actor ->
// KAMT<U256,U256> storage root -> slot value. Unknown address / non-EVM
// actor / missing slot all read as 0x0...0 (the EVM convention for an
// unset slot). Any read fault returns served=false so the caller falls
// back to the bridge rather than returning a wrong zero.

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/holiman/uint256"

	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/actors"
	"github.com/Reiers/lantern/state/kamt"
)

const zeroSlot = "0x0000000000000000000000000000000000000000000000000000000000000000"

// localEthGetStorageAt returns (32-byte-hex value, true, nil) on a clean
// local resolution, or ("", false, nil) to fall back to the bridge.
func (c *ChainAPI) localEthGetStorageAt(ctx context.Context, addrHex, keyHex string) (string, bool, error) {
	if c.Accessor == nil || c.BlockGetter == nil {
		return "", false, nil
	}
	raw, err := decodeEthAddr(addrHex)
	if err != nil {
		return "", false, nil
	}
	filAddr, err := ethAddrToFilecoin(raw)
	if err != nil {
		return "", false, nil
	}
	key, err := decodeStorageKey(keyHex)
	if err != nil {
		return "", false, nil // malformed slot key -> let the bridge try
	}

	acc := c.accForReads()
	bg := newRetryingBlockGetter(c.BlockGetter, 2, 8*time.Second)

	actor, _, err := acc.GetActor(ctx, filAddr)
	if err != nil {
		if errors.Is(err, accessor.ErrAddressNotFound) {
			return zeroSlot, true, nil // no actor -> unset slot
		}
		return "", false, nil
	}
	st, err := actors.LoadEVM(ctx, actor.Code, actor.Head, bg, actors.DefaultRegistry())
	if err != nil {
		return zeroSlot, true, nil // not an EVM contract -> no storage
	}

	v, _, err := kamt.GetU256(ctx, st.StorageRoot(), key.ToBig(), bg)
	if err != nil {
		// A missing slot reads as 0 in kamt.GetU256 (returns zero, no
		// error). A real error here is a cold/missing trie block -> not
		// definitive, fall back to the bridge.
		return "", false, nil
	}
	var out uint256.Int
	out.SetFromBig(v)
	b := out.Bytes32()
	return "0x" + hex.EncodeToString(b[:]), true, nil
}

// decodeStorageKey parses a 0x-hex storage slot key into a uint256.
func decodeStorageKey(s string) (uint256.Int, error) {
	h := s
	if len(h) >= 2 && (h[:2] == "0x" || h[:2] == "0X") {
		h = h[2:]
	}
	var k uint256.Int
	if h == "" {
		return k, nil
	}
	if err := k.SetFromHex("0x" + h); err != nil {
		return uint256.Int{}, err
	}
	return k, nil
}
