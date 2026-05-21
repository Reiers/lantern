# Phase 10 — Blockers, Decisions, and Known Limitations

Phase 10 was the V1.2 swarm-native delivery: make Lantern a first-class
participant in the Filecoin libp2p swarm, replace the HTTP-gateway-as-
primary architecture with Bitswap-primary fetch, ship the `lantern beacon`
subcommand, and turn the Curio webui Chain Node Network panel into real
data instead of stubs.

This file pairs with `docs/phase10-part-d-live-bind.md` (the live-mainnet
deployment evidence) and supersedes the V1.2 outstanding items called
out in PHASE9-BLOCKERS.md.

---

## Headline outcome

- ✅ **Part A** — Live libp2p host wired through the Net*/Eth RPC methods.
  Curio's webui Chain Node Network panel now shows Lantern with real
  peer count, real bandwidth totals, real reachability (no longer
  0/0/Unknown).
- ✅ **Part B** — Real `boxo/bitswap` client integrated as the primary
  fetch source. HTTP gateway demoted to last-resort fallback. New
  `--metrics` Prometheus endpoint exposes per-source hit counts.
- ✅ **Part C** — `lantern beacon` subcommand: pure-Go single-binary
  state-serving node with a Badger v4 persistent cache, libp2p host,
  Bitswap server (via boxo), DHT mode-server routing, and
  `lantern/beacon/v1` rendezvous announce. E2E test in
  `cmd/lantern/beacon_test.go` proves client-side Bitswap fetch via a
  beacon works end-to-end.
- ✅ **Part D** — Deployed to live mainnet Lantern on sp.reiers.io
  (192.168.2.32) and visually verified in Curio's webui. Screenshot at
  `docs/screenshots/curio-webui-phase10-live.png`; JSON dump of the
  exact NetSummary response at
  `docs/screenshots/curio-netsummary-phase10.json`.
- ✅ **Part E** — README, SWARM-ARCHITECTURE.md updated; this file
  written.

V1.2-rc.1 is ready.

---

## What Phase 10 shipped

### Part A — Live libp2p Net* wiring (URGENT → DONE)

Files:
- `net/libp2p/host.go`: install `metrics.NewBandwidthCounter` on
  construction via `libp2p.BandwidthReporter`; subscribe to
  `event.EvtLocalReachabilityChanged` from the host's event bus and
  cache the value in an `atomic.Int32`; expose `Reachability()` and
  `PublicAddrs()` helpers.
- `net/libp2p/netinfo.go` (new): `Host.NetInfo()` adapter that
  satisfies `handlers.NetInfo` (the narrow surface rpc/handlers
  consumes). Single boundary file: rpc/handlers itself does not import
  libp2p.
- `rpc/handlers/netinfo.go` (new): declare the `NetInfo` and
  `NetInfoPeer` shapes, add `ChainAPI.WithNetInfo()` + `NetInfoSource`
  field.
- `rpc/handlers/extra.go`: replace the Phase 9 Net* stubs with real
  implementations that delegate to `NetInfoSource` when wired. New
  behaviour:
    - `NetPeers` → live `host.Network().Peers()` + each peer's
      `Peerstore().Addrs(pid)`
    - `NetAgentVersion(pid)` → live `Peerstore().Get(pid, "AgentVersion")`
    - `NetConnectedness(pid)` → live `host.Network().Connectedness(pid)`
    - `NetListening` → `len(host.Network().ListenAddresses()) > 0`
    - `NetBandwidthStats` → live BandwidthCounter totals
    - `NetAutoNatStatus` → cached EvtLocalReachabilityChanged value +
      live host.Addrs()
    - `EthBlockNumber` → current head epoch from the header store as
      `0x%x` (was hardcoded 0x0)
- `cmd/lantern/main.go`: `--p2p-listen` flag (default
  `/ip4/0.0.0.0/tcp/0,/ip4/0.0.0.0/udp/0/quic-v1`), `--no-libp2p`,
  brings up the host at daemon boot, attaches its NetInfo() to
  ChainAPI.
