// Tests for the ChainExchange responder (issue #17).
//
// - notFoundResponse bytes are validated against a reference encoding
//   (the exact bytes that Lotus's cbor_gen.go would produce for
//   Response{Status: 201, ErrorMessage: "", Chain: nil}).
// - Wire round-trip: A dials B's /fil/chain/xchg/0.0.1 stream, sends a
//   fake request, expects B to write back notFoundResponse and close
//   the stream. Stats counter increments on both success and failure.

package chainxchg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestNotFoundResponse_ByteLayout verifies the hardcoded response is the
// CBOR encoding of {Status: 201, ErrorMessage: "", Chain: []}:
//
//	0x83 = array, length 3
//	0x18 0xC9 = uint, 1-byte payload, value 201
//	0x60 = text string, length 0 (empty)
//	0x80 = array, length 0 (empty)
func TestNotFoundResponse_ByteLayout(t *testing.T) {
	want := []byte{0x83, 0x18, 0xC9, 0x60, 0x80}
	if !bytes.Equal(notFoundResponse, want) {
		t.Errorf("notFoundResponse = % x, want % x", notFoundResponse, want)
	}
}

// TestStats_FreshIsZero: a fresh service starts with zero counters.
func TestStats_FreshIsZero(t *testing.T) {
	h, err := libp2p.New()
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	defer h.Close()
	s := NewService(h)
	got := s.Stats()
	if got.Received != 0 || got.Rejected != 0 {
		t.Errorf("fresh stats = %+v, want zero", got)
	}
}

// TestRegister_Idempotent: Register can be called multiple times
// without panicking.
func TestRegister_Idempotent(t *testing.T) {
	h, err := libp2p.New()
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	defer h.Close()
	s := NewService(h)
	s.Register()
	s.Register()
}

// TestService_WireRoundTrip: real libp2p hosts. A opens a stream to B
// on ProtocolID, writes a fake request, closes the write side, then
// reads the response. B must reply with notFoundResponse and B's
// Received counter must be 1.
func TestService_WireRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	hA, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("hA: %v", err)
	}
	defer hA.Close()
	hB, err := libp2p.New()
	if err != nil {
		t.Fatalf("hB: %v", err)
	}
	defer hB.Close()

	svcB := NewService(hB)
	svcB.Register()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hA.Peerstore().AddAddrs(hB.ID(), hB.Addrs(), time.Hour)
	if err := hA.Connect(ctx, peer.AddrInfo{ID: hB.ID(), Addrs: hB.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	str, err := hA.NewStream(ctx, hB.ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	// Write a plausible-looking but garbage request blob. The MVP
	// responder doesn't parse it, just drains.
	fakeReq := []byte{0x83, 0x80, 0x00, 0x00}
	if _, err := str.Write(fakeReq); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := str.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	got, err := io.ReadAll(str)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !bytes.Equal(got, notFoundResponse) {
		t.Errorf("response = % x, want % x", got, notFoundResponse)
	}

	// Wait briefly for B's Received counter to bump (handleStream runs
	// in a goroutine).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if svcB.Stats().Received == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := svcB.Stats().Received; got != 1 {
		t.Errorf("Stats().Received = %d, want 1", got)
	}
	if got := svcB.Stats().Rejected; got != 0 {
		t.Errorf("Stats().Rejected = %d, want 0", got)
	}
}

// TestDrainBounded_BoundEnforced: reads beyond the max are rejected
// with an error so a malicious peer can't OOM us.
func TestDrainBounded_BoundEnforced(t *testing.T) {
	infinite := &repeatReader{b: 0xff}
	err := drainBounded(infinite, 1024)
	if err == nil {
		t.Fatal("expected error on oversized input")
	}
	if !errors.Is(err, errors.New("request exceeded max bytes")) && err.Error() != "request exceeded max bytes" {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestDrainBounded_EOF: a clean EOF returns nil.
func TestDrainBounded_EOF(t *testing.T) {
	buf := bytes.NewBuffer([]byte{0x83, 0x80, 0x80, 0x80})
	if err := drainBounded(buf, 1024); err != nil {
		t.Errorf("clean EOF returned %v, want nil", err)
	}
}

// repeatReader emits the same byte forever; used to test the bounded
// drain.
type repeatReader struct{ b byte }

func (r *repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}
