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
// response wins. Each endpoint is retried up to `retries` times with
// exponential backoff on transient errors (DNS, TLS handshake, connection
// reset, 5xx, 429).
type Client struct {
	endpoints []string
	hc        *http.Client
	retries   int
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
		retries:   2, // 1 initial + 2 retries per endpoint = 3 attempts
	}
}

// SetRetries overrides the per-endpoint retry budget.
func (c *Client) SetRetries(n int) {
	if n < 0 {
		n = 0
	}
	c.retries = n
}

// isTransient returns true for errors worth retrying with backoff.
// TLS handshake errors, DNS resolution flakes, connection resets, and
// context-cancellation-via-deadline-timeout are all transient. 4xx is not.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, needle := range []string{
		"tls: handshake failure",
		"tls: client requested unsupported",
		"connection reset",
		"connection refused",
		"no such host",
		"i/o timeout",
		"EOF",
		"broken pipe",
		"unexpected EOF",
		"network is unreachable",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
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

		// Per-endpoint retry loop with exponential backoff on transient errors.
		var resp *http.Response
		var body []byte
		var attemptErr error
		backoff := 100 * time.Millisecond
		for attempt := 0; attempt <= c.retries; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				backoff *= 2
				// Clone request because Body is single-use (nil here but cheap to clone).
				req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				req2.Header = req.Header.Clone()
				req = req2
			}
			resp, attemptErr = c.hc.Do(req)
			if attemptErr == nil && resp.StatusCode < 500 && resp.StatusCode != 429 {
				break // success or definitive 4xx, no retry
			}
			if attemptErr != nil && !isTransient(attemptErr) {
				break // permanent error, give up on this endpoint
			}
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
		if attemptErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", ep, attemptErr)
			}
			continue
		}
		body, _ = io.ReadAll(resp.Body)
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
