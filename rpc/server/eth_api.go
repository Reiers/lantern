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
	"encoding/hex"
	"fmt"
	stdbig "math/big"
	"runtime"

	"golang.org/x/crypto/sha3"

	"github.com/filecoin-project/go-jsonrpc"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/internal/buildinfo"
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

// netAPI implements the small "net_*" namespace. Go-ethereum's
// ethclient.NetworkID() calls net_version (returns the chain ID as a
// decimal string). Adding the entire namespace as a sibling to eth_*
// keeps the wire shape predictable.
type netAPI struct {
	full api.FullNode
}

func newNetAPI(full api.FullNode) *netAPI {
	return &netAPI{full: full}
}

// NetVersion returns the decimal chain ID. Matches eth_chainId's
// integer value but in decimal string form, which is what net_version
// returns on every Ethereum-compat node.
func (n *netAPI) NetVersion(ctx context.Context) (string, error) {
	chainHex, err := n.full.EthChainId(ctx)
	if err != nil {
		return "", err
	}
	// Convert 0x-hex chain id to decimal string.
	s := chainHex
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = s[2:]
	}
	v, ok := new(stdbig.Int).SetString(s, 16)
	if !ok {
		return "", fmt.Errorf("netAPI: bad chain id hex %q", chainHex)
	}
	return v.String(), nil
}

// EthGetTransactionByHash forwards to the VMBridge.
func (e *ethAPI) EthGetTransactionByHash(ctx context.Context, txHash string) (any, error) {
	return e.full.EthGetTransactionByHash(ctx, txHash)
}

// web3API implements the small "web3_*" namespace. Wallets and dapps
// call web3_clientVersion to identify what node they're talking to;
// without it the wallet treats us as an unknown / unsupported node.
// EIP-1474 lists this as the canonical client-introspection method.
type web3API struct {
	networkName string
}

func newWeb3API(networkName string) *web3API {
	return &web3API{networkName: networkName}
}

// ClientVersion returns the wallet-facing client identifier. Matches
// go-ethereum's format "<software>/<version>/<os>-<arch>/<lang>" but
// with our own software/version so wallets logging the field can
// distinguish a Lantern light client from a full Lotus.
func (w *web3API) ClientVersion(_ context.Context) (string, error) {
	net := w.networkName
	if net == "" {
		net = "unknown"
	}
	return fmt.Sprintf("Lantern/%s/%s/go-%s",
		buildinfo.BuildVersion(), net, runtime.Version()[2:]), nil
}

// Sha3 returns keccak256 of the input bytes. Some libraries call this
// during chain-id discovery flows.
func (w *web3API) Sha3(_ context.Context, input string) (string, error) {
	raw := input
	if len(raw) >= 2 && (raw[:2] == "0x" || raw[:2] == "0X") {
		raw = raw[2:]
	}
	data, err := hex.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("web3_sha3: invalid hex input: %w", err)
	}
	h := keccak256(data)
	return "0x" + hex.EncodeToString(h), nil
}

// keccak256 is the small helper for web3_sha3. We avoid pulling in
// go-ethereum's crypto package here so the rpc/server package stays
// thin; this is one of two places in the codebase that needs the
// hash (the other is chain/beacon which has its own blake2b helper).
func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// EthGetTransactionByBlockNumberAndIndex forwards to the VMBridge.
func (e *ethAPI) EthGetTransactionByBlockNumberAndIndex(ctx context.Context, blockParam string, index string) (any, error) {
	return e.full.EthGetTransactionByBlockNumberAndIndex(ctx, blockParam, index)
}

// EthGetCode forwards to the VMBridge.
func (e *ethAPI) EthGetCode(ctx context.Context, addr string, blockParam any) (string, error) {
	return e.full.EthGetCode(ctx, addr, blockParam)
}

// EthGetStorageAt forwards to the VMBridge.
func (e *ethAPI) EthGetStorageAt(ctx context.Context, addr string, key string, blockParam any) (string, error) {
	return e.full.EthGetStorageAt(ctx, addr, key, blockParam)
}

// EthGetBlockByHash forwards to the VMBridge.
func (e *ethAPI) EthGetBlockByHash(ctx context.Context, blockHash string, fullTx bool) (any, error) {
	return e.full.EthGetBlockByHash(ctx, blockHash, fullTx)
}

// EthGetLogs forwards to the VMBridge. Heavily used by FilecoinPay rail
// event watchers and client-side payment monitoring.
func (e *ethAPI) EthGetLogs(ctx context.Context, filter any) (any, error) {
	return e.full.EthGetLogs(ctx, filter)
}

// EthSubscribe (EIP-1193) registers an event subscription on the WS
// connection. V1 supports event type "newHeads". Returns the
// subscription ID; events flow back as eth_subscription notifications.
func (e *ethAPI) EthSubscribe(ctx context.Context, params jsonrpc.RawParams) (string, error) {
	return e.full.EthSubscribe(ctx, params)
}

// EthUnsubscribe cancels a subscription. Returns true if found.
func (e *ethAPI) EthUnsubscribe(ctx context.Context, id string) (bool, error) {
	return e.full.EthUnsubscribe(ctx, id)
}
