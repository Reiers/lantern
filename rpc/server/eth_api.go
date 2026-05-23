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
// Implemented (eth_* + Filecoin.Eth*):
//   eth_blockNumber          mirrors Filecoin chain epoch
//   eth_chainId              314 mainnet / 314159 calibration
//   eth_accounts             []
//   eth_maxPriorityFeePerGas "0x0" (Filecoin has no tip)
//   eth_gasPrice             MinimumBaseFee (TODO: live base-fee)
//   eth_syncing              false (light client always anchored)
//   eth_getBalance           state.Accessor.GetActor + balance lookup
//   eth_getBlockByNumber     HeaderStore.GetTipSetByHeight + ETH reshape
//
// Implemented via VMBridge forwarding (lantern#30):
//   eth_call                  forwarded to upstream Forest/Lotus for FEVM execution
//   eth_estimateGas           forwarded to upstream Forest/Lotus
//   eth_sendRawTransaction    forwarded to upstream Forest/Lotus mempool
//
// Not implemented yet:
//   eth_getTransactionCount   medium: state nonce lookup (could be local)
//   eth_getTransactionByHash  hard: needs message indexing or VMBridge forward
//   eth_getTransactionReceipt hard: needs receipt indexing or VMBridge forward
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

// EthSyncing returns false; Lantern is always anchored to its trust root.
func (e *ethAPI) EthSyncing(ctx context.Context) (any, error) {
	return e.full.EthSyncing(ctx)
}

// EthGetBalance returns the balance of an Ethereum-shaped address.
func (e *ethAPI) EthGetBalance(ctx context.Context, addr string, blockParam any) (string, error) {
	return e.full.EthGetBalance(ctx, addr, blockParam)
}

// EthGetBlockByNumber returns an ETH-shaped block for the given epoch.
func (e *ethAPI) EthGetBlockByNumber(ctx context.Context, blockParam string, fullTx bool) (any, error) {
	return e.full.EthGetBlockByNumber(ctx, blockParam, fullTx)
}

// EthCall forwards to the VMBridge for FEVM execution. Returns an
// error pointing at --vm-bridge-rpc when no bridge is configured.
func (e *ethAPI) EthCall(ctx context.Context, callObj any, blockParam any) (string, error) {
	return e.full.EthCall(ctx, callObj, blockParam)
}

// EthEstimateGas forwards to the VMBridge for FEVM execution.
func (e *ethAPI) EthEstimateGas(ctx context.Context, callObj any) (string, error) {
	return e.full.EthEstimateGas(ctx, callObj)
}

// EthSendRawTransaction forwards a signed raw tx to the VMBridge
// upstream for mempool admission.
func (e *ethAPI) EthSendRawTransaction(ctx context.Context, signedTxHex string) (string, error) {
	return e.full.EthSendRawTransaction(ctx, signedTxHex)
}

// EthGetTransactionCount returns the nonce for an Ethereum address.
// Forwarded to the VMBridge (state-tree backed nonce lookup; Lantern's
// local state doesn't currently include FEVM account nonces).
func (e *ethAPI) EthGetTransactionCount(ctx context.Context, addr string, blockParam any) (string, error) {
	return e.full.EthGetTransactionCount(ctx, addr, blockParam)
}

// EthGetTransactionReceipt returns the receipt for a previously-broadcast
// transaction. Forwarded to the VMBridge.
func (e *ethAPI) EthGetTransactionReceipt(ctx context.Context, txHash string) (any, error) {
	return e.full.EthGetTransactionReceipt(ctx, txHash)
}

// EthFeeHistory returns historical gas-fee data for tx builders.
// Forwarded to the VMBridge.
func (e *ethAPI) EthFeeHistory(ctx context.Context, blockCount string, newestBlock string, rewardPercentiles []float64) (any, error) {
	return e.full.EthFeeHistory(ctx, blockCount, newestBlock, rewardPercentiles)
}
