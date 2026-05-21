// Package bitswap is Lantern's Bitswap client.
//
// Phase 10 (V1.2 swarm-native fetch) replaced the original Phase 2 stub
// with a real boxo/bitswap client wired against the Lantern libp2p host.
// The package still exposes the Stub type for backward compatibility with
// older Phase 2 demos and unit tests in net/combined.
//
// Production usage:
//
//	hc, _ := llibp2p.New(ctx, llibp2p.HostConfig{...})
//	bs, _ := bitswap.New(ctx, bitswap.Config{
//	    Host:           hc.H,
//	    ProviderFinder: hc.DHT(),       // optional; when nil broadcasts to connected peers
//	    PreferredPeers: beaconPeers,    // optional; warm-pool of beacon multiaddrs
//	})
//	// bs satisfies state/hamt.BlockGetter:
//	rawBlock, err := bs.Get(ctx, someCID)
//
// Trust model: every block returned is hashed by net/combined.Fetcher
// before being trusted. Peers cannot lie undetected; the worst they can
// do is refuse to serve.
//
// No CGo; depends only on github.com/ipfs/boxo and github.com/libp2p/go-libp2p.
package bitswap
