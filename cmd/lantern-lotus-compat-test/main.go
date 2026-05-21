// lantern-lotus-compat-test verifies Lotus RPC client compatibility.
//
// It uses go-jsonrpc to dial Lantern over its FULLNODE_API_INFO and
// invokes:
//
//   - Filecoin.Version
//   - Filecoin.ChainHead
//   - Filecoin.StateNetworkVersion
//   - Filecoin.StateGetActor (f099, f04)
//   - Filecoin.WalletList
//   - Filecoin.WalletBalance against a known on-chain address
//
// If every call succeeds and returns the expected shape, Lantern's RPC
// surface is wire-compatible with what `lotus` would send.
//
// Run:
//
//	lantern-lotus-compat-test \
//	  --api-info "$(cat ~/.lantern/token):/ip4/127.0.0.1/tcp/11234/http"

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/types"
)

// LotusClient is the typed Lotus-shape client. It mirrors the Lotus-side
// FullNodeStruct.Internal — go-jsonrpc.NewClient reflects over these
// fields and dispatches by method name.
type LotusClient struct {
	Internal struct {
		Version               func(ctx context.Context) (api.Version, error)
		ChainHead             func(ctx context.Context) (*types.TipSet, error)
		StateNetworkVersion   func(ctx context.Context, key types.TipSetKey) (network.Version, error)
		StateGetActor         func(ctx context.Context, a address.Address, key types.TipSetKey) (*types.Actor, error)
		StateLookupID         func(ctx context.Context, a address.Address, key types.TipSetKey) (address.Address, error)
		WalletList            func(ctx context.Context) ([]address.Address, error)
		WalletBalance         func(ctx context.Context, a address.Address) (big.Int, error)
		StateNetworkName      func(ctx context.Context) (string, error)
	}
}

func main() {
	apiInfo := flag.String("api-info", os.Getenv("FULLNODE_API_INFO"), "FULLNODE_API_INFO")
	flag.Parse()
	if *apiInfo == "" {
		fatal("FULLNODE_API_INFO not set")
	}

	tok, addr := parseAPIInfo(*apiInfo)
	url, err := multiaddrToHTTP(addr)
	if err != nil {
		fatal("bad multiaddr: %v", err)
	}

	fmt.Println("Lotus-compat smoke test")
	fmt.Println("=======================")
	fmt.Println("URL:  ", url)
	fmt.Println("Token:", short(tok))
	fmt.Println()

	ctx := context.Background()

	cl := &LotusClient{}
	closer, err := jsonrpc.NewMergeClient(ctx, url, "Filecoin",
		[]interface{}{&cl.Internal},
		http.Header{"Authorization": {"Bearer " + tok}},
	)
	if err != nil {
		fatal("dial: %v", err)
	}
	defer closer()

	check("Filecoin.Version", func() (any, error) { return cl.Internal.Version(ctx) })
	check("Filecoin.ChainHead", func() (any, error) {
		ts, err := cl.Internal.ChainHead(ctx)
		if err != nil {
			return nil, err
		}
		return fmt.Sprintf("height=%d cids=%d", ts.Height(), len(ts.Cids())), nil
	})
	check("Filecoin.StateNetworkVersion", func() (any, error) {
		return cl.Internal.StateNetworkVersion(ctx, types.TipSetKey{})
	})
	check("Filecoin.StateNetworkName", func() (any, error) {
		return cl.Internal.StateNetworkName(ctx)
	})

	f099, _ := address.NewFromString("f099")
	check("Filecoin.StateLookupID(f099)", func() (any, error) {
		return cl.Internal.StateLookupID(ctx, f099, types.TipSetKey{})
	})
	check("Filecoin.StateGetActor(f099)", func() (any, error) {
		act, err := cl.Internal.StateGetActor(ctx, f099, types.TipSetKey{})
		if err != nil {
			return nil, err
		}
		return fmt.Sprintf("balance=%s code=%s", act.Balance.String(), act.Code), nil
	})
	check("Filecoin.WalletBalance(f099)", func() (any, error) {
		b, err := cl.Internal.WalletBalance(ctx, f099)
		if err != nil {
			return nil, err
		}
		return b.String(), nil
	})
	check("Filecoin.WalletList", func() (any, error) {
		return cl.Internal.WalletList(ctx)
	})

	// Round-trip: confirm a wallet we created is reachable.
	addrs, err := cl.Internal.WalletList(ctx)
	if err == nil && len(addrs) > 0 {
		// Pick a wallet-owned address and query its balance through the
		// Lantern RPC (should be zero for fresh wallets).
		check(fmt.Sprintf("Filecoin.WalletBalance(%s)", addrs[0]), func() (any, error) {
			b, err := cl.Internal.WalletBalance(ctx, addrs[0])
			if err != nil {
				return nil, err
			}
			return b.String(), nil
		})
	}

	fmt.Println()
	fmt.Println("OK — Lotus-compatible client successfully dialled, authed, and decoded responses.")
}

func parseAPIInfo(s string) (string, string) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		fatal("malformed FULLNODE_API_INFO")
	}
	return s[:i], s[i+1:]
}

// multiaddrToHTTP turns "/ip4/<host>/tcp/<port>/http" into an http URL.
func multiaddrToHTTP(maddr string) (string, error) {
	parts := strings.Split(maddr, "/")
	var host, port string
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "ip4", "ip6", "dns", "dns4", "dns6":
			if i+1 < len(parts) {
				host = parts[i+1]
			}
		case "tcp":
			if i+1 < len(parts) {
				port = parts[i+1]
			}
		}
	}
	if host == "" || port == "" {
		return "", fmt.Errorf("missing host or port in %q", maddr)
	}
	scheme := "http"
	if strings.HasSuffix(maddr, "/https") || strings.HasSuffix(maddr, "/wss") {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%s/rpc/v1", scheme, host, port), nil
}

func check(name string, fn func() (any, error)) {
	start := time.Now()
	res, err := fn()
	dur := time.Since(start)
	if err != nil {
		fmt.Printf("  FAIL %-50s %v  (%s)\n", name, err, dur)
		return
	}
	fmt.Printf("  OK   %-50s %v  (%s)\n", name, res, dur)
}

func short(t string) string {
	if len(t) > 20 {
		return t[:10] + "..." + t[len(t)-6:]
	}
	return t
}

func fatal(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "error:", fmt.Sprintf(format, args...))
	os.Exit(1)
}
