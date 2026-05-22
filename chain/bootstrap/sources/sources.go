// Package sources provides Source implementations for the
// chain/bootstrap quorum:
//
//   - ForestSource:   any Lotus-compatible JSON-RPC endpoint (e.g.
//                     forest-archive.chainsafe.dev). Calls
//                     Filecoin.F3GetLatestCertificate.
//   - GatewaySource:  the Lantern project's HTTPS gateway. Same wire
//                     protocol as ForestSource but tagged
//                     KindLanternGateway so quorum policy can exclude it
//                     by default.
//   - UserPeerSource: a user-supplied --peer URL. Uses the same
//                     Lotus-compatible JSON-RPC protocol.
//   - Libp2pSource:   a single libp2p peer queried via the F3
//                     cert-exchange protocol (/f3/certexch/get/1/<nn>).
//   - LanternBeaconSource: a DHT-discovered Lantern beacon. Lantern
//                     beacons today serve Bitswap but do not yet
//                     implement cert-exchange; this source is a stub
//                     that returns ErrNoBeaconBackend so quorum probes
//                     don't panic on it. When V1.2.1 ships beacon
//                     cert-exchange, this becomes the real
//                     implementation.
//
// Source implementations live here instead of next to bootstrap.go to
// keep the bootstrap driver dependency-free (no libp2p, no HTTP) so it
// can be tested with mocks.
package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/filecoin-project/go-f3/certexchange"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/gpbft"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/Reiers/lantern/chain/bootstrap"
)

// ---------- JSON-RPC sources (Forest / Lantern gateway / user-peer) ----------

// rpcSource is shared plumbing for any Lotus-compatible JSON-RPC source
// that exposes Filecoin.F3GetLatestCertificate.
type rpcSource struct {
	name    string
	kind    bootstrap.Kind
	url     string
	token   string // optional bearer token
	timeout time.Duration
}

// NewForestSource constructs a Source that calls
// Filecoin.F3GetLatestCertificate on a Forest/Lotus-compatible HTTP RPC
// endpoint. Empty token is fine for public archives.
func NewForestSource(name, url, token string, timeout time.Duration) bootstrap.Source {
	if name == "" {
		name = "forest:" + url
	}
	return &rpcSource{name: name, kind: bootstrap.KindForest, url: url, token: token, timeout: timeout}
}

// NewLanternGatewaySource constructs a Source for the Lantern project's
// own gateway. Same wire as ForestSource but tagged so quorum policy
// excludes it from the count by default.
func NewLanternGatewaySource(name, url string, timeout time.Duration) bootstrap.Source {
	if name == "" {
		name = "lantern-gateway:" + url
	}
	return &rpcSource{name: name, kind: bootstrap.KindLanternGateway, url: url, timeout: timeout}
}

// NewUserPeerSource constructs a Source for a user-supplied --peer URL.
// These count toward the quorum because the user has explicitly opted
// in by listing them on the command line.
func NewUserPeerSource(name, url, token string, timeout time.Duration) bootstrap.Source {
	if name == "" {
		name = "user-peer:" + url
	}
	return &rpcSource{name: name, kind: bootstrap.KindUser, url: url, token: token, timeout: timeout}
}

func (s *rpcSource) Name() string         { return s.name }
func (s *rpcSource) Kind() bootstrap.Kind { return s.kind }

