// Built-in FEVM warm-set (lantern#69).
//
// The prefetcher warms whatever contract addresses its consumer injects
// via Config.Addrs. curio-core injects the PDP contract set; stock
// upstream Curio injects nothing, so without a built-in default its
// Settle / provider-lookup eth_calls local-miss and fall back to the
// bridge ("FEVM method requires --vm-bridge-rpc"). To keep the zero-Glif
// read path working for any Lotus-API consumer, Lantern ships a small,
// stable, per-network default set of the well-known Filecoin PDP
// contracts and merges it with the consumer-supplied addresses.
//
// The list is deliberately tiny and matches curio-core's
// cmd/curio-core/fevm_prefetch.go set. Addresses are the deployed
// proxies (storage is read through the proxy), sourced from
// filecoin-project/curio pdp/contract/addresses.go.
package prefetch

import "strings"

// Well-known PDP contract proxy addresses, per network. Keep in sync
// with filecoin-project/curio pdp/contract/addresses.go.
const (
	// mainnet
	pdpVerifierMainnet     = "0xBADd0B92C1c71d02E7d520f64c0876538fa2557F"
	fwssMainnet            = "0x8408502033C418E1bbC97cE9ac48E5528F371A9f"
	serviceRegistryMainnet = "0xf55dDbf63F1b55c3F1D4FA7e339a68AB7b64A5eB"
	usdfcMainnet           = "0x80B98d3aa09ffff255c3ba4A241111Ff1262F045"

	// calibration
	pdpVerifierCalib     = "0x85e366Cf9DD2c0aE37E963d9556F5f4718d6417C"
	fwssCalib            = "0x02925630df557F957f70E112bA06e50965417CA0"
	serviceRegistryCalib = "0x839e5c9988e4e9977d40708d0094103c0839Ac9D"
	usdfcCalib           = "0xb3042734b608a1B16e9e86B374A3f3e389B4cDf0"
)

// BuiltinWarmSet returns Lantern's built-in FEVM prefetch warm-set for
// the given network name ("mainnet" or "calibration"; case-insensitive,
// "calibnet" accepted as an alias). Unknown networks (devnet, "") return
// nil — those contract addresses aren't fixed and must come from the
// consumer via Config.Addrs.
//
// The returned slice is a fresh copy the caller may mutate.
func BuiltinWarmSet(network string) []string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "mainnet":
		return []string{
			pdpVerifierMainnet,
			fwssMainnet,
			serviceRegistryMainnet,
			usdfcMainnet,
		}
	case "calibration", "calibnet", "calibrationnet":
		return []string{
			pdpVerifierCalib,
			fwssCalib,
			serviceRegistryCalib,
			usdfcCalib,
		}
	default:
		return nil
	}
}

// MergeWarmSets returns the union of two address lists, de-duplicated by
// canonical (lowercase, 0x-stripped) form, preserving first-seen order.
// Unparseable entries are kept verbatim (the prefetcher logs+skips them
// later) so this function never silently drops a consumer's input.
func MergeWarmSets(builtin, consumer []string) []string {
	seen := make(map[string]struct{}, len(builtin)+len(consumer))
	out := make([]string, 0, len(builtin)+len(consumer))
	add := func(list []string) {
		for _, a := range list {
			key := dedupeKey(a)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, a)
		}
	}
	// Consumer first so an explicitly-supplied address wins ordering,
	// then built-ins fill in anything the consumer didn't list.
	add(consumer)
	add(builtin)
	return out
}

// dedupeKey normalizes an address for de-dup: lowercase, 0x-stripped if
// it parses as a 20-byte hex address; otherwise the trimmed lowercase
// string verbatim (so two spellings of an unparseable entry still dedupe
// but distinct ones survive).
func dedupeKey(s string) string {
	if addr, ok := parseEthAddr(s); ok {
		return canonicalKey(addr)
	}
	return strings.ToLower(strings.TrimSpace(s))
}
