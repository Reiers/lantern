# Phase 11 sub-agent: peer-count + chain-head visibility

> **Status (V1.2.1):** Fix 1 (DHT full discovery) and Fix 2 (connection
> manager with low-watermark) are **SHIPPED in V1.2.1**. Fix 3
> (gossipsub topic subscription) remains a follow-up — likely combined
> with the V1.2.1-followup conversation on state-call timeouts.

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

### Fix 1: Kademlia DHT in full discovery mode — SHIPPED in V1.2.1

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

**V1.2.1 implementation:** `EnableDHT` now spawns two extra loops in
addition to the existing refresh loop:

- `dhtClosestWalkLoop` runs `dht.GetClosestPeers(self)` every 5 minutes
  (after a 30s warm-up). Each walk seeds the routing table with peers
  from across the swarm and logs a one-line summary.
- `dhtDialWalkLoop` reads `dht.RoutingTable().ListPeers()` every 10
  minutes, filters out peers we're already connected to, and dials up
  to 25 candidates per cycle with an 8s per-dial timeout. Capped by
  the connmgr high-water-mark so it doesn't fight the trim path.

The loops are exposed as a free function `Host.RunDHTDiscovery` so the
beacon path (which constructs its own server-mode DHT) gets the same
active peer growth without going through `EnableDHT`.

### Fix 2: Connection manager with low-watermark target — SHIPPED in V1.2.1

libp2p's `connmgr.NewConnManager(low, high, gracePeriod)` is the
standard pattern. Set `low=50, high=200`. When peer count drops below
50, connmgr proactively dials from the peerstore.

Wire it via `libp2p.ConnectionManager(connmgr)` at host construction.

File: `net/libp2p/host.go` (or wherever the host is constructed)

**V1.2.1 implementation:** `HostConfig` now exposes both `MinPeers` and
`MaxPeers` (with a `ConnMgrGrace` default of 20s). `New(...)` constructs
a `connmgr.NewConnManager(MinPeers, MaxPeers, WithGracePeriod(...))`
and passes it via `libp2p.ConnectionManager(cm)`. Defaults are tuned
per subcommand:

- `lantern daemon`: MinPeers=50, MaxPeers=200 (was hard cap 50)
- `lantern beacon`: MinPeers=100, MaxPeers=200 (was hard cap 200)
- `lantern init` ephemeral bootstrap host: MinPeers=20, MaxPeers=100

`Host.MinPeers()` / `Host.MaxPeers()` are exposed so the DHT dial-walk
loop in Fix 1 can stop dialing once it crosses the high-water.

### Fix 3: Subscribe to a Filecoin protocol (~50 LOC, opt-in) — DEFERRED

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

## Expected outcome (V1.2.1 deployment validation TBD)

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


---

## V1.2.1 deploy finding (2026-05-22)

After shipping Fix 1 + Fix 2 and redeploying to live mainnet
(daemon 192.168.2.32:11234, lantern v1.2.1):

```
libp2p[dht]: closest-walk peers=0 rt_size=0 connected=4 err=failed to find any peer in table
libp2p[dht]: dial-walk candidates=0 dialed=0 connected=4 rt_size=0
```

**Diagnosis:** the DHT walks fire on schedule, but the routing table
never populates because:

1. `dht.Bootstrap(ctx)` is not called against the standard libp2p /
   Filecoin DHT seed peers after host construction.
2. The 4 already-connected peers (3 PL/ChainSafe/Glif bootstraps +
   1 Lantern beacon) come from the daemon's `connectKnownPeers` path,
   which doesn't push them into the DHT routing table.

**Sub-agent task for V1.2.1-followup-peers:**

a) After `dht.New(...)` returns, call `dht.Bootstrap(ctx)` against
   either:
   - the libp2p IPFS DHT bootstrap list (8 known peers), OR
   - the Filecoin-specific bootstrap list from `chain/bootstrap/peerlist`
     (whichever already exists; otherwise add a small const list)

b) After every successful `connectKnownPeers` connect, also call
   `dht.RoutingTable().TryAddPeer(pid, ..., false)` so those peers
   become walk candidates.

c) Surface `rt_size` in the boot summary line, not just in the
   periodic walk log, so misconfig is visible at startup.

This unblocks peers >= 50 and likely also unblocks the StateMinerInfo
timeouts (state walks over Bitswap with only 4 peers are the obvious
bottleneck).

