package handlers

// Local eth_getCode (lantern#74). Stock Curio's PDP-only build (maxboom,
// filecoin-project/curio#1311) embeds Lantern over plain JSON-RPC with no
// VMBridge, so eth_getCode must resolve from local state rather than
// forwarding. Every ethclient.CodeAt (contract-presence checks before
// calling PDPVerifier / FWSS / ServiceProviderRegistry / USDFC) depends
// on this working bridge-off.
//
// Resolution mirrors localEthCall: ETH addr -> Filecoin addr -> live-head
// actor -> if it's an EVM actor, fetch + verify its bytecode. Actors whose
// code CID is KNOWN to be a non-EVM kind (accounts/placeholders/EOAs/miners)
// and unknown addresses return "0x" (no code), which is exactly what
// eth_getCode reports for them. An actor whose code CID is not in the
// bundled actors table (a network upgrade this build predates) is NOT
// definitive: it logs a one-time WARN naming the code CID and returns
// served=false. Any other resolution fault also returns served=false so the
// caller falls back to the bridge, never a wrong answer.

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/actors"
)

// localEthGetCode returns ("0x"+hexbytecode, true, nil) on a clean local
// resolution (including "0x" for addresses with no contract code), or
// ("", false, nil) when the address can't be resolved locally and the
// caller should fall back to the VMBridge.
func (c *ChainAPI) localEthGetCode(ctx context.Context, addrHex string) (string, bool, error) {
	if c.Accessor == nil || c.BlockGetter == nil {
		return "", false, nil // can't serve locally
	}
	raw, err := decodeEthAddr(addrHex)
	if err != nil {
		return "", false, nil // malformed -> let the bridge try
	}
	filAddr, err := ethAddrToFilecoin(raw)
	if err != nil {
		return "", false, nil
	}

	// Anchor at the LIVE head like the eth_call path, with the same
	// bounded retry layer for cold storage-trie blocks under load.
	acc := c.accForReads()
	bg := newRetryingBlockGetter(c.BlockGetter, 2, 8*time.Second)

	actor, _, err := acc.GetActor(ctx, filAddr)
	if err != nil {
		// Address not found in state == account with no code. eth_getCode
		// reports "0x" for unknown/EOA addresses; that's a definitive,
		// correct local answer.
		if errors.Is(err, accessor.ErrAddressNotFound) {
			return "0x", true, nil
		}
		// Any other read fault (cold block, transient) is NOT definitive;
		// fall back to the bridge.
		return "", false, nil
	}

	st, err := actors.LoadEVM(ctx, actor.Code, actor.Head, bg, acc.Registry())
	if err != nil {
		if isDefinitelyNotEVM(err, actor.Code, "eth_getCode") {
			// Known non-EVM actor (account / placeholder / miner / etc.)
			// -> no contract bytecode. eth_getCode == "0x". Definitive.
			return "0x", true, nil
		}
		// Unknown code CID, unsupported actors version or a head
		// fetch/decode fault: we cannot prove there is no code. Fall back.
		return "", false, nil
	}

	code, err := actors.FetchBytecode(ctx, st, bg)
	if err != nil {
		// We know it's an EVM contract but couldn't fetch/verify the
		// bytecode block (cold/missing). Don't answer "0x" (that would be
		// wrong for a real contract); fall back to the bridge.
		return "", false, nil
	}
	return "0x" + hex.EncodeToString(code), true, nil
}

// isDefinitelyNotEVM classifies a LoadEVM error. It returns true only when
// the actor's code CID is known and resolves to a non-EVM kind, the one
// case where "no contract here" is a definitive answer. An unknown code CID
// (typically a builtin-actors bundle from a network upgrade this build does
// not carry) logs a one-time WARN naming the CID and returns false, as do
// unsupported versions and fetch/decode faults.
func isDefinitelyNotEVM(err error, code cid.Cid, where string) bool {
	var wk actors.ErrWrongKind
	if errors.As(err, &wk) {
		return true
	}
	var uc actors.ErrUnknownCode
	if errors.As(err, &uc) {
		actors.WarnUnknownCodeOnce(code, where)
	}
	return false
}
