// HTTP gateway client. Satisfies state/hamt.BlockGetter.

package hsync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/hamt"
)

// Client fetches IPLD blocks from one or more Lantern gateways. Multiple
// gateways are tried sequentially; the first successful CID-matching
// response wins.
type Client struct {
	endpoints []string
	hc        *http.Client
}

// HTTPClient lets callers swap in a custom *http.Client (e.g. with a custom
// dialer for DNS workarounds). Optional; default is set in NewClient.
func (c *Client) SetHTTPClient(hc *http.Client) { c.hc = hc }

// NewClient configures a Client. `endpoints` is a list of gateway base URLs
// (no trailing slash); the canonical Lantern endpoint is
// `https://gateway.lantern.reiers.io`. `timeout` is per-request.
func NewClient(endpoints []string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	if len(endpoints) == 0 {
		endpoints = []string{"https://gateway.lantern.reiers.io"}
	}
	return &Client{
		endpoints: endpoints,
		hc:        &http.Client{Timeout: timeout},
	}
}

// Get fetches the block bytes for `c` from the first gateway that returns
// a 200 with a CID-matching body. Returns ErrNotFound if every gateway
// returns 404; other errors are wrapped and returned from the first failure.
func (c *Client) Get(ctx context.Context, k cid.Cid) ([]byte, error) {
	var firstErr error
	for _, ep := range c.endpoints {
		url := fmt.Sprintf("%s/block/%s", strings.TrimRight(ep, "/"), k.String())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		req.Header.Set("Accept", "application/vnd.ipld.raw")
		resp, err := c.hc.Do(req)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", ep, err)
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			if firstErr == nil {
				firstErr = ErrNotFound
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: HTTP %d: %s", ep, resp.StatusCode, truncate(body, 200))
			}
			continue
		}
		// Defensive CID verification.
		if err := hamt.VerifyBlockCID(k, body); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", ep, err)
			}
			continue
		}
		return body, nil
	}
	if firstErr == nil {
		firstErr = ErrNotFound
	}
	return nil, firstErr
}

// ErrNotFound is returned when no gateway has the requested CID.
var ErrNotFound = fmt.Errorf("block not found in any gateway")

// StateHead is the JSON returned by GET /state/root.
type StateHead struct {
	Epoch        int64    `json:"epoch"`
	TipsetKey    []string `json:"tipsetKey"`
	StateRoot    string   `json:"stateRoot"`
	ParentWeight string   `json:"parentWeight"`
}

// GetStateHead probes /state/root on the first gateway. Useful for cold-
// start: Lantern needs a starting state-root CID before any other fetch.
func (c *Client) GetStateHead(ctx context.Context) (*StateHead, error) {
	if len(c.endpoints) == 0 {
		return nil, fmt.Errorf("no gateway endpoint configured")
	}
	url := strings.TrimRight(c.endpoints[0], "/") + "/state/root"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}
	var out StateHead
	if err := decodeJSON(resp.Body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
