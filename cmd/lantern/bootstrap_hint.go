// F10/F11: operator-aware bootstrap quorum defaults and failure guidance.

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Reiers/lantern/build"
	"github.com/Reiers/lantern/chain/bootstrap"
)

// defaultBootstrapQuorum is the default number of distinct operators that
// must agree on the F3 finality (F10). Mainnet and calibration share the
// same bar: 3 operators, zero conflicting answers. Counting operators
// instead of endpoints is what lets the number drop from "5 sources"
// without weakening the bar: the old 5-source mainnet default could be met
// by endpoints of just two operators (Glif is served by chain.love, and
// chain.love also runs a bootstrap peer). Devnet is single-endpoint by
// design.
func defaultBootstrapQuorum(n build.Network) int {
	if n == build.Devnet {
		return 1
	}
	return bootstrap.DefaultQuorum
}

// countOperators returns the number of distinct operators among the
// sources that can count toward the quorum.
func countOperators(ss []bootstrap.Source, countGateway bool) int {
	ops := map[string]bool{}
	for _, s := range ss {
		if s.Kind() == bootstrap.KindLanternGateway && !countGateway {
			continue
		}
		ops[bootstrap.OperatorKey(s)] = true
	}
	return len(ops)
}

// quorumFailureHint explains a failed bootstrap quorum in operator terms:
// which sources failed, how many distinct operators answered, and which
// flags change the outcome (F10). Shared by init, doctor and repair via
// runBootstrapQuorum.
func quorumFailureHint(res *bootstrap.QuorumResult, err error, p bootstrapParams) string {
	var b strings.Builder
	network := p.Network
	if network == "" {
		network = build.Mainnet
	}
	netFlag := "--network " + string(network)
	fmt.Fprintln(&b)
	if errors.Is(err, bootstrap.ErrConflict) {
		fmt.Fprintln(&b, "  ✗ Sources returned DIFFERENT finalized tipsets for the same F3 instance.")
		fmt.Fprintln(&b, "    This is not a connectivity problem, and Lantern will not pick a side:")
		fmt.Fprintln(&b, "    F3 finality is unique per instance, so one of these sources is wrong,")
		fmt.Fprintln(&b, "    broken, or on another network. Lowering --bootstrap-quorum does not help.")
		fmt.Fprintln(&b, "    What you can do:")
		fmt.Fprintln(&b, "      - compare against a node you run yourself: --peer https://your-node:1234/rpc/v1")
		fmt.Fprintf(&b, "      - re-run later: lantern doctor %s\n", netFlag)
		fmt.Fprintln(&b, "      - report the disagreeing source: https://github.com/Reiers/lantern/issues")
		return b.String()
	}
	if errors.Is(err, bootstrap.ErrInsufficientSources) && res != nil {
		fmt.Fprintf(&b, "  Why: only %d distinct operators are configured for %s, but %d must agree.\n",
			res.CountableOperators, network, p.Quorum)
		fmt.Fprintln(&b, "  Nothing was probed. What you can do:")
		fmt.Fprintln(&b, "    - add finality sources run by other operators (each counts once):")
		fmt.Fprintln(&b, "        --peer https://your-lotus-or-forest:1234/rpc/v1   (repeatable; URL|TOKEN for auth)")
		if res.CountableOperators > 0 {
			fmt.Fprintf(&b, "    - or ask for what exists: --bootstrap-quorum %d (or omit it for the default %d)\n",
				res.CountableOperators, defaultBootstrapQuorum(network))
		}
		return b.String()
	}
	if res != nil {
		reach := map[string]bool{}
		for _, s := range res.Results {
			if s.OK() && s.Counted {
				reach[s.Operator] = true
			}
		}
		ops := make([]string, 0, len(reach))
		for o := range reach {
			ops = append(ops, o)
		}
		sort.Strings(ops)
		list := strings.Join(ops, ", ")
		if list == "" {
			list = "none"
		}
		fmt.Fprintf(&b, "  Why: %d of %d required distinct operators answered (%s).\n", len(ops), p.Quorum, list)
		if failed := res.FailedSources(); len(failed) > 0 {
			fmt.Fprintln(&b, "  Sources that failed:")
			for _, s := range failed {
				fmt.Fprintf(&b, "    ✗ %s {%s}: %s\n", s.Name, s.Operator, truncateErr(s.Error.Error(), 100))
			}
		}
	}
	fmt.Fprintln(&b, "  What you can do:")
	fmt.Fprintf(&b, "    - re-run (public endpoints flake): lantern doctor %s\n", netFlag)
	fmt.Fprintln(&b, "    - add a finality source you trust (it counts as its own operator):")
	fmt.Fprintln(&b, "        --peer https://your-lotus-or-forest:1234/rpc/v1   (repeatable; URL|TOKEN for auth)")
	if !p.NoLibp2p {
		fmt.Fprintln(&b, "    - libp2p blocked on this network? the RPC sources are HTTPS; --no-libp2p skips the libp2p dials")
	}
	if p.Quorum > 1 {
		fmt.Fprintln(&b, "    - accept fewer operators (each unit is one distinct operator; 1 = trust a")
		fmt.Fprintf(&b, "      single operator): --bootstrap-quorum %d\n", p.Quorum-1)
	}
	return b.String()
}

// missingAnchorError is the F11 refusal: the daemon no longer silently
// falls back to the weaker gateway+RPC anchor when init never wrote one.
func missingAnchorError(netDir string, network build.Network) error {
	return fmt.Errorf("no trust anchor at %s: `lantern init --network %s` has not completed.\n"+
		"  Refusing to boot from a gateway+RPC head, which is weaker than init's %d-operator quorum (F11).\n"+
		"  Fix: run `lantern init --network %s --no-wallet` (or `lantern repair`). If it fails,\n"+
		"  `lantern doctor --network %s` shows which sources failed and which flags help.\n"+
		"  Override (weaker trust, prints a warning): `lantern daemon --allow-rpc-anchor`",
		filepath.Join(netDir, "bootstrap-anchor.json"), network, defaultBootstrapQuorum(network), network, network)
}
