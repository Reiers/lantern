package handlers

// eth_getLogs filter parsing + range helpers (lantern#73).

import (
	"strings"

	abi "github.com/filecoin-project/go-state-types/abi"
)

// parseEthLogFilter normalizes the JSON filter object into ethLogFilter.
// Returns ok=false for a shape we can't interpret (caller falls back to
// the bridge rather than returning wrong results).
func parseEthLogFilter(raw any) (ethLogFilter, bool) {
	f := ethLogFilter{addresses: map[string]bool{}}
	m, ok := raw.(map[string]any)
	if !ok {
		return f, false
	}

	if v, ok := m["fromBlock"].(string); ok {
		f.fromBlock = v
	}
	if v, ok := m["toBlock"].(string); ok {
		f.toBlock = v
	}
	if v, ok := m["blockHash"].(string); ok {
		f.blockHash = v
	}

	// address: string or []string.
	switch a := m["address"].(type) {
	case string:
		if a != "" {
			f.addresses[strings.ToLower(a)] = true
		}
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s != "" {
				f.addresses[strings.ToLower(s)] = true
			}
		}
	}

	// topics: array; each position is null (wildcard), a string, or
	// []string (OR-set).
	if topics, ok := m["topics"].([]any); ok {
		f.topics = make([]map[string]bool, len(topics))
		for i, t := range topics {
			switch tv := t.(type) {
			case nil:
				f.topics[i] = nil // wildcard
			case string:
				f.topics[i] = map[string]bool{strings.ToLower(tv): true}
			case []any:
				set := map[string]bool{}
				for _, x := range tv {
					if s, ok := x.(string); ok {
						set[strings.ToLower(s)] = true
					}
				}
				f.topics[i] = set
			default:
				f.topics[i] = nil
			}
		}
	}
	return f, true
}

// resolveEpochParam maps an eth block tag / 0x-hex number to an epoch.
// Unknown/empty falls back to `def`.
func resolveEpochParam(param string, head abi.ChainEpoch, def abi.ChainEpoch) abi.ChainEpoch {
	switch strings.ToLower(strings.TrimSpace(param)) {
	case "", "latest", "pending", "safe", "finalized":
		if param == "" {
			return def
		}
		return head
	case "earliest":
		return 0
	default:
		h := param
		if len(h) >= 2 && (h[:2] == "0x" || h[:2] == "0X") {
			h = h[2:]
		}
		var n int64
		got := false
		for _, ch := range h {
			var d int64
			switch {
			case ch >= '0' && ch <= '9':
				d = int64(ch - '0')
			case ch >= 'a' && ch <= 'f':
				d = int64(ch-'a') + 10
			case ch >= 'A' && ch <= 'F':
				d = int64(ch-'A') + 10
			default:
				return def
			}
			n = n*16 + d
			got = true
		}
		if !got {
			return def
		}
		return abi.ChainEpoch(n)
	}
}

// heightForBlockHash resolves an eth block hash to its height by scanning
// the recent local window (reuses the eth_getBlockByHash window). Returns
// (height, true) when found, (0, false) to fall back to the bridge.
func (c *ChainAPI) heightForBlockHash(blockHash string, head abi.ChainEpoch) (abi.ChainEpoch, bool) {
	want := normalizeHashHex(blockHash)
	if want == "" || c.HeaderStore == nil {
		return 0, false
	}
	low := head - localBlockByHashWindow
	if low < 0 {
		low = 0
	}
	for ep := head; ep >= low; ep-- {
		ts, err := c.HeaderStore.GetTipSetByHeight(ep)
		if err == nil && ts != nil {
			for _, b := range ts.Blocks() {
				if normalizeHashHex(EthHashFromCid(b.Cid())) == want {
					return ep, true
				}
			}
		}
		if ep == 0 {
			break
		}
	}
	return 0, false
}
