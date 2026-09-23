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

// OperatorOf derives an independence key from an endpoint URL or bare host
// (#153): the registrable domain (last two DNS labels), so
// api.node.glif.io and api.calibration.node.glif.io are both "glif.io" and
// calibration.filfox.info / filfox.info are both "filfox.info". IPs and
// single-label hosts (localhost) are returned as-is. Returns "" when no
// host can be parsed.
//
// This is a heuristic: it cannot prove two domains are different
// operators, only collapse obvious duplicates. It is strictly more honest
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
	if len(labels) <= 2 {
		return host
	}
	return strings.Join(labels[len(labels)-2:], ".")
}
