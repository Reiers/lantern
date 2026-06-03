// eth_subscribe.go - EIP-1193 subscription support on the WebSocket
// transport. Closes Lantern#32. Supports the two event types that cover
// ~95% of wallet/dapp usage: "newHeads" and "logs".
// "newPendingTransactions" + "syncing" remain deferred (low usage; the
// former needs a mempool watcher Lantern doesn't index).
//
// "logs" is bridge-backed: Lantern has no local event/receipt index, so
// on each new head we query the VM bridge's eth_getLogs scoped to that
// block with the caller's filter and push each matching log. See
// subscribeLogs.
//
// Why this matters for the V0.1 turnkey-product vision (curio-core#60):
// browser wallets (MetaMask, Brave, WalletConnect dapps) expect
// eth_subscribe at startup. Without it the wallet falls back to
// polling eth_blockNumber + eth_getBalance on a 4-12s timer, adding
// load + UI latency. Some wallets refuse to attach at all if
// eth_subscribe returns "method not found".
//
// Wire shape (matches Lotus exactly):
//
//   client -> server  eth_subscribe(["newHeads"])
//   server -> client  {"id":1, "result":"0x<32-byte hex>"}     <- subscription id
//   server -> client  {"method":"eth_subscription",            <- async notif
//                      "params":{"subscription":"0x...",
//                                "result":<eth-shaped block>}}
//   ...
//   client -> server  eth_unsubscribe("0x...")
//   server -> client  {"id":2, "result":true}
//
// Architecture:
//
//   - go-jsonrpc's RPCServer.ServeHTTP auto-upgrades to WebSocket on
//     the "Connection: upgrade" header; no extra mux wiring needed.
//
//   - Server-initiated callbacks ride the same WS connection via the
//     reverse-client mechanism (jsonrpc.ExtractReverseClient). The
//     reverse-client proxy type is EthSubscriberMethods, with a single
//     EthSubscription method that wires to "eth_subscription" on the
//     wire (notify:true means no response expected).
//
//   - Each active subscription gets a goroutine that ranges over a
//     channel from headnotify.Distributor.Subscribe(ctx) and pushes
//     each HeadChange as an EthSubscriptionResponse. ctx is per-
//     subscription so eth_unsubscribe + WS disconnect both clean up
//     by cancelling.
//
//   - Subscription state lives in a per-ChainAPI map keyed by sub id;
//     simple sync.Mutex protects it. Connection identity is not
//     tracked separately - if a WS disconnects, the goroutine notices
//     when the reverse-call returns an error and self-cancels.

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-jsonrpc"
)

var log = logging.Logger("lantern/rpc-handlers")

// EthSubscriberMethods is the proxy-struct type that go-jsonrpc uses
// to construct the reverse-client. The method name + notify tag
// determine the wire shape - "eth_subscription" + notify:true means
// the server sends fire-and-forget JSON-RPC notifications to the
// connected WS client (no response expected).
//
// This type is exported because (a) it's the contract clients depend
// on via WithReverseClient[EthSubscriberMethods] at server setup, and
// (b) tests can mock it.
type EthSubscriberMethods struct {
	EthSubscription func(ctx context.Context, p jsonrpc.RawParams) error `notify:"true" rpc_method:"eth_subscription"`
}

// EthSubscriptionID is a 32-byte hex string with 0x prefix. Matches
// Lotus/Geth shape on the wire. Wallets pass this back to
// eth_unsubscribe. We use the bare string type (not a named type)
// on the api.FullNode interface to avoid pulling handler-specific
// types into the public API surface; the handler uses this alias
// internally for readability.
type EthSubscriptionID = string

// EthSubscriptionResponse is the payload of each eth_subscription
// notification pushed to the client. Result is the event-type-
// specific payload: an ETH-shaped block for newHeads, an EthLog for
// logs, a tx hash for newPendingTransactions, etc.
type EthSubscriptionResponse struct {
	SubscriptionID EthSubscriptionID `json:"subscription"`
	Result         interface{}       `json:"result"`
}

// subscription tracks one active eth_subscribe. Stored in
// ChainAPI.ethSubs by ID.
type subscription struct {
	id     EthSubscriptionID
	cancel context.CancelFunc
}

