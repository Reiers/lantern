// curioburst replays the read burst a stock Curio-PDP node fires at its
// chain node on every new head, and reports per-method latency + errors.
//
// Curio talks eth_* through ONE shared go-ethereum ethclient over a
// websocket (deps.GetEthClient: erpc.DialOptions on ws://.../rpc/v1), and
// Filecoin.* through a separate go-jsonrpc client. On each head it walks
// every proving data set and issues PDPVerifier view calls
// (getNextChallengeEpoch, getDataSetLeafCount, getChallengeRange, ...),
// polls eth_getTransactionReceipt for its pending messages, and reads
// eth_getBlockByNumber("latest"). The prove task also pulls challenge
// randomness via Filecoin.StateGetRandomnessDigestFromBeacon.
//
// This tool reproduces that shape:
//
//   - bursts are triggered by a new head (polled over a separate HTTP
//     connection, like Curio's ChainNotify connection), so the eth
//     websocket sees the same idle gaps between bursts Curio's does;
//   - every call carries a client timeout (default 5s);
//   - errors are bucketed (context timeout, transport i/o timeout,
//     connection, other) so a websocket teardown is visible as such.
//
// Usage:
//
//	go run . -rpc ws://127.0.0.1:12377/rpc/v1 -n 30 -duration 10m
//
// It lives in its own module so the go-ethereum client does not become a
// dependency of the lantern module.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	erpc "github.com/ethereum/go-ethereum/rpc"
)

// Calibration PDPVerifier proxy (same constant Curio ships in
// pdp/contract/addresses.go).
const pdpVerifierCalib = "0x85e366Cf9DD2c0aE37E963d9556F5f4718d6417C"

func sel(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

func callData(sig string, id uint64) []byte {
	out := append([]byte{}, sel(sig)...)
	word := make([]byte, 32)
	new(big.Int).SetUint64(id).FillBytes(word)
	return append(out, word...)
}

var perSetViews = []string{
	"getNextChallengeEpoch(uint256)",
	"getDataSetLeafCount(uint256)",
	"getChallengeRange(uint256)",
}

type sample struct {
	method string
	dur    time.Duration
	err    error
}

type stats struct {
	mu      sync.Mutex
	samples []sample
}

func (s *stats) add(m string, d time.Duration, err error) {
	s.mu.Lock()
	s.samples = append(s.samples, sample{m, d, err})
	s.mu.Unlock()
}

func classify(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "client-timeout"
	case strings.Contains(msg, "i/o timeout"):
		return "io-timeout"
	case strings.Contains(msg, "revert"):
		return "revert"
	case strings.Contains(msg, "connection") || strings.Contains(msg, "EOF") || strings.Contains(msg, "closed"):
		return "conn"
	default:
		return "other"
	}
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	return d[int(float64(len(d)-1)*p)]
}

// filRPC is a minimal HTTP JSON-RPC caller for Filecoin.* and the
// reference node.
type filRPC struct {
	url, token string
	hc         *http.Client
}

