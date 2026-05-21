# Phase 11 sub-agent: peer-count + chain-head visibility

Note for the Phase 11 sub-agent if it has cycles after Parts A-D.

## Observed problem

Live mainnet Lantern daemon on sp.reiers.io reports only **4 libp2p
peers** in steady state:
- 3 mainnet bootstrap peers (PL, ChainSafe, Glif)
- 1 Lantern beacon (Hetzner)

For comparison, Lotus on the same box sits at ~658 peers. Forest defaults
to a target of 75. The 4-peer floor is artificially low.

Sync is unaffected (chain head advances in real-time at the expected
~30s/epoch rate), but:
- Bitswap broadcast WANTs only reach those 4 peers, dramatically
  limiting state-fetch resilience
- The daemon is invisible to most of the Filecoin swarm
- Operators looking at Curio's "Chain Node Network" panel see a small
  number that looks unhealthy

## Three fixes, ranked

### Fix 1: Kademlia DHT in full discovery mode (~200 LOC)

Phase 6 wired the DHT as a client-mode singleton used only for
`lantern/beacon/v1` rendezvous discovery. Expand it to a generic
peer-discovery role:

1. Start the DHT in **client mode** (we don't want to be a query target;
   we're a light client)
2. After bootstrap, run a periodic `dht.GetClosestPeers(myPeerID)` walk
   every 5 minutes — this populates the DHT routing table with ~50-200
   peers from across the network
3. Walk the DHT routing table on a slower cadence (every 10 min),
   attempt outbound libp2p connections to peers we don't already have

This is what brings peer count from 4 → 50-100 organically.

File: `net/libp2p/dht.go`

### Fix 2: Connection manager with low-watermark target (~80 LOC)

libp2p's `connmgr.NewConnManager(low, high, gracePeriod)` is the
standard pattern. Set `low=50, high=200`. When peer count drops below
50, connmgr proactively dials from the peerstore.

Wire it via `libp2p.ConnectionManager(connmgr)` at host construction.

File: `net/libp2p/host.go` (or wherever the host is constructed)

### Fix 3: Subscribe to a Filecoin protocol (~50 LOC, opt-in)

Lantern doesn't currently subscribe to gossipsub topics like
`/fil/msgs/<network>` or `/fil/blocks/<network>`. Subscribing alone
makes Lantern advertise itself as a real participant, and peers
naturally discover us via gossipsub mesh formation.

This is opt-in via a `--gossipsub-topics` flag because:
- It costs some bandwidth (mainnet messages stream in continuously)
- Light clients don't strictly need it (we don't validate or relay)
- But for users who want to be a "good citizen" of the swarm and
  benefit from passive peer-discovery, it's a clean toggle

File: `net/libp2p/gossipsub.go` (new) + `cmd/lantern/daemon.go` flag

## Expected outcome

After Fix 1 + Fix 2:
- Peer count: 4 → 50-150 in 5-10 minutes
- Bitswap WANT-HAVE broadcast reach: ~30x larger
- Curio webui Chain Node Network panel shows healthy numbers
- Beacon discovery and DHT rendezvous works (already does, just becomes
  more effective at scale)

Fix 3 is optional polish.

## Validation

Deploy updated binary to mainnet daemon, watch NetPeers for 10 minutes,
expect monotonic climb to 50+ then stabilization at 75-150.