// ensureEthSubState initialises the per-ChainAPI subscription map.
// Safe to call multiple times; called lazily on the first
// EthSubscribe call so we don't carry the state on cold ChainAPI
// instances that never see a subscription.
func (c *ChainAPI) ensureEthSubState() {
	c.ethSubMu.Lock()
	defer c.ethSubMu.Unlock()
	if c.ethSubs == nil {
		c.ethSubs = make(map[EthSubscriptionID]*subscription)
	}
}

// EthSubscribe registers a new subscription for the given event type.
// Returns the subscription ID on success; the client uses this ID in
// subsequent eth_subscription notifications (to disambiguate when
// multiple subs are active on the same connection) and in
// eth_unsubscribe.
//
// Supports "newHeads" and "logs". "newPendingTransactions" and
// "syncing" return method-supported-but-event-type-not-supported.
func (c *ChainAPI) EthSubscribe(ctx context.Context, params jsonrpc.RawParams) (EthSubscriptionID, error) {
	if c.HeadNotify == nil {
		return "", xerrors.New("eth_subscribe: head distributor not wired; subscriptions unavailable")
	}

	// Pull the reverse client. This works ONLY on WebSocket connections.
	// HTTP requests don't have a path for server-initiated callbacks, so
	// eth_subscribe over plain HTTP returns an explicit error.
	revClient, ok := jsonrpc.ExtractReverseClient[EthSubscriberMethods](ctx)
	if !ok {
		return "", xerrors.New("eth_subscribe: connection doesn't support callbacks; use WebSocket transport (ws://host/rpc/v1)")
	}

	// Parse params: ["newHeads"] or ["newHeads", {filter}].
	// We only use the event type for V1; future filter params get
	// ignored for unsupported event types.
	var rawArgs []json.RawMessage
	if err := json.Unmarshal(params, &rawArgs); err != nil {
		return "", xerrors.Errorf("eth_subscribe: parse params array: %w", err)
	}
	if len(rawArgs) == 0 {
		return "", xerrors.New("eth_subscribe: missing event type argument (e.g. \"newHeads\")")
	}
	var eventType string
	if err := json.Unmarshal(rawArgs[0], &eventType); err != nil {
		return "", xerrors.Errorf("eth_subscribe: parse event type: %w", err)
	}

	switch eventType {
	case "newHeads":
		return c.subscribeNewHeads(ctx, revClient)
	case "logs":
		// Optional second arg is the log filter object
		// ({"address":..., "topics":...}). fromBlock/toBlock are
		// ignored for subscriptions — we scope each query to the new
		// head as it arrives.
		var filter map[string]json.RawMessage
		if len(rawArgs) > 1 {
			if err := json.Unmarshal(rawArgs[1], &filter); err != nil {
				return "", xerrors.Errorf("eth_subscribe(logs): parse filter object: %w", err)
			}
		}
		return c.subscribeLogs(ctx, revClient, filter)
	case "newPendingTransactions":
		return "", xerrors.New("eth_subscribe(newPendingTransactions): not yet supported by Lantern V1")
	case "syncing":
		return "", xerrors.New("eth_subscribe(syncing): not yet supported by Lantern V1")
	default:
		return "", xerrors.Errorf("eth_subscribe: unsupported event type %q (supported: newHeads)", eventType)
	}
}

