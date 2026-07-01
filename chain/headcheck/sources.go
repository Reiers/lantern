package headcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	abi "github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/bootstrap"
)

// RPCHeadSource is a HeadSource backed by a Lotus-compatible JSON-RPC
// endpoint's Filecoin.ChainHead. It is used ONLY to corroborate or
// dispute the head Lantern already derived from gossip (see package
// doc); it is never the source of truth for the head.
//
// Kind defaults to bootstrap.KindForest for an operator-supplied node;
// pass bootstrap.KindLanternGateway / KindUser explicitly to tag the
// project gateway or a user --peer so diversity counting is honest.
type RPCHeadSource struct {
	name    string
	kind    bootstrap.Kind
	url     string
	token   string
	timeout time.Duration
	client  *http.Client
}

// NewRPCHeadSource builds an RPC-backed head source. Empty name derives
// one from the URL; zero timeout defaults to 15s.
func NewRPCHeadSource(name string, kind bootstrap.Kind, url, token string, timeout time.Duration) *RPCHeadSource {
	if name == "" {
		name = "rpc:" + url
	}
	if kind == "" {
		kind = bootstrap.KindForest
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &RPCHeadSource{
		name: name, kind: kind, url: url, token: token, timeout: timeout,
		client: &http.Client{Timeout: timeout},
	}
}

func (s *RPCHeadSource) Name() string         { return s.name }
func (s *RPCHeadSource) Kind() bootstrap.Kind { return s.kind }

// GatewayHeadSource is a HeadSource backed by the Lantern gateway's HTTP
// /state/root endpoint (net/hsync shape), which returns an Epoch field.
// The gateway speaks HTTP, not Filecoin.ChainHead JSON-RPC, so it needs
// its own adapter. Tagged KindLanternGateway so diversity counting treats
// it as its own kind (and, per bootstrap policy, does not let the project
// gateway alone satisfy a quorum). Purely corroborative, never the head
// oracle - same contract as RPCHeadSource.
type GatewayHeadSource struct {
	name    string
	url     string // gateway base URL
	timeout time.Duration
	client  *http.Client
}

// NewGatewayHeadSource builds a gateway-backed head source. url is the
// gateway base (e.g. https://gateway.golantern.io); zero timeout => 15s.
func NewGatewayHeadSource(url string, timeout time.Duration) *GatewayHeadSource {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &GatewayHeadSource{
		name: "gateway:" + url, url: strings.TrimRight(url, "/"), timeout: timeout,
		client: &http.Client{Timeout: timeout},
	}
}

func (s *GatewayHeadSource) Name() string         { return s.name }
func (s *GatewayHeadSource) Kind() bootstrap.Kind { return bootstrap.KindLanternGateway }

func (s *GatewayHeadSource) HeadEpoch(ctx context.Context) (abi.ChainEpoch, error) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", s.url+"/state/root", nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("headcheck gateway %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("headcheck gateway %s: HTTP %d", s.url, resp.StatusCode)
	}
	var head struct {
		Epoch abi.ChainEpoch `json:"Epoch"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return 0, fmt.Errorf("headcheck gateway %s: decode: %w", s.url, err)
	}
	return head.Epoch, nil
}

func (s *RPCHeadSource) HeadEpoch(ctx context.Context) (abi.ChainEpoch, error) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	reqBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "Filecoin.ChainHead",
		"params":  []any{},
	})
	req, err := http.NewRequestWithContext(cctx, "POST", s.url, bytes.NewReader(reqBody))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("headcheck rpc %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("headcheck rpc %s: HTTP %d", s.url, resp.StatusCode)
	}
	var env struct {
		Result *struct {
			Height abi.ChainEpoch `json:"Height"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("headcheck rpc %s: decode: %w", s.url, err)
	}
	if env.Error != nil {
		return 0, fmt.Errorf("headcheck rpc %s: %s", s.url, env.Error.Message)
	}
	if env.Result == nil {
		return 0, fmt.Errorf("headcheck rpc %s: nil head", s.url)
	}
	return env.Result.Height, nil
}
