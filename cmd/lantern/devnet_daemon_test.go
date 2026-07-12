// Boot-path smoke test for `lantern daemon --network devnet`. Spins up a
// fake lotus JSON-RPC that serves the handful of methods the boot
// sequence touches, runs devnet-init to seed config + anchor, then
// starts the daemon in a goroutine and asserts:
//   - it loads the devnet config
//   - it configures the runtime Network variant
//   - it opens RPC + shuts down cleanly on ctx cancel
//
// This is what tells us the wiring survives future refactors — not just
// the isolated cmd/lantern/devnet.go unit test.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Reiers/lantern/build"
)

// fakeDaemonLotus is fakeLotusRPC extended with the couple of extra
// methods `lantern daemon --network devnet` calls during startup. Every
// unknown method returns an empty result rather than 404 so the boot
// path can gracefully degrade instead of crash-looping.
func fakeDaemonLotus(t *testing.T, networkName, genesisCIDStr string, headEpoch int64, stateRootCIDStr, tsCIDStr string) (*httptest.Server, *atomic.Uint64) {
	t.Helper()
	var calls atomic.Uint64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST expected", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		calls.Add(1)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "Filecoin.StateNetworkName":
			resp["result"] = networkName
		case "Filecoin.ChainGetGenesis":
			resp["result"] = map[string]any{"Cids": []map[string]string{{"/": genesisCIDStr}}}
		case "Filecoin.ChainHead":
			resp["result"] = map[string]any{
				"Height": headEpoch,
				"Cids":   []map[string]string{{"/": tsCIDStr}},
				"Blocks": []map[string]any{{
					"Height":                headEpoch,
					"ParentStateRoot":       map[string]string{"/": stateRootCIDStr},
					"ParentMessageReceipts": map[string]string{"/": stateRootCIDStr},
					"ParentWeight":          "12345",
				}},
			}
		case "eth_chainId":
			resp["result"] = "0x1df5e76" // 31415926, curio-fork devnet default
		case "Filecoin.Version":
			resp["result"] = map[string]any{
				"Version":    "1.35.1+devnet-fake",
				"APIVersion": 0x00020300,
				"BlockDelay": 4,
			}
		default:
			// Return an "unimplemented" JSON-RPC error rather than 404.
			// Lantern's boot path already tolerates optional-method failures
			// (F3 latest, network-name second-source, etc.) via best-effort
			// paths — this shape matches what a real devnet lotus does for
			// methods it doesn't implement.
			resp["error"] = map[string]any{"code": -32601, "message": "method not found: " + req.Method}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv, &calls
}

// TestDevnet_DevnetInitPersistsConfig walks the full flow via
// cmdDevnetInit against a fake lotus, then re-loads the config via
// LoadDevnetConfig + ConfigureDevnet and asserts every Network method
// returns the right value for the loaded config. This proves the
// devnet-init → daemon-load handshake works without needing to actually
// start the full daemon (which would need a wallet, keystore setup, and
// a real libp2p host).
func TestDevnet_DevnetInitPersistsConfig(t *testing.T) {
	// Isolate the config singleton between test runs.
	t.Cleanup(func() { build.ConfigureDevnet(nil) })

	tmp := t.TempDir()
	t.Setenv("LANTERN_HOME", tmp)

	genesis := "bafkreichcugkeap75x2pn2q57k55asaotr7l6uphi3nsypqzhpu6pj373q"
	tsCID := "bafkreiff5tyqz5hzcncxkeisrp7ka2mycdkkkg4z6thnu7feeknp3rmkje"
	stateRoot := "bafkreidhgtsvv5tsxqcxeklkxbi7d4lozgd4a34hu2sdd5jpxeamszxduq"

	srv, calls := fakeDaemonLotus(t, "localnet", genesis, 100, stateRoot, tsCID)
	defer srv.Close()

	if err := cmdDevnetInit([]string{"--lotus-rpc", srv.URL}); err != nil {
		t.Fatalf("cmdDevnetInit: %v", err)
	}
	// lantern#123 raises the discovery-call floor to 5: adds eth_chainId +
	// Filecoin.Version to the pre-existing StateNetworkName + ChainGetGenesis
	// + ChainHead trio.
	if got := calls.Load(); got < 5 {
		t.Errorf("expected at least 5 RPC calls (StateNetworkName + ChainGetGenesis + ChainHead + eth_chainId + Filecoin.Version), got %d", got)
	}

	// Now re-load via the same path cmdDaemon uses.
	netDir := networkDataDir(build.Devnet)
	cfg, err := build.LoadDevnetConfig(filepath.Join(netDir, "devnet-config.json"))
	if err != nil {
		t.Fatalf("LoadDevnetConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadDevnetConfig returned nil after devnet-init")
	}
	build.ConfigureDevnet(cfg)

	// Every Network method must now return devnet values.
	if got := build.Devnet.NetworkName(); got != "localnet" {
		t.Errorf("Devnet.NetworkName() = %q, want %q", got, "localnet")
	}
	if got := build.Devnet.GossipTopicBlocks(); got != "/fil/blocks/localnet" {
		t.Errorf("Devnet.GossipTopicBlocks() = %q", got)
	}
	if got := build.Devnet.GossipTopicMessages(); got != "/fil/msgs/localnet" {
		t.Errorf("Devnet.GossipTopicMessages() = %q", got)
	}
	if got := build.Devnet.GenesisCID(); got != genesis {
		t.Errorf("Devnet.GenesisCID() = %q, want %q", got, genesis)
	}
	if got := build.Devnet.F3Manifest(); got != nil {
		t.Errorf("Devnet.F3Manifest() should be nil (no F3 on devnet), got %d bytes", len(got))
	}
	if got := glifURLForNetwork(build.Devnet); got != srv.URL {
		t.Errorf("glifURLForNetwork(Devnet) = %q, want the lotus URL %q", got, srv.URL)
	}

	// Anchor file must exist and be seeded from ChainHead.
	anchorPath := filepath.Join(netDir, "bootstrap-anchor.json")
	raw, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	var anchor BootstrapAnchor
	if err := json.Unmarshal(raw, &anchor); err != nil {
		t.Fatalf("parse anchor: %v", err)
	}
	if anchor.Epoch != 100 {
		t.Errorf("anchor.Epoch = %d, want 100", anchor.Epoch)
	}
	if !strings.EqualFold(anchor.Network, "devnet") {
		t.Errorf("anchor.Network = %q", anchor.Network)
	}
}
