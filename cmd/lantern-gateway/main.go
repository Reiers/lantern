// lantern-gateway is the proxy HTTP server backing gateway.lantern.reiers.io.
//
// Endpoints:
//
//	GET /block/{cid}      → raw IPLD-DAG-CBOR block bytes, with CID
//	                        re-verified server-side. Returns 404 if no
//	                        upstream has the block.
//	GET /state/root       → current finalized state head as JSON:
//	                        { epoch, tipsetKey[], stateRoot, parentWeight }
//	GET /healthz          → "ok"
//
// Upstream strategy:
//
//   - Filecoin RPC (Glif) for state blocks. Most Filecoin state CIDs aren't
//     publicly pinned on IPFS, but Glif's hot blockstore has the recent ~30
//     days of state. `Filecoin.ChainReadObj` returns base64 block bytes.
//   - Public IPFS gateways (ipfs.io, w3s.link) as a parallel fallback, for
//     blocks that happen to be on the public IPFS network (rare for chain
//     state, common for some sector commitment CIDs).
//
// Defense in depth: every returned block has its CID recomputed and
// verified before the response writes. The client (net/hsync) ALSO
// verifies. Two checks because a single network or process bug should not
// silently corrupt downstream state.
//
// See PHASE2-BLOCKERS.md item B7 for why this isn't a full Forest on the
// Hetzner box.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/hamt"
)

type config struct {
	addr      string
	glifRPC   string
	ipfsGWs   stringList
	cacheSize int
	logLevel  string
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", ":8080", "HTTP listen address")
	flag.StringVar(&cfg.glifRPC, "glif", "https://api.node.glif.io/rpc/v1", "Filecoin RPC URL (Glif by default)")
	flag.Var(&cfg.ipfsGWs, "ipfs-gw", "Additional IPFS gateway base URLs (repeatable). Defaults: ipfs.io, w3s.link")
	flag.IntVar(&cfg.cacheSize, "cache-size", 4096, "In-process LRU cache size (number of blocks)")
	flag.StringVar(&cfg.logLevel, "log", "info", "log level: info|debug")
	flag.Parse()

	if len(cfg.ipfsGWs) == 0 {
		cfg.ipfsGWs = []string{
			"https://ipfs.io",
			"https://w3s.link",
		}
	}

	gw := newGateway(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/block/", gw.handleBlock)
	mux.HandleFunc("/state/root", gw.handleStateRoot)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("lantern-gateway listening on %s (glif=%s, ipfs-gws=%v, cache=%d)", cfg.addr, cfg.glifRPC, cfg.ipfsGWs, cfg.cacheSize)
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// ---------------------------------------------------------------------

type gateway struct {
	cfg config
	hc  *http.Client

	mu    sync.Mutex
	cache map[string][]byte // CID string -> block bytes (bounded by cacheSize)
	order []string          // LRU order
}

func newGateway(cfg config) *gateway {
	return &gateway{
		cfg:   cfg,
		hc:    &http.Client{Timeout: 30 * time.Second},
		cache: make(map[string][]byte),
	}
}

func (g *gateway) handleBlock(w http.ResponseWriter, r *http.Request) {
	cidStr := strings.TrimPrefix(r.URL.Path, "/block/")
	if cidStr == "" {
		http.Error(w, "missing CID", http.StatusBadRequest)
		return
	}
	c, err := cid.Parse(cidStr)
	if err != nil {
		http.Error(w, "bad CID: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Cache?
	g.mu.Lock()
	if raw, ok := g.cache[c.KeyString()]; ok {
		g.touch(c.KeyString())
		g.mu.Unlock()
		writeBlock(w, raw)
		return
	}
	g.mu.Unlock()

	raw, src, err := g.fetch(r.Context(), c)
	if err != nil {
		log.Printf("MISS  %s: %v", c, err)
		if errors.Is(err, errNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		}
		return
	}
	log.Printf("HIT   %s (%d bytes via %s)", c, len(raw), src)
	g.put(c.KeyString(), raw)
	writeBlock(w, raw)
}

func writeBlock(w http.ResponseWriter, raw []byte) {
	w.Header().Set("Content-Type", "application/vnd.ipld.raw")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(raw)
}

func (g *gateway) handleStateRoot(w http.ResponseWriter, r *http.Request) {
	body := []byte(`{"jsonrpc":"2.0","method":"Filecoin.ChainHead","params":[],"id":1}`)
	resp, err := g.hc.Post(g.cfg.glifRPC, "application/json", bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	dec := json.NewDecoder(bytes.NewReader(all))
	var out struct {
		Result struct {
			Cids []struct {
				Slash string `json:"/"`
			} `json:"Cids"`
			Height int64 `json:"Height"`
			Blocks []struct {
				ParentStateRoot struct {
					Slash string `json:"/"`
				} `json:"ParentStateRoot"`
				ParentWeight string `json:"ParentWeight"`
			} `json:"Blocks"`
		} `json:"result"`
	}
	if err := dec.Decode(&out); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	tipsetKey := make([]string, 0, len(out.Result.Cids))
	for _, c := range out.Result.Cids {
		tipsetKey = append(tipsetKey, c.Slash)
	}
	resJSON := map[string]any{
		"epoch":        out.Result.Height,
		"tipsetKey":    tipsetKey,
		"stateRoot":    out.Result.Blocks[0].ParentStateRoot.Slash,
		"parentWeight": out.Result.Blocks[0].ParentWeight,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resJSON)
}

// fetch tries Glif first, then each IPFS gateway. Returns the first
// CID-matching response.
func (g *gateway) fetch(ctx context.Context, c cid.Cid) ([]byte, string, error) {
	// 1. Glif RPC.
	raw, err := g.fetchGlif(ctx, c)
	if err == nil {
		if err := hamt.VerifyBlockCID(c, raw); err == nil {
			return raw, "glif", nil
		}
	}
	// 2. IPFS gateways in order.
	for _, gw := range g.cfg.ipfsGWs {
		url := fmt.Sprintf("%s/ipfs/%s?format=raw", strings.TrimRight(gw, "/"), c.String())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Accept", "application/vnd.ipld.raw")
		resp, gerr := g.hc.Do(req)
		if gerr != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		if err := hamt.VerifyBlockCID(c, body); err != nil {
			continue
		}
		return body, gw, nil
	}
	return nil, "", errNotFound
}

func (g *gateway) fetchGlif(ctx context.Context, c cid.Cid) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "Filecoin.ChainReadObj",
		"params":  []any{map[string]string{"/": c.String()}},
		"id":      1,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.glifRPC, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	dec := json.NewDecoder(bytes.NewReader(all))
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("glif: %s", out.Error.Message)
	}
	raw, err := base64.StdEncoding.DecodeString(out.Result)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (g *gateway) put(k string, raw []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.cache[k]; !ok {
		g.cache[k] = raw
		g.order = append(g.order, k)
	}
	for len(g.cache) > g.cfg.cacheSize {
		oldest := g.order[0]
		g.order = g.order[1:]
		delete(g.cache, oldest)
	}
}

func (g *gateway) touch(k string) {
	for i, o := range g.order {
		if o == k {
			g.order = append(append(g.order[:i], g.order[i+1:]...), k)
			return
		}
	}
}

var errNotFound = errors.New("not found in any upstream")

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		h.ServeHTTP(w, r)
	})
}