// subscribeNewHeads starts a goroutine that subscribes to the
// headnotify Distributor and pushes each new head to the client as
// an eth_subscription notification with an ETH-shaped block as the
// Result. The goroutine self-terminates when the subscription ctx
// is cancelled (via eth_unsubscribe) or when the reverse-call
// returns an error (WS disconnect).
func (c *ChainAPI) subscribeNewHeads(parentCtx context.Context, revClient EthSubscriberMethods) (EthSubscriptionID, error) {
	c.ensureEthSubState()

	id, err := newSubscriptionID()
	if err != nil {
		return "", xerrors.Errorf("eth_subscribe: generate sub id: %w", err)
	}

	// Detach the goroutine's lifetime from the request ctx. The request
	// ctx ends when EthSubscribe returns; the subscription needs to
	// outlive that. We create a fresh background-derived ctx that we
	// can cancel from eth_unsubscribe or from the goroutine itself.
	subCtx, cancel := context.WithCancel(context.Background())

	c.ethSubMu.Lock()
	c.ethSubs[id] = &subscription{id: id, cancel: cancel}
	c.ethSubMu.Unlock()

	// Channel from the headnotify Distributor. The Subscribe call
	// returns a channel that gets a "current" event first, then
	// "apply"/"revert" events for each subsequent head change. We
	// skip "current" because new clients want only forward-going
	// events; "current" is for replay scenarios.
	headCh := c.HeadNotify.Subscribe(subCtx)

	go func() {
		defer func() {
			// Cleanup on goroutine exit: remove from map.
			c.ethSubMu.Lock()
			delete(c.ethSubs, id)
			c.ethSubMu.Unlock()
		}()

		for {
			select {
			case <-subCtx.Done():
				return
			case batch, ok := <-headCh:
				if !ok {
					// Distributor closed the channel; we're done.
					return
				}
				for _, hc := range batch {
					// Skip "current" - this is the first event after
					// Subscribe and it's a snapshot, not a new event.
					// Wallets want forward-going notifications only.
					if hc.Type == "current" {
						continue
					}
					// Only push "apply" events. "revert" represents a
					// reorg; geth/lotus also emit those on newHeads
					// for the new chain head, not the reverted one.
					// In our HeadChange shape an "apply" is the new
					// head. revert events are skipped here; clients
					// that care about reorgs should subscribe to
					// "logs" with removed=true (out of V1 scope).
					if hc.Type != "apply" {
						continue
					}
					if hc.Val == nil {
						continue
					}

					// Format the tipset as an ETH-shaped block. We
					// already have this exact conversion path in
					// EthGetBlockByNumber's reshape; both call sites
					// share tipsetToEthBlock so the wire shape stays
					// identical regardless of how the client got
					// the block (poll vs subscribe).
					ethBlock := tipsetToEthBlock(hc.Val)
					if ethBlock == nil {
						log.Warnw("eth_subscribe(newHeads): empty tipset, skipping",
							"subscription_id", id, "height", hc.Val.Height())
						continue
					}

					resp := EthSubscriptionResponse{
						SubscriptionID: id,
						Result:         ethBlock,
					}
					payload, err := json.Marshal(resp)
					if err != nil {
						log.Warnw("eth_subscribe(newHeads): marshal response",
							"subscription_id", id, "err", err)
						continue
					}

					// Push to the client. The notify-tagged method
					// returns nil on success; an error means the WS
					// connection is dead or write failed. Either way
					// we self-cancel.
					if err := revClient.EthSubscription(subCtx, jsonrpc.RawParams(payload)); err != nil {
						log.Infow("eth_subscribe(newHeads): client write failed, terminating subscription",
							"subscription_id", id, "err", err)
						return
					}
				}
			}
		}
	}()

	log.Infow("eth_subscribe(newHeads): subscription created",
		"subscription_id", id)
	return id, nil
}