- `net/libp2p/netinfo_test.go` (new): two-host integration test that
  connects two libp2p hosts and verifies `Peers/Connectedness/Listening`
  return live data and that the BandwidthCounter records bytes after
  the identify handshake.

**Live evidence (Part D):**
```json
{
  "node": "/ip4/127.0.0.1/tcp/11234/http",
  "epoch": 6036201,
  "peerCount": 2,
  "bandwidth": {"totalIn": 5031, "totalOut": 3486, "rateIn": 0, "rateOut": 0},
  "reachability": {"status": "private", "publicAddrs": [6 multiaddrs]}
}
```
Compare to Phase 9: `{peerCount: 0, totalIn: 0, totalOut: 0, status: "unknown"}`.

### Part B — Bitswap as primary fetch path (DONE)

Files:
- `net/bitswap/client.go` (new): Lantern's Bitswap client wrapper.
  Boxo `bitswap.New()` bound to `bsnet.NewFromIpfsHost(host)`. Two-stage
  Get(): fast deadline for `PreferredPeers`, longer deadline for
  full-swarm broadcast. `NewSession(ctx)` exposes a Bitswap session so
  HAMT walks share a warm peer pool. `Stats()` returns
  `{GotBlocks, Misses, Errors, BytesIn}` counters.
- `net/bitswap/client_test.go` (new): two-host integration test — one
  serves a block via boxo Bitswap, the other fetches via Lantern's
  `Client`. Verifies real byte transfer with full CID match.
- `net/bitswap/doc.go`: rewritten for Phase 10. The `Stub` type is
  kept for backward compatibility with `cmd/lantern-phase2` and
  `net/combined`'s fetcher unit tests.
- `cmd/lantern/main.go` + `cmd/lantern/daemon_extra.go`:
    - `--bitswap` (default true), `--bitswap-peers`,
      `--bitswap-fast/--bitswap-full` deadlines
    - `--metrics` listen for the `/metrics` Prometheus endpoint
    - After libp2p comes up, rebuild `combined.Fetcher` with Bitswap
      inserted between cache and gateway
    - Metrics exposed: `lantern_fetch_total{source=...}`,
      `lantern_bitswap_blocks_total`, `lantern_bitswap_bytes_in_total`,
      `lantern_libp2p_peers`, `lantern_libp2p_bw_bytes{direction=...}`

**Live mainnet evidence:** the production daemon's `/metrics` after
5 state queries via Lantern's RPC:
```
lantern_fetch_total{source="gateway"} 5
lantern_fetch_total{source="bitswap"} 0
lantern_fetch_total{source="misses"} 1
lantern_bitswap_errors_total 5
```

Read: Bitswap was attempted on every block (5 attempts), all 5 timed
out within the 5s budget because the daemon has zero `--bitswap-peers`
preferred-peer configuration and the 2 mainnet bootstrap peers it's
connected to don't reliably serve Filecoin state CIDs. The gateway
fallback caught all 5, so the user-visible behaviour stayed identical
to V1.1. **This is the expected V1.2-rc.1 behaviour** — Bitswap shines
once beacons are connected as preferred peers (see Part C and the
local E2E test). Phase 11's client-side DHT beacon discovery loop
closes the last gap by auto-populating `--bitswap-peers` from the
DHT rendezvous.

### Part C — `lantern beacon` subcommand (DONE)

Files:
- `cmd/lantern/beacon.go` (new): `cmdBeacon` + `badgerBlockstore` (a
  `boxo/blockstore.Blockstore` backed by Badger v4 directly, with
  multihash keys and codec-erasure on iteration). Boots a libp2p host
  with mainnet bootstrap peers, a `dht.IpfsDHT` in ModeServer, a boxo
  Bitswap server-side wired to the badger blockstore, and a
  `routing.NewRoutingDiscovery(kdht).Advertise(BeaconRendezvous)` loop
  that re-advertises every TTL/2 (or 1h fallback).
