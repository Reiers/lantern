// End-to-end test for `lantern devnet-init` against a fake JSON-RPC
// server that speaks the three Filecoin methods the command calls. This
// exercises the actual RPC parse paths (glif.Client.StateNetworkName +
// FetchGenesis + FetchHead) without needing a real docker devnet up.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reiers/lantern/build"
)

// fakeLotusRPC serves the three JSON-RPC methods devnet-init calls.
// Everything else 404s so a wrong-method reaches the test as a failure.
func fakeLotusRPC(t *testing.T, networkName, genesisCIDStr string, headEpoch int64, stateRootCIDStr, tsCIDStr string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "Filecoin.StateNetworkName":
			resp["result"] = networkName
		case "Filecoin.ChainGetGenesis":
			resp["result"] = map[string]any{
				"Cids": []map[string]string{{"/": genesisCIDStr}},
			}
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
			// curio-fork docker devnet default: 31415926 (0x1df5e76).
			resp["result"] = "0x1df5e76"
		case "Filecoin.Version":
			// curio-fork docker devnet default: BlockDelay=4.
			resp["result"] = map[string]any{
				"Version":    "1.35.1+devnet-fake",
				"APIVersion": 0x00020300,
				"BlockDelay": 4,
			}
		case "Filecoin.StateNetworkVersion":
			resp["result"] = 28
		case "Filecoin.StateActorCodeCIDs":
			// A minimal custom devnet bundle (just power, enough to prove
			// the discovery + config round-trip).
			resp["result"] = map[string]any{
				"storagepower": map[string]string{"/": "bafk2bzaceal437l2hwjynf3pzvjbtnwlqn7p5gibdf7rkrauk6cnnwez7jtmw"},
				"storageminer": map[string]string{"/": "bafk2bzacebz6pb4vdnl74oekr53zui3mgnonzphlul7uzvsmxcles54f6pebg"},
			}
		default:
			t.Errorf("unexpected RPC method: %s", req.Method)
			resp["error"] = map[string]any{"code": -32601, "message": "unknown method"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestDevnetInit_EndToEnd runs the command against a fake lotus and
// asserts the config file + bootstrap anchor land on disk with the right
// values.
func TestDevnetInit_EndToEnd(t *testing.T) {
	// Divert LANTERN_HOME so networkDataDir() lands in a tempdir.
	tmp := t.TempDir()
	t.Setenv("LANTERN_HOME", tmp)

	// Real-shape CIDs (v1/raw). Content-address of "genesis" and "head".
	genesis := "bafkreichcugkeap75x2pn2q57k55asaotr7l6uphi3nsypqzhpu6pj373q"
	tsCID := "bafkreiff5tyqz5hzcncxkeisrp7ka2mycdkkkg4z6thnu7feeknp3rmkje"
	stateRoot := "bafkreidhgtsvv5tsxqcxeklkxbi7d4lozgd4a34hu2sdd5jpxeamszxduq"

	srv := fakeLotusRPC(t, "localnet", genesis, 42, stateRoot, tsCID)
	defer srv.Close()

	err := cmdDevnetInit([]string{
		"--lotus-rpc", srv.URL,
		"--bootstrap-peers", "/ip4/172.20.0.2/tcp/9090/p2p/12D3KooWDummy",
	})
	if err != nil {
		t.Fatalf("cmdDevnetInit: %v", err)
	}

	netDir := networkDataDir(build.Devnet)
	cfgPath := filepath.Join(netDir, "devnet-config.json")
	anchorPath := filepath.Join(netDir, "bootstrap-anchor.json")

	// devnet-config.json — required fields + bootstrap peers survive.
	cfg, err := build.LoadDevnetConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadDevnetConfig: %v", err)
	}
	if cfg == nil {
		t.Fatalf("devnet-config.json was not written")
	}
	if cfg.NetworkName != "localnet" {
		t.Errorf("NetworkName=%q, want %q", cfg.NetworkName, "localnet")
	}
	if cfg.GenesisCID != genesis {
		t.Errorf("GenesisCID=%q, want %q", cfg.GenesisCID, genesis)
	}
	if cfg.LotusRPC != srv.URL {
		t.Errorf("LotusRPC=%q, want %q", cfg.LotusRPC, srv.URL)
	}
	// lantern#123: EIP-155 chainId + BlockDelay must be captured from
	// the running devnet lotus so the daemon reports the same values.
	if cfg.EthChainID != 31415926 {
		t.Errorf("EthChainID=%d, want 31415926", cfg.EthChainID)
	}
	if cfg.BlockDelaySecs != 4 {
		t.Errorf("BlockDelaySecs=%d, want 4", cfg.BlockDelaySecs)
	}
	if cfg.NetworkVersion != 28 {
		t.Errorf("NetworkVersion=%d, want 28", cfg.NetworkVersion)
	}
	if got := cfg.ActorCodeCIDs["storagepower"]; got != "bafk2bzaceal437l2hwjynf3pzvjbtnwlqn7p5gibdf7rkrauk6cnnwez7jtmw" {
		t.Errorf("ActorCodeCIDs[storagepower]=%q, want the devnet power CID", got)
	}
	if len(cfg.BootstrapPeers) != 1 || !strings.Contains(cfg.BootstrapPeers[0], "12D3KooWDummy") {
		t.Errorf("BootstrapPeers=%v, want the one we passed", cfg.BootstrapPeers)
	}

	// bootstrap-anchor.json — seeded from ChainHead.
	raw, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	var anchor BootstrapAnchor
	if err := json.Unmarshal(raw, &anchor); err != nil {
		t.Fatalf("parse anchor: %v", err)
	}
	if anchor.Epoch != 42 {
		t.Errorf("anchor.Epoch=%d, want 42", anchor.Epoch)
	}
	if anchor.Network != "devnet" {
		t.Errorf("anchor.Network=%q, want %q", anchor.Network, "devnet")
	}
	if anchor.StateRoot != stateRoot {
		t.Errorf("anchor.StateRoot=%q, want %q", anchor.StateRoot, stateRoot)
	}
	if anchor.Instance != 0 {
		t.Errorf("anchor.Instance=%d, want 0 (devnet has no F3)", anchor.Instance)
	}
	if len(anchor.TipSetKey) != 1 || anchor.TipSetKey[0] != tsCID {
		t.Errorf("anchor.TipSetKey=%v, want [%q]", anchor.TipSetKey, tsCID)
	}
}

// TestDevnetInit_RefusesOverwriteWithoutForce mirrors the interactive
// safety: a stale devnet config should not be silently clobbered.
func TestDevnetInit_RefusesOverwriteWithoutForce(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LANTERN_HOME", tmp)

	genesis := "bafkreichcugkeap75x2pn2q57k55asaotr7l6uphi3nsypqzhpu6pj373q"
	tsCID := "bafkreiff5tyqz5hzcncxkeisrp7ka2mycdkkkg4z6thnu7feeknp3rmkje"
	stateRoot := "bafkreidhgtsvv5tsxqcxeklkxbi7d4lozgd4a34hu2sdd5jpxeamszxduq"

	srv := fakeLotusRPC(t, "localnet", genesis, 1, stateRoot, tsCID)
	defer srv.Close()

	// First run: writes the config.
	if err := cmdDevnetInit([]string{"--lotus-rpc", srv.URL}); err != nil {
		t.Fatalf("first init: %v", err)
	}

	// Second run: refuses without --force.
	err := cmdDevnetInit([]string{"--lotus-rpc", srv.URL})
	if err == nil {
		t.Fatalf("expected refusal on second devnet-init without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal error should hint at --force, got: %v", err)
	}

	// Third run: --force succeeds.
	if err := cmdDevnetInit([]string{"--lotus-rpc", srv.URL, "--force"}); err != nil {
		t.Fatalf("--force run should succeed: %v", err)
	}
}
