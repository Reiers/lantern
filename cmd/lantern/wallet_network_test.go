package main

import (
	"testing"

	addr "github.com/filecoin-project/go-address"

	"github.com/Reiers/lantern/build"
)

// #148: wallet subcommands must render addresses with the selected
// network's prefix.
func TestApplyAddressNetworkPrefix(t *testing.T) {
	prev := addr.CurrentNetwork
	t.Cleanup(func() { addr.CurrentNetwork = prev })

	a, err := addr.NewIDAddress(1234)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		net  build.Network
		want string
	}{
		{build.Mainnet, "f01234"},
		{build.Calibration, "t01234"},
		{build.Devnet, "t01234"},
	}
	for _, c := range cases {
		applyAddressNetwork(c.net)
		if got := a.String(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.net, got, c.want)
		}
	}
}

func TestParseAttoFIL(t *testing.T) {
	b, err := parseAttoFIL("1500000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "1500000000000000000" {
		t.Fatalf("got %s", got)
	}
	if _, err := parseAttoFIL("not-a-number"); err == nil {
		t.Fatal("expected error for garbage input")
	}
}
