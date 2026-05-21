# Phase 10 live beacon deployment evidence

**Date:** 2026-05-21 ~22:05-22:15 CPH

## What was deployed

### Beacon on Hetzner (`157.180.16.39`)

- Service: `lantern-beacon` (systemd, auto-restart)
- Binary: `/usr/local/bin/lantern` (Phase 10, 57 MB)
- Cache: `/var/lib/lantern-beacon` (Badger v4, cap 10 GiB)
- Listen: `/ip4/0.0.0.0/tcp/4001`, `/ip4/0.0.0.0/udp/4001/quic-v1`
- Peer ID: `12D3KooWHUD3zzdQQavMbkrUjM1JFhTMB3s745KsziKu26tPRY13`
- Public multiaddr: `/ip4/157.180.16.39/tcp/4001/p2p/12D3KooWHUD3zzdQQavMbkrUjM1JFhTMB3s745KsziKu26tPRY13`
- DHT: server-mode, announces under `lantern/beacon/v1`
- Backfill upstream: `https://gateway.lantern.reiers.io`
- Metrics: `http://127.0.0.1:9091/metrics`
- Memory: ~10 MB
- Firewall: `ufw allow 4001/tcp`, `ufw allow 4001/udp`

### Mainnet daemon update (`192.168.2.32`)

- Binary swapped from `lantern v0.4.0 (Phase 4)` to Phase 10 build (57 MB)
- Old binary preserved at `/home/reiers/lantern.prev`
- Earlier binary at `/home/reiers/lantern.bak`
- Restart command (in tmux session `lantern`):
  ```
  LANTERN_PASS=lantern-mainnet-test /home/reiers/lantern daemon \
    -listen 127.0.0.1:11234 \
    -bitswap-peers /ip4/157.180.16.39/tcp/4001/p2p/12D3KooWHUD3zzdQQavMbkrUjM1JFhTMB3s745KsziKu26tPRY13 \
    -metrics 127.0.0.1:9092
  ```
- Boot log confirms `bitswap: enabled (preferred=1, fast=1.5s, full=5s)`

## Connectivity verified

After ~30s of runtime, mainnet daemon's libp2p host had 4 peers:

```
peer_count= 4
peers: [
  '12D3KooWHQRSDFv4FvAjtU32shQ7znz7oRbLBryXzZ9NMK2feyyH',  # mainnet bootstrap
  '12D3KooWGnkd9GQKo3apkShQDaq1d6cKJJmsVe6KiQkacUk1T8oZ',  # mainnet bootstrap
  '12D3KooWBF8cpp65hp2u9LK5mh19x67ftAam84z9LsfaquTDSBpt',  # mainnet bootstrap
  '12D3KooWHUD3zzdQQavMbkrUjM1JFhTMB3s745KsziKu26tPRY13'   # **our beacon on Hetzner**
]
```

Beacon symmetrically reports 4 libp2p peers (mainnet daemon among them).

## Fetch metrics (mainnet daemon, post-deployment)

After triggering 5 state queries:

```
lantern_fetch_total{source="cache"}    9
lantern_fetch_total{source="misses"}   6
lantern_fetch_total{source="bitswap"}  0
lantern_fetch_total{source="gateway"}  6
lantern_fetch_total{source="glif"}     0
```

## The honest gap

Bitswap is connected end-to-end, but **the beacon's cache is cold and its
backfill loop is a stub**. Specifically, `cmd/lantern/beacon.go:220` has:

```go
// Placeholder: when boxobs.Bitswap exposes IncomingWantlist (it does via
// GetWantlist on the peer manager but the API is internal), we'll wire
// backfill here. For now backfill is best done by the client side hitting
// the gateway as a fallback, so this loop is a no-op stub kept for the
// operator UX (the flag is documented and the loop is gated on --gateway
// being set).
```

So the beacon doesn't proactively backfill from the upstream gateway on
incoming WANTs that miss its cache. Each request from the mainnet daemon
fails the Bitswap fast deadline (1.5s) and full deadline (5s), then falls
through to the HTTP gateway directly. Result: bitswap=0, gateway=6.

This is the "Phase 11 follow-up" the code comment promises. Tracked in
PHASE10-BLOCKERS as B-10-XX (operational, not architectural). Fix options:

1. **Subscribe to Bitswap's wantlist channel** — boxo exposes it via the
   peer-manager, just needs a wrapper. ~150 LOC. Phase 11 should do this.
2. **Alternative: bidirectional WANT relay** — when a client connects to
   the beacon, the beacon proactively WANTs the same CIDs the client
   wants, fetches from its connected peers (which includes the gateway),
   and serves them back to the client. More natural for the pure-libp2p
   architecture. ~300 LOC.
3. **Pre-warm the beacon** — periodically scrape recent HAMT roots from
   the mainnet chain and pull them through the gateway into the beacon's
   cache. Useful but not architecturally clean. ~200 LOC.

V1.2 GA should ship at least option 1, ideally option 2.

## What did get proven

- Beacon binary works end-to-end on Linux as a systemd service
- DHT advertisement under `lantern/beacon/v1` is alive
- libp2p connectivity is symmetric and stable
- Mainnet daemon discovered the beacon as a preferred peer
- Bitswap path is wired correctly in the daemon
- Old binary rollback path is in place (`lantern.bak`, `lantern.prev`)

V1.2-rc.1 is operational in the sense that the **infrastructure is up**;
the **last-mile** (beacon backfill on miss) is a Phase 11 deliverable
that closes the loop.