// subscribeLogs starts a subscription that, on every new canonical head,
// queries eth_getLogs for that block scoped to the caller's filter
// (address + topics) and pushes each matching log to the client as an
// eth_subscription notification.
//
// Design note (issue #32, option a): Lantern has no local event/receipt
// index — EthGetLogs forwards to the VM bridge (Forest/Lotus upstream).
// So rather than scanning tipsets locally, we reuse that bridge path:
// each new head triggers one bridge eth_getLogs call with
// fromBlock=toBlock=<new head>. Latency is one block time; cost is one
// bridge call per active logs-subscription per block. Geth emits one
// notification per matching log, which we mirror.
func (c *ChainAPI) subscribeLogs(parentCtx context.Context, revClient EthSubscriberMethods, filter map[string]json.RawMessage) (EthSubscriptionID, error) {
	if c.Bridge == nil {
		return "", xerrors.New("eth_subscribe(logs): VM bridge not configured; log subscriptions require --vm-bridge-rpc")
	}
	c.ensureEthSubState()

	id, err := newSubscriptionID()
	if err != nil {
		return "", xerrors.Errorf("eth_subscribe: generate sub id: %w", err)
	}

	// Copy the caller's filter, stripping any client-supplied block range
	// — we own fromBlock/toBlock and set them per head.
	baseFilter := map[string]json.RawMessage{}
	for k, v := range filter {
		switch k {
		case "fromBlock", "toBlock", "blockHash":
			// ignored for subscriptions
		default:
			baseFilter[k] = v
		}
	}

	subCtx, cancel := context.WithCancel(context.Background())
	c.ethSubMu.Lock()
	c.ethSubs[id] = &subscription{id: id, cancel: cancel}
	c.ethSubMu.Unlock()

	headCh := c.HeadNotify.Subscribe(subCtx)

	go func() {
		defer func() {
			c.ethSubMu.Lock()
			delete(c.ethSubs, id)
			c.ethSubMu.Unlock()
		}()

		for {
			select {
			case <-subCtx.Done():
				return
			case batch, ok := <-headCh:
				if !ok {
					return
				}
				for _, hc := range batch {
					if hc.Type != "apply" || hc.Val == nil {
						continue
					}
					blockHex := fmt.Sprintf("0x%x", int64(hc.Val.Height()))

					// Build the per-head filter: base + fromBlock=toBlock=head.
					f := map[string]json.RawMessage{}
					for k, v := range baseFilter {
						f[k] = v
					}
					bh, _ := json.Marshal(blockHex)
					f["fromBlock"] = bh
					f["toBlock"] = bh

					logs, err := c.queryLogs(subCtx, f)
					if err != nil {
						// Transient bridge error: log + skip this head,
						// keep the subscription alive for the next one.
						log.Debugw("eth_subscribe(logs): getLogs failed for head",
							"subscription_id", id, "block", blockHex, "err", err)
						continue
					}

					for _, lg := range logs {
						resp := EthSubscriptionResponse{SubscriptionID: id, Result: lg}
						payload, err := json.Marshal(resp)
						if err != nil {
							log.Warnw("eth_subscribe(logs): marshal response",
								"subscription_id", id, "err", err)
							continue
						}
						if err := revClient.EthSubscription(subCtx, jsonrpc.RawParams(payload)); err != nil {
							log.Infow("eth_subscribe(logs): client write failed, terminating subscription",
								"subscription_id", id, "err", err)
							return
						}
					}
				}
			}
		}
	}()

	log.Infow("eth_subscribe(logs): subscription created", "subscription_id", id)
	return id, nil
}

// queryLogs calls the bridge's eth_getLogs with the given filter and
// returns the result as a slice of raw log objects. The bridge returns
// an array of EthLog objects; we keep them opaque (interface{}) and pass
// them through unchanged so the wire shape matches eth_getLogs exactly.
func (c *ChainAPI) queryLogs(ctx context.Context, filter map[string]json.RawMessage) ([]interface{}, error) {
	out, err := c.EthGetLogs(ctx, filter)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	logs, ok := out.([]interface{})
	if !ok {
		// Some upstreams may return null or a non-array on empty; treat
		// anything non-array as "no logs" rather than erroring.
		return nil, nil
	}
	return logs, nil
}

// EthUnsubscribe cancels the subscription with the given ID. Returns
// true if a subscription was found and cancelled, false if no such
// subscription existed on this connection.
//
// Note: we don't currently scope subscriptions per-connection; a
// future hardening pass would track which connection owns which sub
// and reject cross-connection unsubscribe. For V1 this is acceptable
// since the cost of a malicious unsubscribe is only freeing your own
// resources (the next push attempt to a closed conn would clean it
// up anyway).
func (c *ChainAPI) EthUnsubscribe(ctx context.Context, id EthSubscriptionID) (bool, error) {
	c.ethSubMu.Lock()
	sub, ok := c.ethSubs[id]
	if !ok {
		c.ethSubMu.Unlock()
		return false, nil
	}
	delete(c.ethSubs, id)
	c.ethSubMu.Unlock()

	sub.cancel()
	log.Infow("eth_unsubscribe: cancelled", "subscription_id", id)
	return true, nil
}

// newSubscriptionID returns a fresh 32-byte hex ID with 0x prefix.
// Matches the Lotus / Geth wire shape.
func newSubscriptionID() (EthSubscriptionID, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return EthSubscriptionID("0x" + hex.EncodeToString(b[:])), nil
}

// (tipsetToEthBlock lives in extra.go and is shared with EthGetBlockByNumber.)