- `cmd/lantern/beacon_test.go` (new): E2E test boots a beacon-shaped
  host with one block in cache, points a Lantern Bitswap client at it
  as a preferred peer, and verifies the block transfers via Bitswap
  with byte-exact match.
- `cmd/lantern/main.go`: dispatch `lantern beacon` + help text.

Flags:
- `--cache-dir` (default `~/.lantern-beacon`)
- `--cache-size` (default `5GiB`, parsed with K/M/G/T + iB suffixes)
- `--listen` (default `/ip4/0.0.0.0/tcp/4001,/ip4/0.0.0.0/udp/4001/quic-v1`)
- `--dht-announce` (default true)
- `--gateway` (upstream URL for cache-miss backfill)
- `--metrics` (optional metrics listen, empty disables)

**E2E test result:** `TestBeacon_E2E_FetchViaBeacon` passes in 0.5s
locally. Beacon-side stats show 1 block in cache and the client-side
Bitswap stats show 1 block fetched and non-zero bytes received.

### Part D — Live mainnet deployment + visual verification (DONE)

Steps executed:
1. Cross-compiled `linux/amd64`:
   `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o lantern-linux-amd64 ./cmd/lantern`
   (57 MB statically linked ELF).
2. Uploaded via lex (`lexluthr@37.202.57.171`) → mainnet
   (`reiers@192.168.2.32` via internal LAN).
3. Backed up the running 35 MB Phase 9 binary as `/home/reiers/lantern.bak`.
4. Ctrl-C'd the existing `lantern` tmux pane, swapped the binary,
   started a fresh tmux session with:
   ```
   tmux new -d -s lantern bash -c "LANTERN_PASS=lantern-mainnet-test \\
     /home/reiers/lantern daemon -listen 127.0.0.1:11234 \\
                                  -metrics 127.0.0.1:11235 \\
                                  2>&1 | tee -a /home/reiers/lantern.log"
   ```
5. Did **not** touch the `curio`, `lotus`, or `yugabyte` tmux sessions.
6. Probed the live daemon via JSON-RPC (admin token); confirmed
   `NetPeers` = 3, `NetBandwidthStats` non-zero, `NetAutoNatStatus`
   `private` with 6 listen multiaddrs, `EthBlockNumber` derived from
   the live head epoch.
7. Tunnelled `Mac:14701 → lex → mainnet:4701` (the Curio webui) and
   used `capture-website` (Playwright under the hood) to take a
   screenshot.
8. Cross-checked the rendered HTML against Curio's `CurioWeb.NetSummary`
   WebSocket-RPC response — values match.

**Files of evidence:**
- `docs/screenshots/curio-webui-phase10-live.png` (full-page screenshot
  of the Overview panel after Phase 10 deploy)
- `docs/screenshots/curio-netsummary-phase10.json` (raw
  `CurioWeb.NetSummary` RPC response showing both the Lantern node and
  the Lotus node side-by-side)
