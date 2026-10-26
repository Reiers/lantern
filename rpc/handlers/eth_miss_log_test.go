package handlers

import (
	"context"
	"testing"
	"time"
)

func TestEthMissLogger_RateLimitsPerKey(t *testing.T) {
	now := time.Unix(1000, 0)
	type line struct {
		method, to, sel, reason string
		sup                     int
	}
	var got []line
	m := &ethMissLogger{
		last:       map[string]time.Time{},
		suppressed: map[string]int{},
		now:        func() time.Time { return now },
		emit: func(method, to, sel, reason string, _ bool, sup int) {
			got = append(got, line{method, to, sel, reason, sup})
		},
	}
	for i := 0; i < 5; i++ {
		m.record("eth_call", "0xABC", "0x12345678", "local exec fault: x", false)
	}
	m.record("eth_call", "0xabc", "0x87654321", "local exec fault: x", false) // other selector
	if len(got) != 2 {
		t.Fatalf("want 2 lines in window, got %d: %+v", len(got), got)
	}
	if got[0].to != "0xabc" || got[0].sel != "0x12345678" {
		t.Fatalf("bad first line %+v", got[0])
	}
	now = now.Add(ethMissLogInterval + time.Second)
	m.record("eth_call", "0xabc", "0x12345678", "local exec fault: x", false)
	if len(got) != 3 || got[2].sup != 4 {
		t.Fatalf("want 3rd line with 4 suppressed, got %+v", got)
	}
}

func TestEthSelector(t *testing.T) {
	for in, want := range map[string]string{
		"0xA9059CBB000000": "0xa9059cbb",
		"a9059cbb":         "0xa9059cbb",
		"0x1234":           "",
		"":                 "",
	} {
		if got := ethSelector(in); got != want {
			t.Fatalf("ethSelector(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEthAddressToFilecoinAddress(t *testing.T) {
	c := &ChainAPI{}
	id, err := c.EthAddressToFilecoinAddress(context.Background(), "0xff00000000000000000000000000000000000400")
	if err != nil || id.String() != "f01024" {
		t.Fatalf("masked id: got %v %v", id, err)
	}
	d, err := c.EthAddressToFilecoinAddress(context.Background(), "0xd44e13E90f36ad26B5E2B7922c5100e673Ee4Bee")
	if err != nil || d.Protocol() != 4 {
		t.Fatalf("f410: got %v %v", d, err)
	}
	if _, err := c.EthAddressToFilecoinAddress(context.Background(), "0x1234"); err == nil {
		t.Fatal("want error for short address")
	}
}
