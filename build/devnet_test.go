// Unit tests for the runtime devnet config (build/devnet.go).
package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevnetConfig_SaveThenLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devnet", "devnet-config.json")
	cfg := &DevnetConfig{
		NetworkName:    "localnet",
		GenesisCID:     "bafy2bzacecdevnetgenesisstubforroundtriptest",
		LotusRPC:       "http://127.0.0.1:1234/rpc/v1",
		BootstrapPeers: []string{"/ip4/172.20.0.2/tcp/9090/p2p/12D3KooWDummy"},
		GenesisTime:    1700000000,
		BlockDelaySecs: 4,
	}
	if err := SaveDevnetConfig(path, cfg); err != nil {
		t.Fatalf("SaveDevnetConfig: %v", err)
	}

	loaded, err := LoadDevnetConfig(path)
	if err != nil {
		t.Fatalf("LoadDevnetConfig: %v", err)
	}
	if loaded == nil {
		t.Fatalf("LoadDevnetConfig returned nil after Save")
	}
	if loaded.NetworkName != cfg.NetworkName ||
		loaded.GenesisCID != cfg.GenesisCID ||
		loaded.LotusRPC != cfg.LotusRPC ||
		loaded.GenesisTime != cfg.GenesisTime ||
		loaded.BlockDelaySecs != cfg.BlockDelaySecs {
		t.Errorf("roundtrip mismatch:\n got %+v\nwant %+v", loaded, cfg)
	}
	if len(loaded.BootstrapPeers) != 1 || loaded.BootstrapPeers[0] != cfg.BootstrapPeers[0] {
		t.Errorf("bootstrap peers roundtrip mismatch: got %v want %v", loaded.BootstrapPeers, cfg.BootstrapPeers)
	}
}

func TestDevnetConfig_LoadMissingIsNilNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devnet-config.json")
	loaded, err := LoadDevnetConfig(path)
	if err != nil {
		t.Fatalf("LoadDevnetConfig on missing file: unexpected err %v", err)
	}
	if loaded != nil {
		t.Fatalf("expected nil config for missing file, got %+v", loaded)
	}
}

func TestDevnetConfig_LoadRejectsMissingRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devnet-config.json")
	// Missing GenesisCID.
	if err := os.WriteFile(path, []byte(`{"networkName":"localnet","lotusRPC":"http://x"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadDevnetConfig(path)
	if err == nil {
		t.Fatalf("expected error for config missing genesisCID, got nil")
	}
}

func TestDevnetConfig_ConfigureAndGet(t *testing.T) {
	t.Cleanup(func() { ConfigureDevnet(nil) })
	if IsDevnetConfigured() {
		t.Fatalf("expected IsDevnetConfigured=false at test start")
	}
	cfg := &DevnetConfig{NetworkName: "localnet", GenesisCID: "bafy-stub"}
	ConfigureDevnet(cfg)
	if !IsDevnetConfigured() {
		t.Fatalf("expected IsDevnetConfigured=true after ConfigureDevnet")
	}
	got := GetDevnetConfig()
	if got == nil || got.NetworkName != "localnet" {
		t.Errorf("GetDevnetConfig round-trip: got %+v", got)
	}
	// Mutating the returned copy must not affect the singleton
	// (defensive copy — protects callers on the hot path).
	got.NetworkName = "mutated"
	again := GetDevnetConfig()
	if again.NetworkName != "localnet" {
		t.Errorf("singleton was mutated via returned pointer: %+v", again)
	}
	// Nil clears.
	ConfigureDevnet(nil)
	if IsDevnetConfigured() {
		t.Errorf("expected IsDevnetConfigured=false after Configure(nil)")
	}
}

func TestDevnet_MethodsPanicWhenUnconfigured(t *testing.T) {
	t.Cleanup(func() { ConfigureDevnet(nil) })
	ConfigureDevnet(nil)

	for _, tc := range []struct {
		name string
		fn   func()
	}{
		{"NetworkName", func() { _ = Devnet.NetworkName() }},
		{"GenesisCID", func() { _ = Devnet.GenesisCID() }},
		{"GossipTopicBlocks", func() { _ = Devnet.GossipTopicBlocks() }},
		{"GossipTopicMessages", func() { _ = Devnet.GossipTopicMessages() }},
		{"BootstrapPeers", func() { _ = Devnet.BootstrapPeers() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected panic for Devnet.%s without config", tc.name)
				}
				msg, _ := r.(string)
				if !strings.Contains(msg, "ConfigureDevnet") && !strings.Contains(msg, "devnet-init") {
					t.Errorf("panic message should point at the fix (ConfigureDevnet / devnet-init): %q", msg)
				}
			}()
			tc.fn()
		})
	}
}

func TestDevnet_MethodsAfterConfigure(t *testing.T) {
	t.Cleanup(func() { ConfigureDevnet(nil) })
	ConfigureDevnet(&DevnetConfig{
		NetworkName:    "localnet",
		GenesisCID:     "bafy-stub",
		BootstrapPeers: []string{"/ip4/1.2.3.4/tcp/1"},
	})
	if got := Devnet.NetworkName(); got != "localnet" {
		t.Errorf("NetworkName=%q", got)
	}
	if got := Devnet.GossipTopicBlocks(); got != "/fil/blocks/localnet" {
		t.Errorf("GossipTopicBlocks=%q", got)
	}
	if got := Devnet.GossipTopicMessages(); got != "/fil/msgs/localnet" {
		t.Errorf("GossipTopicMessages=%q", got)
	}
	if got := Devnet.GenesisCID(); got != "bafy-stub" {
		t.Errorf("GenesisCID=%q", got)
	}
	if got := Devnet.BootstrapPeers(); len(got) != 1 || got[0] != "/ip4/1.2.3.4/tcp/1" {
		t.Errorf("BootstrapPeers=%v", got)
	}
	if got := Devnet.F3Manifest(); got != nil {
		t.Errorf("F3Manifest for devnet should be nil, got %d bytes", len(got))
	}
	if !Devnet.Valid() {
		t.Errorf("Devnet.Valid() must be true")
	}
}

func TestDevnet_SaveConfigAtomicRename(t *testing.T) {
	// Sanity check that the tmp+rename dance leaves no orphan.
	dir := t.TempDir()
	path := filepath.Join(dir, "devnet-config.json")
	if err := SaveDevnetConfig(path, &DevnetConfig{NetworkName: "localnet", GenesisCID: "bafy-x"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("orphan .tmp left behind: %s", e.Name())
		}
	}
}
