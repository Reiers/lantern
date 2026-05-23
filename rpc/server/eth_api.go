// eth_api.go — wrapper struct exposing the Ethereum-namespace (eth_*)
// JSON-RPC methods Lantern serves.
//
// Why a separate wrapper instead of just registering api.FullNode under
// the "eth" namespace too: jsonrpc.RPCServer.Register reflects EVERY
// exported method on the value. Registering FullNode under "eth" would
// expose `eth_chainHead`, `eth_netInfo`, etc. — surface area no real
// client asks for and which obscures the actual EVM-compat layer we're
// promising. The ethAPI struct below explicitly lists only the methods
// that map to the Ethereum JSON-RPC spec.
//
// As Lantern's eth_* coverage grows (issue #26), add the new method
// here and on api.FullNode. The pair drives both the wire surface and
// the type contract.

package server

import (
	"context"

	"github.com/Reiers/lantern/api"
)

// ethAPI implements the subset of api.FullNode that maps to Ethereum
// JSON-RPC. The lanternMethodNameFormatter (see server.go) converts
// each Go method like `EthBlockNumber` to its wire name `eth_blockNumber`.
type ethAPI struct {
	full api.FullNode
}

func newEthAPI(full api.FullNode) *ethAPI {
	return &ethAPI{full: full}
}

// --- Coverage ledger ----------------------------------------------------
//
// Implemented:
//   eth_blockNumber  → mirrors Filecoin chain epoch
//
// Not implemented yet (see Reiers/lantern#26 for the full matrix):
//   eth_chainId               cheap; needs build.Network → 314 / 314159
//   eth_call                  HARD: needs FEVM execution. Forward via
//                             VMBridge to upstream Forest/Lotus.
//   eth_getBalance            implementable: state.Accessor.GetActor + balance
//   eth_gasPrice              implementable: read MinimumBaseFee from chain
//   eth_estimateGas           HARD: needs FEVM. Forward via VMBridge.
//   eth_getTransactionCount   needs state nonce lookup
//   eth_getTransactionReceipt needs message + receipt indexing
//   eth_sendRawTransaction    needs FEVM mempool admission. Forward via
//                             VMBridge (similar to AllowBlockSubmit path).
//   eth_accounts              returns []string{} (clients use their own keystore)
//   eth_maxPriorityFeePerGas  returns "0x0" (Filecoin doesn't tip)
// ------------------------------------------------------------------------

// EthBlockNumber returns the current head epoch as a 0x-prefixed hex
// string. Forwarded to the underlying FullNode handler so the
// behaviour stays in one place.
func (e *ethAPI) EthBlockNumber(ctx context.Context) (string, error) {
	return e.full.EthBlockNumber(ctx)
}

// EthChainId returns the EIP-155 chain ID for the active network.
func (e *ethAPI) EthChainId(ctx context.Context) (string, error) {
	return e.full.EthChainId(ctx)
}

// EthAccounts returns []. See FullNode docstring.
func (e *ethAPI) EthAccounts(ctx context.Context) ([]string, error) {
	return e.full.EthAccounts(ctx)
}

// EthMaxPriorityFeePerGas returns "0x0" (Filecoin has no tip concept).
func (e *ethAPI) EthMaxPriorityFeePerGas(ctx context.Context) (string, error) {
	return e.full.EthMaxPriorityFeePerGas(ctx)
}

// EthGasPrice returns the protocol-minimum base fee as the compat
// gas-price quote.
func (e *ethAPI) EthGasPrice(ctx context.Context) (string, error) {
	return e.full.EthGasPrice(ctx)
}
