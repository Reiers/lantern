// Test for the gateway-backed HeadSource (#80 head-source diversity): the
// Lantern gateway is an independent-Kind corroborating head source for the
// running-head divergence monitor. It speaks HTTP /state/root (not
// Filecoin.ChainHead JSON-RPC), so it has its own adapter.

package headcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	abi "github.com/filecoin-project/go-state-types/abi"

	"github.com/Reiers/lantern/chain/bootstrap"
)

func TestGatewayHeadSource_ParsesEpoch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/state/root" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Epoch": 6140000, "StateRoot": "bafy2bzaceabc"}`))
	}))
	defer srv.Close()

	src := NewGatewayHeadSource(srv.URL, 0)
	if src.Kind() != bootstrap.KindLanternGateway {
		t.Fatalf("gateway source kind = %q, want %q", src.Kind(), bootstrap.KindLanternGateway)
	}
	ep, err := src.HeadEpoch(context.Background())
	if err != nil {
		t.Fatalf("HeadEpoch error: %v", err)
	}
	if ep != abi.ChainEpoch(6140000) {
		t.Fatalf("HeadEpoch = %d, want 6140000", ep)
	}
}

func TestGatewayHeadSource_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	src := NewGatewayHeadSource(srv.URL, 0)
	if _, err := src.HeadEpoch(context.Background()); err == nil {
		t.Fatal("expected error on HTTP 502, got nil")
	}
}
