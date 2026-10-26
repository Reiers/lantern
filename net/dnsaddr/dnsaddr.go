// Package dnsaddr expands /dnsaddr bootstrap roots into dialable
// /p2p multiaddrs (F10).
//
// Lotus ships bootstrap lists that mix fully specified peers
// (/dns/host/tcp/port/p2p/ID) with /dnsaddr roots such as
// /dnsaddr/bootstrap-mainnet.chainsafe-fil.io. A /dnsaddr root has no
// /p2p component, so every peer.AddrInfoFromP2pAddr consumer silently
// skipped it. Expand resolves the TXT records at _dnsaddr.<domain>
// (recursively, bounded) once at startup and hands consumers a plain
// list of /p2p multiaddrs, with an embedded snapshot as the offline
// fallback.
package dnsaddr

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"

	"github.com/Reiers/lantern/build"
)

// Entry is one expanded bootstrap address plus the list entry it came
// from. Origin equals Addr for entries that needed no expansion; for
// /dnsaddr expansions it is the /dnsaddr root, which is what the quorum
// uses to attribute the peer to an operator (the domain owner who
// publishes the TXT records vouches for the peers behind it).
type Entry struct {
	Addr     string
	Origin   string
	Fallback bool // true when Addr came from the embedded snapshot
}

// Resolver is the subset of madns.Resolver Expand needs. Injectable for
// tests.
type Resolver interface {
	Resolve(ctx context.Context, ma multiaddr.Multiaddr) ([]multiaddr.Multiaddr, error)
}

// DefaultResolver is the system resolver used by Expand.
var DefaultResolver Resolver = madns.DefaultResolver

// DefaultTimeout bounds the whole expansion.
const DefaultTimeout = 5 * time.Second

const maxDepth = 3

// IsRoot reports whether s is a /dnsaddr root that still needs expansion
// (starts with /dnsaddr/ and carries no /p2p component).
func IsRoot(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "/dnsaddr/") && !strings.Contains(s, "/p2p/")
}

// Expand resolves every /dnsaddr root in addrs with DefaultResolver and
// returns the de-duplicated result in input order. Non-root entries pass
// through untouched. A root that fails to resolve (or resolves to
// nothing) falls back to build.BootstrapDNSAddrSnapshot; a root with no
// snapshot is dropped.
func Expand(ctx context.Context, addrs []string) []Entry {
	return ExpandWith(ctx, DefaultResolver, addrs, build.BootstrapDNSAddrSnapshot)
}

// ExpandStrings is Expand returning only the addresses.
func ExpandStrings(ctx context.Context, addrs []string) []string {
	if !anyRoot(addrs) {
		return addrs
	}
	es := Expand(ctx, addrs)
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Addr)
	}
	return out
}

func anyRoot(addrs []string) bool {
	for _, a := range addrs {
		if IsRoot(a) {
			return true
		}
	}
	return false
}

// ExpandWith is Expand with an explicit resolver and snapshot.
func ExpandWith(ctx context.Context, r Resolver, addrs []string, snapshot map[string][]string) []Entry {
	if anyRoot(addrs) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	seen := map[string]bool{}
	var out []Entry
	add := func(e Entry) {
		if e.Addr == "" || seen[e.Addr] {
			return
		}
		seen[e.Addr] = true
		out = append(out, e)
	}
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !IsRoot(a) {
			add(Entry{Addr: a, Origin: a})
			continue
		}
		var resolved []string
		if r != nil {
			resolved = resolve(ctx, r, a, 0)
		}
		if len(resolved) == 0 {
			for _, s := range snapshot[a] {
				add(Entry{Addr: s, Origin: a, Fallback: true})
			}
			continue
		}
		for _, s := range resolved {
			add(Entry{Addr: s, Origin: a})
		}
	}
	return out
}

func resolve(ctx context.Context, r Resolver, root string, depth int) []string {
	if depth >= maxDepth {
		return nil
	}
	ma, err := multiaddr.NewMultiaddr(root)
	if err != nil {
		return nil
	}
	res, err := r.Resolve(ctx, ma)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range res {
		s := m.String()
		switch {
		case IsRoot(s):
			out = append(out, resolve(ctx, r, s, depth+1)...)
		case strings.Contains(s, "/p2p/"):
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
