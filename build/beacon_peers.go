package build

// DefaultBeaconPeers is the built-in, trusted Lantern cert-exchange beacon
// floor (security #59). A fresh node seeds its cert-exchange peer set from
// this list BEFORE DHT rendezvous discovery warms, so it always has an
// honest source of F3 finality certs and cannot be fully eclipsed by an
// attacker who floods the rendezvous point with hostile peers.
//
// F3 certs from any peer are still BLS-aggregate-verified downstream, so a
// hostile peer cannot forge a cert; the threat this floor mitigates is
// *eclipse / availability* — being crowded out of honest cert sources and
// thereby stalled in finality observation. Operators can extend the floor
// with --certexch-peers.
//
// Entries are full multiaddrs including /p2p/<peerid>. Keep this list small
// and operator-trustworthy; it is a trust anchor.
var DefaultBeaconPeers = map[Network][]string{
	// Populated as public Lantern beacons come online. Intentionally empty
	// today rather than pointing at unverified third-party infra: an empty
	// floor degrades to "rely on DHT discovery + operator --certexch-peers"
	// (today's behaviour), while a wrong entry would be a standing trust
	// liability. The wiring (seed-before-discovery + dynamic cap) is the
	// security fix; this list is the operator-owned trust anchor it seeds.
	Mainnet:     {},
	Calibration: {},
}

// BeaconPeers returns the default trusted beacon floor for the network.
func (n Network) BeaconPeers() []string {
	return DefaultBeaconPeers[n]
}

// MaxDynamicBeaconPeers caps how many DHT-discovered (untrusted-origin)
// beacon peers are kept in the cert-exchange rotation (security #59
// anti-eclipse). Bounding the dynamic pool means a rendezvous flood can
// crowd the rotation only up to this ceiling; the trusted floor + operator
// pins always sit ahead of them and are never evicted.
const MaxDynamicBeaconPeers = 16