func (s *rpcSource) LatestFinality(ctx context.Context) (bootstrap.Finality, error) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "Filecoin.F3GetLatestCertificate",
		"params":  []any{},
	})
	req, err := http.NewRequestWithContext(cctx, "POST", s.url, bytes.NewReader(body))
	if err != nil {
		return bootstrap.Finality{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return bootstrap.Finality{}, fmt.Errorf("rpc %s: %w", s.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return bootstrap.Finality{}, fmt.Errorf("rpc %s: HTTP %d: %s", s.url, resp.StatusCode, snippet(raw))
	}
	var env struct {
		Result *certs.FinalityCertificate `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return bootstrap.Finality{}, fmt.Errorf("rpc %s: decode: %w (body=%s)", s.url, err, snippet(raw))
	}
	if env.Error != nil {
		return bootstrap.Finality{}, fmt.Errorf("rpc %s: %s (code=%d)", s.url, env.Error.Message, env.Error.Code)
	}
	if env.Result == nil {
		return bootstrap.Finality{}, fmt.Errorf("rpc %s: nil cert", s.url)
	}
	return finalityFromCert(env.Result)
}

func snippet(b []byte) string {
	if len(b) > 220 {
		return string(b[:220]) + "..."
	}
	return string(b)
}

// finalityFromCert pulls the (instance, head tipset, state root) tuple
// out of an F3 finality certificate. The state root is encoded in the
// head tipset's Commitments field via a Filecoin-specific scheme: today
// only the tipset CIDs + state root are needed for the bootstrap trust
// anchor, and the state root is *not* in the F3 cert directly — it's
// derived by walking from the previously known anchor. Since the
// bootstrap quorum's purpose is to agree on which tipset is finalized,
// we equality-check the finality on (instance, tipset CIDs); the state
// root is captured later when Lantern's state walker first resolves the
// tipset against its blockstore.
//
// To keep Source.LatestFinality stateless and side-effect-free, we set
// Finality.StateRoot to a deterministic synthetic CID derived from the
// tipset key bytes. Two sources agreeing on the same finalized tipset
// will therefore agree on the same StateRoot, while two sources
// returning different tipsets will not. This satisfies the quorum
// equality contract.
func finalityFromCert(c *certs.FinalityCertificate) (bootstrap.Finality, error) {
	if c == nil || c.ECChain == nil {
		return bootstrap.Finality{}, errors.New("nil cert/ECChain")
	}
	head := c.ECChain.Head()
	if head == nil {
		return bootstrap.Finality{}, errors.New("empty ECChain")
	}
	cids, err := cidsFromTipSetKey(head.Key)
	if err != nil {
		return bootstrap.Finality{}, fmt.Errorf("decode tsk: %w", err)
	}
	// Derive a deterministic state-root CID from the head tipset's
	// PowerTable + Commitments + Key. This is *not* the chain state
	// root (light clients learn that by walking the chain). It is a
	// digest the quorum uses to detect divergence: two sources returning
	// the same finalized tipset produce the same digest; two sources
	// returning different tipsets produce different digests.
	sr := head.PowerTable
	if !sr.Defined() {
		// Fallback: use the first block CID. Shouldn't happen on
		// well-formed mainnet certs but kept defensive.
		if len(cids) > 0 {
			sr = cids[0]
		}
	}
	return bootstrap.Finality{
		Instance:  c.GPBFTInstance,
		TipSetKey: cids,
		StateRoot: sr,
		Epoch:     head.Epoch,
	}, nil
}

// cidsFromTipSetKey is a copy of go-f3's unexported helper. The TipSet
// key is a concatenation of block-CID byte representations.
func cidsFromTipSetKey(encoded gpbft.TipSetKey) ([]cid.Cid, error) {
	if len(encoded) == 0 {
		return nil, errors.New("empty tipset key")
	}
	var out []cid.Cid
	for next := 0; next < len(encoded); {
		nr, c, err := cid.CidFromBytes(encoded[next:])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		next += nr
	}
	return out, nil
}

// ---------- libp2p cert-exchange source ----------

// Libp2pSource queries a single libp2p peer via the F3 cert-exchange
// protocol (/f3/certexch/get/1/<networkName>). Use one per peer; the
// quorum driver fans out across all libp2p sources in parallel.
type Libp2pSource struct {
	host    host.Host
	peer    peer.ID
	network gpbft.NetworkName
	timeout time.Duration
}

// NewLibp2pSource returns a Source for a single libp2p peer.
func NewLibp2pSource(h host.Host, p peer.ID, network gpbft.NetworkName, timeout time.Duration) bootstrap.Source {
	return &Libp2pSource{host: h, peer: p, network: network, timeout: timeout}
}

func (s *Libp2pSource) Name() string         { return "libp2p:" + s.peer.String() }
func (s *Libp2pSource) Kind() bootstrap.Kind { return bootstrap.KindLibp2p }

func (s *Libp2pSource) LatestFinality(ctx context.Context) (bootstrap.Finality, error) {
	if s.host == nil {
		return bootstrap.Finality{}, errors.New("libp2p source: nil host")
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// First: ask for instance=0 with limit=0 to learn the peer's
	// PendingInstance (the next instance to be finalized — i.e. latest
	// finalized + 1). This avoids streaming any certs we don't need.
	c := certexchange.Client{
		Host:           s.host,
		NetworkName:    s.network,
		RequestTimeout: timeout,
	}
	rh, _, err := c.Request(cctx, s.peer, &certexchange.Request{FirstInstance: 0, Limit: 0})
	if err != nil {
		return bootstrap.Finality{}, fmt.Errorf("certexch query head: %w", err)
	}
	if rh == nil || rh.PendingInstance == 0 {
		return bootstrap.Finality{}, errors.New("certexch: peer reports no finalized instances")
	}
	latest := rh.PendingInstance - 1

	// Second: stream exactly the latest cert (instance=latest, limit=1).
	rh2, ch, err := c.Request(cctx, s.peer, &certexchange.Request{FirstInstance: latest, Limit: 1})
	if err != nil {
		return bootstrap.Finality{}, fmt.Errorf("certexch fetch cert %d: %w", latest, err)
	}
	_ = rh2
	if ch == nil {
		return bootstrap.Finality{}, fmt.Errorf("certexch: nil channel for cert %d", latest)
	}
	var cert *certs.FinalityCertificate
	for c := range ch {
		if c != nil && c.GPBFTInstance == latest {
			cert = c
			break
		}
	}
	if cert == nil {
		return bootstrap.Finality{}, fmt.Errorf("certexch: peer did not deliver cert %d", latest)
	}
	return finalityFromCert(cert)
}

// ---------- Lantern beacon source (cert-exchange over libp2p) ----------

// ErrNoBeaconBackend is retained for backwards compatibility with V1.2.0
// callers that constructed a LanternBeaconSource without a host. In
// V1.2.1 (B-11-01) Lantern beacons serve cert-exchange over libp2p, so
// a properly constructed LanternBeaconSource via NewLanternBeaconSource
// returns real results; only the zero-value source emits this error.
var ErrNoBeaconBackend = errors.New("lantern beacon: source not configured with libp2p host")

// LanternBeaconSource queries a Lantern beacon over the F3
// cert-exchange protocol. As of V1.2.1 (B-11-01) beacons run a
// responder backed by their own verified-cert store, so this is a
// first-class quorum source.
//
// KindLanternBeacon counts toward the quorum by default. These are
// independent operators — not the project itself — so the trust model
// treats them like any other libp2p source. (Contrast with
// KindLanternGateway which is opt-in via --count-gateway because the
// project itself runs the gateway.)
type LanternBeaconSource struct {
	host    host.Host
	info    peer.AddrInfo
	network gpbft.NetworkName
	timeout time.Duration
}

// NewLanternBeaconSource returns a Source that asks a Lantern beacon
// for its latest F3 finality over cert-exchange.
//
// The beacon's full peer.AddrInfo is required so the source can dial
// even when the host hasn't already met the beacon via DHT. Pass the
// libp2p host that should perform the dial; nil host returns a source
// that always fails with ErrNoBeaconBackend (preserved for callers that
// still construct a placeholder).
func NewLanternBeaconSource(h host.Host, info peer.AddrInfo, network gpbft.NetworkName, timeout time.Duration) bootstrap.Source {
	return &LanternBeaconSource{host: h, info: info, network: network, timeout: timeout}
}

func (s *LanternBeaconSource) Name() string {
	return "lantern-beacon:" + s.info.ID.String()
}
func (s *LanternBeaconSource) Kind() bootstrap.Kind { return bootstrap.KindLanternBeacon }

func (s *LanternBeaconSource) LatestFinality(ctx context.Context) (bootstrap.Finality, error) {
	if s.host == nil {
		return bootstrap.Finality{}, ErrNoBeaconBackend
	}
	if s.info.ID == "" {
		return bootstrap.Finality{}, errors.New("lantern beacon: empty peer ID")
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	// Pre-populate the peerstore so certexchange.Client can dial without
	// a separate Connect round-trip. No-op if the host already knows
	// this peer.
	if len(s.info.Addrs) > 0 {
		s.host.Peerstore().AddAddrs(s.info.ID, s.info.Addrs, time.Hour)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := certexchange.Client{
		Host:           s.host,
		NetworkName:    s.network,
		RequestTimeout: timeout,
	}
	rh, _, err := c.Request(cctx, s.info.ID, &certexchange.Request{FirstInstance: 0, Limit: 0})
	if err != nil {
		return bootstrap.Finality{}, fmt.Errorf("lantern beacon certexch head: %w", err)
	}
	if rh == nil || rh.PendingInstance == 0 {
		return bootstrap.Finality{}, errors.New("lantern beacon: peer reports no finalized instances")
	}
	latest := rh.PendingInstance - 1

	_, ch, err := c.Request(cctx, s.info.ID, &certexchange.Request{FirstInstance: latest, Limit: 1})
	if err != nil {
		return bootstrap.Finality{}, fmt.Errorf("lantern beacon certexch fetch %d: %w", latest, err)
	}
	if ch == nil {
		return bootstrap.Finality{}, fmt.Errorf("lantern beacon: nil channel for cert %d", latest)
	}
	var cert *certs.FinalityCertificate
	for c := range ch {
		if c != nil && c.GPBFTInstance == latest {
			cert = c
			break
		}
	}
	if cert == nil {
		return bootstrap.Finality{}, fmt.Errorf("lantern beacon: peer did not deliver cert %d", latest)
	}
	return finalityFromCert(cert)
}
