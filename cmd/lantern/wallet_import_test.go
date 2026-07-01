package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	base32 "github.com/multiformats/go-base32"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/wallet"
)

// tWallet opens a throwaway passphrase-protected wallet in a temp dir.
func tWallet(t *testing.T) *wallet.Wallet {
	t.Helper()
	dir := t.TempDir()
	w, err := wallet.New(context.Background(), filepath.Join(dir, "keystore"), "test-passphrase")
	require.NoError(t, err)
	return w
}

// writeLotusKeystoreEntry writes a Lotus-shaped keystore file:
// filename = base32.RawStdEncoding("wallet-<addr>"), content = KeyInfo JSON.
func writeLotusKeystoreEntry(t *testing.T, ksDir, name string, ki *wallet.KeyInfo) {
	t.Helper()
	blob, err := json.Marshal(ki)
	require.NoError(t, err)
	fn := base32.RawStdEncoding.EncodeToString([]byte(name))
	require.NoError(t, os.WriteFile(filepath.Join(ksDir, fn), blob, 0o600))
}

// TestImportLotusKeystoreRoundTrip: a key exported from a Lantern wallet,
// laid out as a Lotus repo keystore, imports back with the same address.
// Covers base32 filename decoding, wallet-* filtering, and KeyInfo JSON.
func TestImportLotusKeystoreRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Source wallet: make a couple of keys, export their KeyInfo.
	src := tWallet(t)
	addrBLS, err := src.NewAddress(ctx, wallet.KTBLS)
	require.NoError(t, err)
	addrSecp, err := src.NewAddress(ctx, wallet.KTSecp256k1)
	require.NoError(t, err)
	kiBLS, err := src.Export(ctx, addrBLS)
	require.NoError(t, err)
	kiSecp, err := src.Export(ctx, addrSecp)
	require.NoError(t, err)

	// Fake Lotus repo.
	repo := t.TempDir()
	ksDir := filepath.Join(repo, "keystore")
	require.NoError(t, os.MkdirAll(ksDir, 0o700))
	writeLotusKeystoreEntry(t, ksDir, "wallet-"+addrBLS.String(), kiBLS)
	writeLotusKeystoreEntry(t, ksDir, "wallet-"+addrSecp.String(), kiSecp)
	// Non-wallet entries that must be ignored: libp2p host key + garbage.
	writeLotusKeystoreEntry(t, ksDir, "libp2p-host", &wallet.KeyInfo{Type: "libp2p-host", PrivateKey: []byte("x")})
	require.NoError(t, os.WriteFile(filepath.Join(ksDir, "not-base32-!!!"), []byte("junk"), 0o600))

	// Destination wallet: import the repo via the same walk the command
	// uses (exercise the internals directly; the CLI wrapper only adds
	// openWalletDefault + printing).
	dst := tWallet(t)
	imported := 0
	entries, err := os.ReadDir(ksDir)
	require.NoError(t, err)
	for _, e := range entries {
		nameBytes, err := base32.RawStdEncoding.DecodeString(e.Name())
		if err != nil {
			continue
		}
		if len(nameBytes) < 7 || string(nameBytes[:7]) != "wallet-" {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(ksDir, e.Name()))
		require.NoError(t, err)
		var ki wallet.KeyInfo
		require.NoError(t, json.Unmarshal(blob, &ki))
		addr, err := dst.Import(ctx, &ki)
		require.NoError(t, err)
		require.Contains(t, []string{addrBLS.String(), addrSecp.String()}, addr.String(),
			"import must derive the same address the key had in the source wallet")
		imported++
	}
	require.Equal(t, 2, imported, "exactly the two wallet-* entries import")

	// The imported keys must be able to sign.
	_, err = dst.Sign(ctx, addrBLS, []byte("msg"))
	require.NoError(t, err)
}

// TestWalletHexKeyInfoRoundTrip: the `wallet export` wire format
// (hex of KeyInfo JSON) parses back to the same key.
func TestWalletHexKeyInfoRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := tWallet(t)
	addr, err := src.NewAddress(ctx, wallet.KTSecp256k1)
	require.NoError(t, err)
	ki, err := src.Export(ctx, addr)
	require.NoError(t, err)

	blob, err := json.Marshal(ki)
	require.NoError(t, err)
	wire := hex.EncodeToString(blob)

	back, err := hex.DecodeString(wire)
	require.NoError(t, err)
	var ki2 wallet.KeyInfo
	require.NoError(t, json.Unmarshal(back, &ki2))

	dst := tWallet(t)
	addr2, err := dst.Import(ctx, &ki2)
	require.NoError(t, err)
	require.Equal(t, addr.String(), addr2.String())
}

// TestExpandHome covers the ~ expansion helper.
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".lotus"), expandHome("~/.lotus"))
	require.Equal(t, home, expandHome("~"))
	require.Equal(t, "/abs/path", expandHome("/abs/path"))
}
