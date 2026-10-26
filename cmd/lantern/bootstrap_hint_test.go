package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
)

func TestDefaultBootstrapQuorum(t *testing.T) {
	for _, n := range []build.Network{build.Mainnet, build.Calibration} {
		if got := defaultBootstrapQuorum(n); got != 3 {
			t.Errorf("%s: default quorum = %d, want 3 distinct operators", n, got)
		}
	}
	if got := defaultBootstrapQuorum(build.Devnet); got != 1 {
		t.Errorf("devnet: default quorum = %d, want 1", got)
	}
}

func TestQuorumFailureHint_NotReachedListsFailuresAndFlags(t *testing.T) {
	res := &bootstrap.QuorumResult{
		Required: 3,
		Results: []bootstrap.SourceResult{
			{Name: "forest:https://api.chain.love/rpc/v1", Operator: "chain.love", Counted: true},
			{Name: "libp2p:12D3KooWGnkd", Operator: "chainsafe-fil.io", Counted: true},
			{Name: "libp2p:12D3KooWAke3", Operator: "devtty.eu", Counted: true, Error: errors.New("dial timeout")},
		},
	}
	err := fmt.Errorf("%w: short", bootstrap.ErrQuorumNotReached)
	got := quorumFailureHint(res, err, bootstrapParams{Quorum: 3, Network: build.Mainnet})
	for _, want := range []string{
		"2 of 3 required distinct operators answered (chain.love, chainsafe-fil.io)",
		"libp2p:12D3KooWAke3 {devtty.eu}: dial timeout",
		"--peer https://",
		"--no-libp2p",
		"--bootstrap-quorum 2",
		"lantern doctor --network mainnet",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hint missing %q:\n%s", want, got)
		}
	}
}

func TestQuorumFailureHint_ConflictDoesNotSuggestLoweringQuorum(t *testing.T) {
	err := fmt.Errorf("%w: instance 1", bootstrap.ErrConflict)
	got := quorumFailureHint(&bootstrap.QuorumResult{Required: 3}, err, bootstrapParams{Quorum: 3, Network: build.Calibration})
	if !strings.Contains(got, "DIFFERENT finalized tipsets") {
		t.Fatalf("conflict hint missing headline:\n%s", got)
	}
	if strings.Contains(got, "--bootstrap-quorum 2") {
		t.Fatalf("conflict hint must not suggest lowering the quorum:\n%s", got)
	}
}

func TestMissingAnchorError_MentionsInitAndOverride(t *testing.T) {
	err := missingAnchorError("/tmp/x/mainnet", build.Mainnet)
	msg := err.Error()
	for _, want := range []string{"lantern init --network mainnet", "lantern doctor", "--allow-rpc-anchor", "/tmp/x/mainnet/bootstrap-anchor.json"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
}

func TestQuorumFailureHint_InsufficientOperators(t *testing.T) {
	err := fmt.Errorf("%w: have 8 countable sources from 4 distinct operators, need 5 operators", bootstrap.ErrInsufficientSources)
	got := quorumFailureHint(&bootstrap.QuorumResult{Required: 5, CountableOperators: 4}, err, bootstrapParams{Quorum: 5, Network: build.Mainnet})
	for _, want := range []string{"only 4 distinct operators are configured for mainnet, but 5 must agree", "--peer https://", "--bootstrap-quorum 4"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint missing %q:\n%s", want, got)
		}
	}
}