func (f *filRPC) call(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode %s: %w (%.120s)", method, err, raw)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: %d %s", method, env.Error.Code, env.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func ethCallHex(ctx context.Context, f *filRPC, to string, data []byte) (string, error) {
	var out string
	err := f.call(ctx, "eth_call", []any{map[string]string{"to": to, "data": "0x" + hex.EncodeToString(data)}, "latest"}, &out)
	return out, err
}

// pickDataSets returns up to n live data set ids, newest first, read from
// the reference node.
func pickDataSets(ctx context.Context, ref *filRPC, n int) ([]uint64, error) {
	res, err := ethCallHex(ctx, ref, pdpVerifierCalib, sel("getNextDataSetId()"))
	if err != nil {
		return nil, fmt.Errorf("getNextDataSetId: %w", err)
	}
	next, ok := new(big.Int).SetString(strings.TrimPrefix(res, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("bad getNextDataSetId result %q", res)
	}
	var ids []uint64
	for id := next.Uint64(); id > 0 && len(ids) < n; id-- {
		cand := id - 1
		r, err := ethCallHex(ctx, ref, pdpVerifierCalib, callData("dataSetLive(uint256)", cand))
		if err != nil {
			continue
		}
		if v, ok := new(big.Int).SetString(strings.TrimPrefix(r, "0x"), 16); ok && v.Sign() != 0 {
			ids = append(ids, cand)
		}
	}
	return ids, nil
}

type tipsetLite struct {
	Height int64 `json:"Height"`
}

type row struct {
	Method  string         `json:"method"`
	N       int            `json:"n"`
	Errors  map[string]int `json:"errors"`
	P50ms   float64        `json:"p50_ms"`
	P99ms   float64        `json:"p99_ms"`
	MaxMs   float64        `json:"max_ms"`
	ErrRate float64        `json:"err_rate"`
}

func main() {
	rpcURL := flag.String("rpc", "ws://127.0.0.1:12377/rpc/v1", "chain node under test (ws:// like Curio, or http://)")
	httpURL := flag.String("http", "", "HTTP URL of the node under test for Filecoin.* calls + head polling (default: derived from -rpc)")
	token := flag.String("token", os.Getenv("LANTERN_TOKEN"), "bearer token for the node under test")
	refURL := flag.String("ref", "https://api.calibration.node.glif.io/rpc/v1", "reference node used to pick live data sets and verify results")
	n := flag.Int("n", 30, "number of data sets per burst")
	idsFlag := flag.String("ids", "", "comma-separated data set ids (skips discovery)")
	duration := flag.Duration("duration", 10*time.Minute, "run length")
	timeout := flag.Duration("timeout", 5*time.Second, "per-call client timeout")
	every := flag.Duration("every", 0, "fixed burst interval instead of head-triggered (0 = on new head)")
	verify := flag.Bool("verify", true, "compare per-set view results against -ref at the end")
	jsonOut := flag.String("json", "", "write the summary as JSON to this path")
	idleProbe := flag.Bool("ws-idle-probe", false, "only run the websocket keepalive probe: one call, 35s idle (go-ethereum then pings and arms a 30s pong deadline), then a call every 3s for 45s")
	flag.Parse()

	ctx := context.Background()
	hu := *httpURL
	if hu == "" {
		hu = strings.Replace(strings.Replace(*rpcURL, "ws://", "http://", 1), "wss://", "https://", 1)
	}
	node := &filRPC{url: hu, token: *token, hc: &http.Client{Timeout: 30 * time.Second}}
	ref := &filRPC{url: *refURL, hc: &http.Client{Timeout: 30 * time.Second}}

	var ids []uint64
	if *idsFlag != "" {
		for _, s := range strings.Split(*idsFlag, ",") {
			var v uint64
			if _, err := fmt.Sscan(strings.TrimSpace(s), &v); err == nil {
				ids = append(ids, v)
			}
		}
	} else {
		var err error
		ids, err = pickDataSets(ctx, ref, *n)
		if err != nil {
			fmt.Fprintln(os.Stderr, "pick data sets:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("data sets: %d %v\n", len(ids), ids)

	// One shared go-ethereum client, exactly like Curio's EthClient.
	hdr := http.Header{}
	if *token != "" {
		hdr.Set("Authorization", "Bearer "+*token)
	}
	rc, err := erpc.DialOptions(ctx, *rpcURL, erpc.WithHeaders(hdr))
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	ec := ethclient.NewClient(rc)
	pdpv := common.HexToAddress(pdpVerifierCalib)

	if *idleProbe {
		os.Exit(wsIdleProbe(ctx, rc))
	}

	st := &stats{}
	var pendingHashes []common.Hash
	var burstWalls []time.Duration

	timed := func(m string, f func(context.Context) error) {
		cctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		t0 := time.Now()
		err := f(cctx)
		st.add(m, time.Since(t0), err)
	}

	burst := func(head int64) {
		t0 := time.Now()
		var wg sync.WaitGroup
		for _, id := range ids {
			for _, sig := range perSetViews {
				wg.Add(1)
				go func(id uint64, sig string) {
					defer wg.Done()
					timed("eth_call "+sig[:strings.IndexByte(sig, '(')], func(c context.Context) error {
						_, err := ec.CallContract(c, ethereum.CallMsg{To: &pdpv, Data: callData(sig, id)}, nil)
						return err
					})
				}(id, sig)
			}
		}
		for _, h := range pendingHashes {
			wg.Add(1)
			go func(h common.Hash) {
				defer wg.Done()
				timed("eth_getTransactionReceipt", func(c context.Context) error {
					_, err := ec.TransactionReceipt(c, h)
					if errors.Is(err, ethereum.NotFound) {
						return nil
					}
					return err
				})
			}(h)
		}
		wg.Add(3)
		go func() {
			defer wg.Done()
			timed("eth_getBlockByNumber latest", func(c context.Context) error {
				var raw json.RawMessage
				return rc.CallContext(c, &raw, "eth_getBlockByNumber", "latest", false)
			})
		}()
		go func() {
			defer wg.Done()
			timed("eth_blockNumber", func(c context.Context) error {
				_, err := ec.BlockNumber(c)
				return err
			})
		}()
		go func() {
			defer wg.Done()
			// Curio's prove task: challenge randomness at a recent epoch.
			timed("Filecoin.StateGetRandomnessDigestFromBeacon", func(c context.Context) error {
				var out any
				return node.call(c, "Filecoin.StateGetRandomnessDigestFromBeacon", []any{head - 10, []any{}}, &out)
			})
		}()
		wg.Wait()
		burstWalls = append(burstWalls, time.Since(t0))
	}

	// Receipt polls: a few real recent tx hashes plus unknown hashes
	// (messages Curio sent that are still pending).
	{
		var blk struct {
			Transactions []string `json:"transactions"`
		}
		_ = ref.call(ctx, "eth_getBlockByNumber", []any{"latest", false}, &blk)
		for i, h := range blk.Transactions {
			if i >= 4 {
				break
			}
			pendingHashes = append(pendingHashes, common.HexToHash(h))
		}
		for i := 0; i < 4; i++ {
			pendingHashes = append(pendingHashes, common.BytesToHash(crypto.Keccak256([]byte(fmt.Sprintf("pending-%d-%d", time.Now().UnixNano(), i)))))
		}
	}

	end := time.Now().Add(*duration)
	lastHead := int64(-1)
	for time.Now().Before(end) {
		var h tipsetLite
		if err := node.call(ctx, "Filecoin.ChainHead", []any{}, &h); err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if *every > 0 || (h.Height != lastHead && lastHead >= 0) {
			burst(h.Height)
			fmt.Printf("burst %d head=%d wall=%s\n", len(burstWalls), h.Height, burstWalls[len(burstWalls)-1].Round(time.Millisecond))
		}
		lastHead = h.Height
		if *every > 0 {
			time.Sleep(*every)
		} else {
			time.Sleep(2 * time.Second)
		}
	}

	by := map[string][]sample{}
	for _, s := range st.samples {
		by[s.method] = append(by[s.method], s)
	}
	var names []string
	for k := range by {
		names = append(names, k)
	}
	sort.Strings(names)
	var rows []row
	firstErr := map[string]string{}
	fmt.Printf("\n%-46s %6s %8s %10s %10s %10s  %s\n", "method", "n", "err%", "p50", "p99", "max", "errors")
	for _, m := range names {
		ss := by[m]
		var ds []time.Duration
		errs := map[string]int{}
		nerr := 0
		for _, s := range ss {
			ds = append(ds, s.dur)
			if c := classify(s.err); c != "" && c != "revert" {
				errs[c]++
				nerr++
				if _, ok := firstErr[m]; !ok {
					firstErr[m] = s.err.Error()
				}
			}
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		r := row{Method: m, N: len(ss), Errors: errs,
			P50ms:   float64(pct(ds, 0.50).Microseconds()) / 1000,
			P99ms:   float64(pct(ds, 0.99).Microseconds()) / 1000,
			MaxMs:   float64(ds[len(ds)-1].Microseconds()) / 1000,
			ErrRate: float64(nerr) / float64(len(ss))}
		rows = append(rows, r)
		fmt.Printf("%-46s %6d %7.2f%% %9.1fms %9.1fms %9.1fms  %v\n", m, r.N, 100*r.ErrRate, r.P50ms, r.P99ms, r.MaxMs, errs)
	}
	for m, e := range firstErr {
		fmt.Printf("first error %s: %.200s\n", m, e)
	}
	walls := append([]time.Duration{}, burstWalls...)
	sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
	if len(walls) > 0 {
		fmt.Printf("bursts=%d burst-wall p50=%s max=%s\n", len(walls), pct(walls, 0.5).Round(time.Millisecond), walls[len(walls)-1].Round(time.Millisecond))
	}

	// Correctness: same view calls against the reference. Two nodes can be
	// one head apart, so a mismatch is re-checked once after both settle.
	mismatches := 0
	if *verify {
		check := func() []string {
			var bad []string
			for _, id := range ids {
				for _, sig := range perSetViews {
					a, errA := ethCallHex(ctx, node, pdpVerifierCalib, callData(sig, id))
					b, errB := ethCallHex(ctx, ref, pdpVerifierCalib, callData(sig, id))
					if (errA == nil) != (errB == nil) || a != b {
						bad = append(bad, fmt.Sprintf("%d %s node=%s/%v ref=%s/%v", id, sig, a, errA, b, errB))
					}
				}
			}
			return bad
		}
		bad := check()
		if len(bad) > 0 {
			time.Sleep(35 * time.Second)
			bad = check()
		}
		mismatches = len(bad)
		fmt.Printf("verify vs ref: %d calls, %d mismatches\n", len(ids)*len(perSetViews), mismatches)
		for _, b := range bad {
			fmt.Println("  ", b)
		}
	}
	if *jsonOut != "" {
		ms := make([]float64, len(burstWalls))
		for i, w := range burstWalls {
			ms[i] = float64(w.Milliseconds())
		}
		b, _ := json.MarshalIndent(map[string]any{"rows": rows, "bursts": len(burstWalls), "burst_wall_ms": ms, "mismatches": mismatches, "data_sets": len(ids)}, "", "  ")
		_ = os.WriteFile(*jsonOut, b, 0o644)
	}
}

// wsIdleProbe checks websocket keepalive interop with go-ethereum's client.
// After 30s without a client write, go-ethereum sends a ping and sets a
// 30s read deadline that only a pong clears. A server that swallows the
// ping kills the connection 30s later with "read tcp ...: i/o timeout",
// failing every call in flight at that moment (Curio logs these as
// "chain: read tcp ...:1234: i/o timeout"). A newHeads subscription keeps
// a long-lived request open across that moment so the teardown is
// observable even when individual calls are fast. Returns the exit code.
func wsIdleProbe(ctx context.Context, rc *erpc.Client) int {
	heads := make(chan json.RawMessage, 16)
	sub, err := rc.EthSubscribe(ctx, heads, "newHeads")
	if err != nil {
		fmt.Println("eth_subscribe failed:", err)
		return 1
	}
	defer sub.Unsubscribe()
	fmt.Println("subscribed; idling 75s with no client writes (client ping at 30s, pong deadline at 60s)...")
	t0 := time.Now()
	n := 0
	deadline := time.After(75 * time.Second)
	for {
		select {
		case <-heads:
			n++
		case err := <-sub.Err():
			fmt.Printf("ws-idle-probe: FAIL at t+%.0fs after %d heads: %v\n", time.Since(t0).Seconds(), n, err)
			return 1
		case <-deadline:
			fmt.Printf("ws-idle-probe: OK (connection survived the keepalive ping, %d heads received)\n", n)
			return 0
		}
	}
}
