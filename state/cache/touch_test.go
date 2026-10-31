package cache

import (
	"testing"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// TestShouldTouchCoalesces: repeat reads of one block within touchInterval
// produce a single LRU bump (one Badger write), distinct blocks each get
// their own.
func TestShouldTouchCoalesces(t *testing.T) {
	s := &Store{}
	mk := func(b string) cid.Cid {
		c, err := cid.Prefix{Version: 1, Codec: cid.Raw, MhType: mh.SHA2_256, MhLength: -1}.Sum([]byte(b))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := mk("a"), mk("b")
	if !s.shouldTouch(a) {
		t.Fatal("first read of a should bump")
	}
	for i := 0; i < 100; i++ {
		if s.shouldTouch(a) {
			t.Fatal("repeat read of a within touchInterval should not bump")
		}
	}
	if !s.shouldTouch(b) {
		t.Fatal("first read of b should bump")
	}
	s.touched[a.KeyString()] -= touchInterval
	if !s.shouldTouch(a) {
		t.Fatal("a should bump again once touchInterval has passed")
	}
}
