package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reiers/lantern/build"
)

// --network is the one network flag. These pin the mapping for init /
// doctor / repair, including the backwards-compatible paths.
func TestResolveNetworkFlags(t *testing.T) {
	cases := []struct {
		name       string
		network    string
		filNetwork string
		wantNet    build.Network
		wantF3     string
		wantErr    bool
	}{
		{name: "defaults to mainnet", wantNet: build.Mainnet, wantF3: "filecoin"},
		{name: "--network calibration", network: "calibration", wantNet: build.Calibration, wantF3: "calibrationnet2"},
		{name: "--network mainnet", network: "mainnet", wantNet: build.Mainnet, wantF3: "filecoin"},
		{name: "--filecoin-network calibration (alias)", filNetwork: "calibration", wantNet: build.Calibration, wantF3: "calibrationnet2"},
		{name: "both flags agree", network: "calibration", filNetwork: "calibration", wantNet: build.Calibration, wantF3: "calibrationnet2"},
		{name: "both flags conflict", network: "calibration", filNetwork: "mainnet", wantErr: true},
		{name: "legacy F3 name filecoin + calibration alias", network: "filecoin", filNetwork: "calibration", wantNet: build.Calibration, wantF3: "calibrationnet2"},
		{name: "legacy explicit F3 override kept", network: "calibrationnet2", filNetwork: "calibration", wantNet: build.Calibration, wantF3: "calibrationnet2"},
		{name: "invalid alias value", filNetwork: "nope", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, f := tc.network, tc.filNetwork
			got, err := resolveNetworkFlags(&n, &f)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got network %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantNet {
				t.Errorf("network = %q, want %q", got, tc.wantNet)
			}
			if n != tc.wantF3 {
				t.Errorf("F3 name = %q, want %q", n, tc.wantF3)
			}
			if f != string(tc.wantNet) {
				t.Errorf("filNetwork out = %q, want %q", f, tc.wantNet)
			}
		})
	}
}

// F5: a calibration init must never suggest a bare `lantern daemon`,
// which would start mainnet.
func TestInitNextStepsCarryNetwork(t *testing.T) {
	for _, n := range []build.Network{build.Calibration, build.Mainnet} {
		lines := initNextSteps(n)
		if len(lines) == 0 {
			t.Fatalf("%s: no next steps", n)
		}
		for _, l := range lines {
			cmd := strings.TrimSpace(strings.SplitN(l, "#", 2)[0])
			if !strings.HasSuffix(cmd, "--network "+string(n)) {
				t.Errorf("%s: next step %q does not carry --network %s", n, cmd, n)
			}
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "lantern daemon --network "+string(n)) {
			t.Errorf("%s: missing daemon step in %q", n, joined)
		}
	}
}

// lantern info must report FULLNODE_API_INFO for the address the daemon
// actually bound (for Curio in Docker: the docker bridge IP).
func TestInfoReportsPersistedListen(t *testing.T) {
	dir := t.TempDir()
	if got := readRPCListen(dir); got != defaultListen {
		t.Fatalf("no persisted listen: got %q, want %q", got, defaultListen)
	}
	if err := writeRPCListen(dir, "172.17.0.1:1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, rpcListenFile)); err != nil {
		t.Fatal(err)
	}
	listen := readRPCListen(dir)
	if listen != "172.17.0.1:1234" {
		t.Fatalf("persisted listen = %q", listen)
	}
	if got := apiMultiaddr(listen); got != "/ip4/172.17.0.1/tcp/1234/http" {
		t.Errorf("apiMultiaddr(%q) = %q", listen, got)
	}
	if got := apiMultiaddr("0.0.0.0:2345"); got != "/ip4/127.0.0.1/tcp/2345/http" {
		t.Errorf("apiMultiaddr(0.0.0.0) = %q", got)
	}
}
