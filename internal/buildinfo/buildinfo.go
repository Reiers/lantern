// Package buildinfo exposes the Lantern build identity (version + network)
// so callers (RPC handlers, CLI version subcommand, beacons) don't need
// to import package main.
//
// The version string is baked at release time via:
//
//	-ldflags "-X main.versionTag=v1.2.1"
//
// in cmd/lantern/main.go, then propagated here at startup via SetVersion.
// When SetVersion isn't called (e.g. tests, library use) we fall back to
// the const defaultVersion below.
//
// The "Lantern+<network>" format is intentional: it's how clients (Curio's
// `Filecoin.Version` probe in particular) tell apart a Lantern light node
// from a Lotus full node without ambiguous compat suffixes.
package buildinfo

import "sync/atomic"

// defaultVersion is the fall-through identifier when neither ldflags nor
// SetVersion has populated the live version. We deliberately use "dev"
// rather than a stale numeric tag so untagged builds advertise themselves
// honestly.
const defaultVersion = "dev"

// defaultNetwork is the Filecoin network this binary targets. We're
// mainnet-only for V1.2.1; calibration support will flip this via
// SetNetwork (or a future build flag) once the bootstrap quorum has
// calibration sources wired in.
const defaultNetwork = "mainnet"

var (
	version atomic.Value // string
	network atomic.Value // string
)

// SetVersion records the live build version. Called by cmd/lantern/main.go
// at startup, passing the ldflags-injected versionTag. An empty string is
// treated as "not set" and BuildVersion falls back to defaultVersion.
func SetVersion(v string) {
	version.Store(v)
}

// SetNetwork records the active Filecoin network. Defaults to "mainnet"
// when unset. Reserved for calibration support.
func SetNetwork(n string) {
	network.Store(n)
}

// BuildVersion returns the current build's version tag, e.g. "v1.2.1".
// When SetVersion was never called or was called with "", returns "dev".
func BuildVersion() string {
	if v, ok := version.Load().(string); ok && v != "" {
		return v
	}
	return defaultVersion
}

// Network returns the active Filecoin network name. Defaults to "mainnet".
func Network() string {
	if n, ok := network.Load().(string); ok && n != "" {
		return n
	}
	return defaultNetwork
}

// UserAgent returns the canonical libp2p user-agent fragment for this
// build: "lantern/<version>". Suitable for libp2p.UserAgent(...) options.
func UserAgent() string {
	return "lantern/" + BuildVersion()
}
