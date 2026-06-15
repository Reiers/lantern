package handlers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	gocrypto "github.com/filecoin-project/go-crypto"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/ethtx"
	"github.com/Reiers/lantern/chain/types"
)

// capturePublisher records published messages.
type capturePublisher struct {
	published []*types.SignedMessage
}

func (p *capturePublisher) Publish(_ context.Context, sm *types.SignedMessage) (cid.Cid, error) {
	p.published = append(p.published, sm)
	return sm.Cid(), nil
}
func (p *capturePublisher) Pending() []*types.SignedMessage { return nil }

// buildRealSignedTx builds + signs a real EIP-1559 tx with a fresh key and
// returns the 0x raw bytes + the expected eth tx hash.
func buildRealSignedTx(t *testing.T) (rawHex, ethHash string) {
	t.Helper()
	sk, err := gocrypto.GenerateKey()
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	to := [20]byte{0xac, 0xc0, 0xa0, 0x11}
	tx := &ethtx.Eth1559Tx{
		ChainID: 314159, Nonce: 42, To: &to, Value: big.NewInt(0),
		MaxFeePerGas: big.NewInt(1_500_000_000), MaxPriorityFeePerGas: big.NewInt(100_000),
		GasLimit: 5_000_000, Input: []byte{0xde, 0xad},
	}
	hash, err := tx.SigningHash()
	if err != nil {
		t.Fatalf("signinghash: %v", err)
	}
	sig, err := gocrypto.Sign(sk, hash)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := tx.SetSignature(sig); err != nil {
		t.Fatalf("setsig: %v", err)
	}
	signed, err := tx.EncodeSigned()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	h, err := tx.TxHash()
	if err != nil {
		t.Fatalf("txhash: %v", err)
	}
	return "0x" + hex.EncodeToString(signed), "0x" + hex.EncodeToString(h[:])
}

// Send with no mpool + no bridge errors.
func TestEthSendRawTransaction_NoMpoolNoBridge(t *testing.T) {
	c := newCAPI()
	if _, err := c.EthSendRawTransaction(context.Background(), "0xdeadbeef"); err == nil {
		t.Fatal("expected error with no mpool and no bridge")
	}
}

// Undecodable tx with a bridge -> bridge fallback.
func TestEthSendRawTransaction_BridgeFallbackOnUndecodable(t *testing.T) {
	c := newCAPI()
	rb := &recordingBridge{reply: map[string]json.RawMessage{
		"eth_sendRawTransaction": json.RawMessage(`"0xabc123"`),
	}}
	c.Bridge = rb
	out, err := c.EthSendRawTransaction(context.Background(), "0xf801") // legacy-looking
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if out != "0xabc123" {
		t.Fatalf("expected bridge value, got %s", out)
	}
	if len(rb.methods) != 1 || rb.methods[0] != "eth_sendRawTransaction" {
		t.Fatalf("expected bridge fallback, got %v", rb.methods)
	}
}

// A real signed tx published through a capture mpool gets indexed under its
// eth hash and returns that hash (NO bridge call).
func TestEthSendRawTransaction_LocalPublishIndexes(t *testing.T) {
	c := newCAPI()
	pub := &capturePublisher{}
	c.Mpool = pub
	rb := &recordingBridge{}
	c.Bridge = rb // present, but must NOT be used

	raw, wantHash := buildRealSignedTx(t)
	out, err := c.EthSendRawTransaction(context.Background(), raw)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.EqualFold(out, wantHash) {
		t.Fatalf("returned hash %s, want %s", out, wantHash)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected 1 published msg, got %d", len(pub.published))
	}
	if len(rb.methods) != 0 {
		t.Fatalf("bridge must not be called on local publish, got %v", rb.methods)
	}
	if _, ok := c.sentTx().get(strings.ToLower(wantHash)); !ok {
		t.Fatal("tx not indexed under its eth hash")
	}
}

// Receipt for an unknown hash with no header store -> bridge.
func TestEthGetTransactionReceipt_UnknownFallsBack(t *testing.T) {
	c := newCAPI() // no HeaderStore
	rb := &recordingBridge{reply: map[string]json.RawMessage{
		"eth_getTransactionReceipt": json.RawMessage(`{"status":"0x1"}`),
	}}
	c.Bridge = rb
	out, err := c.EthGetTransactionReceipt(context.Background(),
		"0x1111111111111111111111111111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok || m["status"] != "0x1" {
		t.Fatalf("expected bridged receipt, got %#v", out)
	}
}

// sentTxIndex put/get + idempotency.
func TestSentTxIndex_PutGet(t *testing.T) {
	idx := &sentTxIndex{}
	c1, _ := cid.Parse("bafy2bzacecnamqgqmifpluoeldx7zzglxcljo6oja4vrmtj7432rphldpdmm2")
	idx.put("0xaaa", c1)
	if got, ok := idx.get("0xaaa"); !ok || got != c1 {
		t.Fatal("put/get failed")
	}
	if _, ok := idx.get("0xbbb"); ok {
		t.Fatal("unexpected hit")
	}
	idx.put("0xaaa", c1) // idempotent
	if len(idx.order) != 1 {
		t.Fatalf("dup put grew order to %d", len(idx.order))
	}
}

func TestUint64Hex(t *testing.T) {
	for in, want := range map[uint64]string{0: "0", 1: "1", 255: "ff", 256: "100"} {
		if got := uint64Hex(in); got != want {
			t.Fatalf("uint64Hex(%d)=%s want %s", in, got, want)
		}
	}
}
