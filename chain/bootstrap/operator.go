package bootstrap

import (
	"net"
	"net/url"
	"strings"
)

// DefaultGatewayUpstream is the operator a Lantern gateway is attributed to
// when it does not advertise its upstream (#153). lantern-gateway proxies
// /state/root to its -glif flag, whose default is Glif, so an older gateway
// is conservatively counted as the same voter as Glif rather than as an
// independent one.
const DefaultGatewayUpstream = "glif.io"

// Note: OperatorOf(DefaultGatewayUpstream) is "chain.love" since F10 (see
// operatorAliases), so a default gateway votes as chain.love.

// OperatorOf derives an independence key from an endpoint URL or bare host
// (#153): the registrable domain (last two DNS labels), so
// api.node.glif.io and api.calibration.node.glif.io are both "glif.io" and
// calibration.filfox.info / filfox.info are both "filfox.info". IPs and
// single-label hosts (localhost) are returned as-is. Returns "" when no
// host can be parsed.
//
// Domains with documented common ownership are folded onto one key via
// operatorAliases (F10: glif.io serves from chain.love infrastructure).
//
// This is a heuristic: it cannot prove two domains are different
// operators, only collapse known duplicates. It is strictly more honest
// than counting by transport Kind, which let one upstream vote twice.
func OperatorOf(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	host := endpoint
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return ""
		}
		host = u.Hostname()
	} else if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host
	}
	labels := strings.Split(host, ".")
	if len(labels) > 2 {
		host = strings.Join(labels[len(labels)-2:], ".")
	}
	if alias, ok := operatorAliases[host]; ok {
		return alias
	}
	return host
}

// operatorAliases folds registrable domains that are verifiably run by the
// same operator onto one key (F10). Add an entry only with evidence; the
// comment must say what was observed and when.
var operatorAliases = map[string]string{
	// api.node.glif.io and api.calibration.node.glif.io resolve to the
	// same IP as api.chain.love (46.229.170.7), answer with
	// x-chainlove-lb-* load-balancer headers, report the same non-release
	// Lotus build (1.38.3+mainnet+git.eaea3cd), and a bare HEAD on the
	// Glif URL redirects to filecoin.chain.love. Observed 2026-10-26.
	// Counting Glif and chain.love as two voters let one operator vote
	// twice (three times with the chain.love libp2p bootstrap peer).
	"glif.io": "chain.love",
	// ChainSafe runs the bootstrap-*.chainsafe-fil.io peers (published
	// via the /dnsaddr record Lotus ships) and the Forest archive at
	// forest-archive.chainsafe.dev.
	"chainsafe.dev": "chainsafe-fil.io",
	"chainsafe.io":  "chainsafe-fil.io",
}

// OperatorOfMultiaddr derives the operator key for a libp2p multiaddr
// (F10): the registrable domain of its first /dns, /dns4, /dns6 or
// /dnsaddr component (through the same alias table as OperatorOf), else
// the literal IP of its /ip4 or /ip6 component. Returns "" when neither
// is present. Peer IDs alone say nothing about who runs a node, so a
// bare /p2p address has no operator.
func OperatorOfMultiaddr(ma string) string {
	parts := strings.Split(strings.TrimSpace(ma), "/")
	var ip string
	for i := 0; i+1 < len(parts); i++ {
		switch parts[i] {
		case "dns", "dns4", "dns6", "dnsaddr":
			return OperatorOf(parts[i+1])
		case "ip4", "ip6":
			if ip == "" {
				ip = parts[i+1]
			}
		}
	}
	return ip
}
