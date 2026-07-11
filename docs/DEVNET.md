# Running Lantern against a local devnet

Lantern has first-class support for a locally-hosted Curio devnet
(`curio-fork/docker/`, `make devnet/up`). This is the fastest way to
exercise the full Lantern + Curio Core stack against real chain state
without waiting for calibration or paying mainnet FIL.

## Trust posture

The operator OWNS the devnet: they ran `make devnet/up` on their own
machine, and the devnet's genesis was generated at that moment. So:

- **Single-source trust is honest.** The devnet lotus is the canonical
  head source. Multi-source quorum is skipped (there is no second
  independent source available).
- **F3 is not required.** The docker devnet does not run F3 by default,
  and F3-based finality would be meaningless against a single-node
  chain anyway. Lantern's daemon skips F3 assertions on `--network
  devnet`.
- **`--insecure-anchor` is implicit.** The daemon flips it on
  automatically and prints the reason at boot.
- **The lotus RPC is HTTP.** `--insecure-gateway` is also implicit for
  the same reason. TLS on localhost adds no security.

## Quick start

```
# 1. Start the docker devnet (Curio side).
cd $CURIO_REPO
make devnet/up
# Wait for http://localhost:4701 (Curio GUI) to be responsive.

# 2. Discover the devnet's identity + seed Lantern's trust anchor.
lantern devnet-init --lotus-rpc http://127.0.0.1:1234/rpc/v1
# Writes <data-dir>/devnet/{devnet-config.json, bootstrap-anchor.json}.

# 3. Start Lantern against the devnet.
lantern daemon --network devnet
# Auto-loads devnet-config.json, points --gateway at the devnet lotus,
# skips F3, skips multi-source quorum, opens RPC on 127.0.0.1:1234.
```

## What `devnet-init` writes

Two files under `<data-dir>/devnet/`:

**`devnet-config.json`** — runtime identity of the devnet, populated
from three lotus RPC calls:

- `Filecoin.StateNetworkName` → `networkName` (drives gossipsub
  topics: `/fil/blocks/<name>` + `/fil/msgs/<name>`, and the DHT
  protocol prefix).
- `Filecoin.ChainGetGenesis` → `genesisCID` (drives the
  `/fil/hello/1.0.0` handshake identity).
- The `--lotus-rpc` argument → `lotusRPC` (used as both the boot-time
  head source and the daemon's steady-state `--gateway` fallback).

Bootstrap peers are empty by default — the single-cluster docker
devnet has lotus dial itself. You can pass `--bootstrap-peers
<multiaddr>,...` if you're pointing Lantern at a devnet on a different
host.

**`bootstrap-anchor.json`** — the same on-disk shape as `lantern init`
writes for mainnet/calibration, seeded from `Filecoin.ChainHead` at
the moment `devnet-init` ran. `Instance` is 0 (there's no F3 instance
to record) and `Network` is `"devnet"`. Subsequent daemon boots use
the same load path as calibration + mainnet — including #118
auto-refresh once we teach it to skip devnet (currently `--auto-stale-reset`
is flipped off for devnet since the single-source probe is meaningless).

## Re-init after tearing down the devnet

`make devnet/down && make devnet/up` regenerates the devnet's genesis.
The previous `devnet-config.json` is stale (wrong genesis CID) and
`bootstrap-anchor.json` is stale (points at the old chain). Refresh
both with:

```
lantern devnet-init --lotus-rpc http://127.0.0.1:1234/rpc/v1 --force
```

## What this doesn't do

- **No mining on the Lantern side.** The devnet's lotus-miner mines.
  Lantern is a light client here just like it is against mainnet.
- **No F3 verification.** If you need F3 semantics, use calibration.
- **No public devnet.** This is your own machine only. Don't share the
  RPC endpoint publicly.

## Testing tips

- Block time is 4 seconds (`build/params_2k.go` in the curio fork).
  #118 auto-stale-reset tests that would take 24 hours to trigger on
  mainnet-cadence take a few minutes on devnet.
- The devnet spins up FWSS + PDP + Multicall3 + registry contracts on
  first boot (contracts-bootstrap container). Real deals + real
  proving work, so you can end-to-end MpoolPushMessage → gossipsub →
  mine → StateSearchMsg → #119 journal drop without touching
  calibration.
- If Lantern's `MpoolPush` returns errors, the devnet lotus's
  gossipsub mesh may not have formed yet. Give it 30 seconds after
  boot.

## Related

- **#118** bridge-off boot auto-refresh stale anchor
- **#119** durable mpool pending across restart
- `curio-fork/documentation/en/docker-devnet.md` — the Curio side of
  the setup
