// Embedded F3 trust anchors. The build pulls in the JSON files captured by
// `lantern-f3-anchor` so the binary ships self-contained with a recent power
// table per supported network.

package anchor

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed anchor_mainnet.json
var anchorMainnet []byte

// Calibnet anchor is added in a follow-up once Forest on lex has calibnet
// support. For now, calibnet operators must run `lantern-f3-anchor` themselves.

// Embedded returns the embedded anchor for `network` ("mainnet").
// Returns an error if the network is unknown or no anchor is embedded.
func Embedded(network string) (*Anchor, error) {
	var raw []byte
	switch network {
	case "mainnet":
		raw = anchorMainnet
	default:
		return nil, fmt.Errorf("no embedded anchor for network %q", network)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("embedded anchor for %q is empty", network)
	}
	var a Anchor
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("decode embedded anchor %q: %w", network, err)
	}
	return &a, nil
}