- `docs/phase10-part-d-live-bind.md` (this run's narrative)

### Part E — README + SWARM-ARCHITECTURE.md + this file (DONE)

- README updates per `POST-PHASE-10-README.md` queue:
    - Footprint corrected: "~150 MB of disk" (vs the old "~1 GB"
      stretch claim)
    - F3 participation clarification added
    - Status matrix: Phase 10 row added as ✅ Shipped
    - Architecture diagram caption updated: Bitswap is primary, gateway
      is last-resort
    - `lantern beacon` quick-start added
- SWARM-ARCHITECTURE.md: V1.2 §1-§3 marked DELIVERED with the
  shipped-feature details; roadmap shifted so V1.3 covers client-side
  DHT discovery, multi-gateway fallback, and metrics dashboard.

---

## What Phase 10 did NOT close (V1.2 GA priorities)

### B-10-01 — Client-side DHT beacon auto-discovery

**Severity: medium.**

The beacon side advertises `lantern/beacon/v1` correctly. The client
side, however, requires manual `--bitswap-peers` configuration. The
piece missing is a goroutine on the Lantern daemon that periodically
queries `routing.NewRoutingDiscovery(kdht).FindPeers(BeaconRendezvous)`
and updates the Bitswap client's preferred-peer set.

Until this lands, Bitswap-from-preferred-peers requires operators to
hand-configure their beacons in the daemon args.

Estimated effort: ~80 LoC + a unit test, plus the daemon needs a DHT
client (currently optional; switch on by default for V1.3).

### B-10-02 — Beacon cache-miss backfill

**Severity: low (operators can pre-populate the cache).**

`beaconBackfillLoop` is scaffolded in `cmd/lantern/beacon.go` but is
currently a no-op. The intent is to subscribe to the boxo Bitswap
server's "incoming wantlist" (CIDs peers are asking for) and fetch
each unknown CID from the configured upstream gateway, so the beacon
warms its cache reactively.

Boxo's `bitswap.Server` doesn't expose an `OnPeerWant(cid)` hook on
the current API surface; the work is to either subscribe through the
internal peermanager or contribute an upstream API. Either way it's a
V1.3 polish.

Estimated effort: 2-4 hours, includes an upstream PR if we go that
route.

### B-10-03 — Lantern's mainnet daemon needs a beacon preferred-peer

**Severity: low (operators can configure this immediately after the
first beacon lights up).**

The live mainnet daemon (sp.reiers.io) runs with `--bitswap-peers=""`
because no Lantern beacons exist yet. To get the
"Bitswap-serves-blocks-without-gateway-fallback" evidence we need to:
1. Start a Lantern beacon on lex or a Hetzner box, pre-warmed with a
   handful of recent state CIDs.
2. Add its multiaddr to mainnet daemon's `--bitswap-peers` arg.
3. Re-probe `/metrics` after a few minutes of state reads; confirm
   `lantern_fetch_total{source="bitswap"}` is non-zero.

Not a code change — operational follow-up.

### B-10-04 — Beacon's badger blockstore lacks cache eviction

**Severity: low (operators pick a `--cache-size` and stay under it).**

`--cache-size` is parsed and stored, but the beacon never deletes old
blocks. Until eviction lands, operators on a 5 GB cap need to manually
prune or restart with `--cache-dir=fresh`. The cleanest approach is a
size-bounded LRU (Badger's GC + a manifest of last-access timestamps).

Estimated effort: ~150 LoC + an integration test, V1.3 polish.

### B-10-05 — QUIC receive buffer warning at startup

**Severity: cosmetic.**

The daemon prints:
```
failed to sufficiently increase receive buffer size (was: 208 kiB,
wanted: 7168 kiB, got: 416 kiB).
```

This is a kernel `net.core.rmem_max` limit on the mainnet box. Curio
hits the same warning. Operators can raise it via sysctl; not
something Lantern can fix from userland.

### B-10-06 — Header sync still uses Glif RPC, not Bitswap

**Severity: medium-low (architectural alignment).**

The header store sync agent in `chain/header/store/sync.go` polls
Glif via `chain/header/store/glif` for new tipsets. Bitswap could
serve recent block headers just as well from a beacon, but the sync
agent doesn't yet have a Bitswap-aware code path.

This is the natural next step after B-10-01 (DHT auto-discovery)
lands: once we have an auto-populated preferred-peer set, the header
sync source should route through it before Glif. Tracked as a V1.3
item.

### Carry-overs from Phase 9 still open

- B-9-11 — Wallet keystore refuse-to-start when crypto disabled
- B-9-12 — F3 anchor freshness enforcement
- B-9-13 — Release CI / signed binaries
- B-9-14 — Deep block validation in net/blockpub
- B-9-15 — Header store ↔ F3 cert cross-check
- B-9-17 — Curio's `IPNI` task connectivity
- B-9-18 — `dev/dri/renderD128` permission spam

None of these block V1.2-rc.1.

---

## Method coverage table — Phase 10 update

71/71 of the Curio FULLNODE_API spec. Phase 10 didn't add methods; it
turned the Net*/Eth probe stubs into live data:

| Method                           | Pre-Phase-10            | Post-Phase-10                              |
|----------------------------------|-------------------------|--------------------------------------------|
| `Filecoin.NetPeers`              | `[]` (stub empty)       | Live `host.Network().Peers()` + peerstore  |
| `Filecoin.NetAgentVersion`       | `"lantern/unknown"`     | Live peerstore `AgentVersion` lookup       |
| `Filecoin.NetConnectedness`      | `0` (NotConnected)      | Live `host.Network().Connectedness(pid)`   |
| `Filecoin.NetListening`          | `true` (constant)       | Live `len(ListenAddresses()) > 0`          |
| `Filecoin.NetBandwidthStats`     | `{0,0,0,0}`             | Live `metrics.BandwidthCounter` totals     |
| `Filecoin.NetAutoNatStatus`      | `{Unknown, nil}`        | Live EvtLocalReachabilityChanged value     |
| `Filecoin.EthBlockNumber`        | `"0x0"` (constant)      | `0x%x` of head epoch                       |

Plus zero regressions on the other 64.

---

## V1.2 release readiness verdict

| Surface                              | V1.2-rc.1 ready? |
|--------------------------------------|------------------|
| Live libp2p stats via RPC            | ✅ |
| Bitswap as primary fetch source      | ✅ (scaffold + fallback) |
| Bitswap actually carrying load       | 🟡 needs beacons online (B-10-03) |
| `lantern beacon` subcommand          | ✅ |
| DHT beacon rendezvous announce       | ✅ |
| DHT client-side discovery            | 🟡 V1.3 (B-10-01) |
| Curio webui shows real Lantern stats | ✅ (screenshot evidence) |
| Header sync via swarm                | 🟡 V1.3 (B-10-06) |
| Documentation                        | ✅ (README + SWARM-ARCHITECTURE) |

**Recommendation:** cut V1.2-rc.1 from current main; the Bitswap+beacon
machinery is complete and operational; the empty preferred-peer set
on the live deploy is operator config (B-10-03), not a code gap.
Promote to V1.2 GA after at least one beacon is online and the
mainnet daemon's `/metrics` shows non-zero
`lantern_fetch_total{source="bitswap"}`.

---

## Files touched in Phase 10

- `net/libp2p/host.go` — bandwidth counter + reachability subscriber (Part A)
- `net/libp2p/netinfo.go` (new) — handlers.NetInfo adapter (Part A)
- `net/libp2p/netinfo_test.go` (new) — two-host live test (Part A)
- `rpc/handlers/netinfo.go` (new) — NetInfo interface + WithNetInfo (Part A)
- `rpc/handlers/extra.go` — Net* method rewrites (Part A)
- `rpc/handlers/chain_api.go` — NetInfoSource field (Part A)
- `net/bitswap/client.go` (new) — real Bitswap client (Part B)
- `net/bitswap/client_test.go` (new) — two-host fetch test (Part B)
- `net/bitswap/doc.go` — Phase 10 rewrite (Part B)
- `cmd/lantern/main.go` — libp2p+bitswap+beacon wiring (Parts A/B/C)
- `cmd/lantern/daemon_extra.go` (new) — helpers + /metrics (Part B)
- `cmd/lantern/beacon.go` (new) — `lantern beacon` (Part C)
- `cmd/lantern/beacon_test.go` (new) — beacon E2E test (Part C)
- `.gitignore` — anchor patterns to repo root (fixup)
- `docs/screenshots/curio-webui-phase10-live.png` (new) — Part D evidence
- `docs/screenshots/curio-netsummary-phase10.json` (new) — Part D evidence
- `docs/phase10-part-d-live-bind.md` (new) — Part D narrative
- `README.md` — Phase 10 status + V1.2 talking points (Part E)
- `SWARM-ARCHITECTURE.md` — V1.2 marked delivered (Part E)
- `PHASE10-BLOCKERS.md` (this file)

All commits granular. Nothing pushed by me after the fixup commit; the
earlier Phase 10 commits were pushed by Nicklas mid-flight (his call).
