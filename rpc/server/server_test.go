package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/filecoin-project/go-jsonrpc/auth"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/rpc/handlers"
)

// stubBlockGetter satisfies hamt.BlockGetter with a never-found impl.
type stubBlockGetter struct{}

func (stubBlockGetter) Get(_ context.Context, _ interface{}) ([]byte, error) {
	return nil, fmt.Errorf("not implemented")
}

func TestServerVersionAndAuth(t *testing.T) {
	dir := t.TempDir()

	tr := &trustedroot.TrustedRoot{Epoch: 12345}
	chainAPI := handlers.New(tr, nil, nil, nil, "mainnet")

	srv, err := New(Config{
		ListenAddress: "127.0.0.1:0",
		DataDir:       dir,
	}, chainAPI)
	if err != nil {
		t.Fatal(err)
	}
	chainAPI.AuthIssuer = srv.Auth()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(context.Background())

	apiInfo, err := srv.FullNodeAPIInfo()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(apiInfo, "/tcp/") {
		t.Fatalf("bad FULLNODE_API_INFO: %s", apiInfo)
	}

	// Hit Filecoin.Version with admin token.
	host := srv.Addr().String()
	tok := srv.Auth().Token(api.PermAdmin)
	if tok == "" {
		t.Fatal("admin token empty")
	}

	body := `{"jsonrpc":"2.0","method":"Filecoin.Version","params":[],"id":1}`
	req, _ := http.NewRequest("POST", "http://"+host+"/rpc/v1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var rr struct {
		Result struct {
			Version    string
			APIVersion uint32
			BlockDelay uint64
		}
		Error *struct {
			Message string
		}
	}
	if err := json.Unmarshal(rb, &rr); err != nil {
		t.Fatalf("decode %q: %v", string(rb), err)
	}
	if rr.Error != nil {
		t.Fatalf("rpc error: %s", rr.Error.Message)
	}
	// V1.2.1: Version is now "<tag> Lantern+<network>" (capital L). Match
	// the case-insensitive substring so the test survives both the dev
	// build ("dev Lantern+mainnet") and a tagged release ("v1.2.1 Lantern+mainnet").
	if !strings.Contains(strings.ToLower(rr.Result.Version), "lantern") {
		t.Fatalf("unexpected version: %+v", rr.Result)
	}
	if rr.Result.BlockDelay != 30 {
		t.Fatalf("BlockDelay: %d", rr.Result.BlockDelay)
	}
}

func TestAuthRejectsWriteWithReadToken(t *testing.T) {
	dir := t.TempDir()
	tr := &trustedroot.TrustedRoot{Epoch: 0}
	chainAPI := handlers.New(tr, nil, nil, nil, "mainnet")

	srv, _ := New(Config{ListenAddress: "127.0.0.1:0", DataDir: dir}, chainAPI)
	chainAPI.AuthIssuer = srv.Auth()
	srv.Start()
	defer srv.Stop(context.Background())

	// Mint a read-only token.
	readTok, err := srv.Auth().AuthNew([]auth.Permission{api.PermRead})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"jsonrpc":"2.0","method":"Filecoin.WalletNew","params":["bls"],"id":1}`
	req, _ := http.NewRequest("POST", "http://"+srv.Addr().String()+"/rpc/v1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+string(readTok))
	req.Header.Set("Content-Type", "application/json")

	resp, _ := http.DefaultClient.Do(req)
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(rb), "missing permission") {
		t.Fatalf("expected perm error, got %s", string(rb))
	}
}
