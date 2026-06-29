# Lantern v1.8.1 — sync resilience + chain-watcher fixes

**Update if:** you run a Lantern node behind Curio (or any Lotus-API consumer
that watches the chain), especially in a zero/low-Glif configuration. This
release fixes two distinct bugs that, together, made a node fall behind live
head and then stall its consumer's chain watcher. Both were reported from the
field. No protocol or wire changes.

## Highlights

### Header backfill off the Glif critical path (#53)
Live blocks arrive over gossipsub fine, but when a block lands at `head+N`
(N>1) the **parent backfill** was served by Glif `FetchBlock` calls with an 8s
timeout, in *both* the polling sync agent and the gossip ingestor. When Glif was
slow or rate-limited, those calls timed out, `backfillFail` climbed, the head
could not advance contiguously, and the node desynced — surfacing downstream as,
for example:

```
cannot draw randomness from future epoch <E> (head <E-thousands>)
```

(The PDP prover asks for randomness at an epoch the stalled node hasn't reached
yet, so the request is correctly refused — a *symptom* of the desync, not a
proving bug.)

Backfill is now served from the combined **bitswap + gateway** fetcher (the same
content-addressed, CID-verified fetcher already running for state reads), with
Glif demoted to last resort. `HeadEpoch` / `TipsetCIDsByHeight` stay RPC-shaped
(gossipsub already supplies live heads), so during normal operation Glif is off
the head-advancement path entirely. The fetcher is resolved lazily so the
sync/gossip paths see the bitswap-enabled fetcher (rebuilt after libp2p comes
up), not a stale snapshot.

### ChainGetTipSet served from the header store (#68)
`ChainGetTipSet(key)` only ever resolved the synthetic current head and returned
`ErrTipSetNotFound` for any other key — **even with a populated header store
wired**. Its siblings (`ChainGetTipSetByHeight`, `ChainGetBlock`, the randomness
path) all already fell through to the header store; only the by-key lookup was
left stubbed.

Curio's `message/watch.go` and `deps/apiinfo.go` request specific recent
(non-head) tipset keys. Before this fix they were refused, producing a tight
error loop:

```
lantern: tipset not in local store (only current head is cached in V1)
failed to get tipset: chain: lantern: tipset not in local store ...
no new tipset in CurioChainSched.update
```

`ChainGetTipSet` now falls through to a new `Store.GetTipSet(key)` that
reassembles any persisted tipset directly from its block headers (returning
`ErrNotFound` only when a constituent block is genuinely missing). This is
distinct from #53: #53 is the node falling behind; #68 surfaces even on a
healthy, synced node.

## On `--vm-bridge-rpc` as a default

A tester asked whether `--vm-bridge-rpc https://api.node.glif.io/rpc/v1` should
be added by default to dodge these failures. It is a legitimate **fallback** and
fine to set today if you need resilience right now — but it is **not** the
default, by design. Defaulting it on would route every node silently back
through Glif and mask exactly the bugs above. The fixes here address the root
causes so the bridge stays optional.

## Upgrade notes

- No configuration changes required. Drop in the new binary and restart.
- No new external dependencies.

## Verification

CGO-free build, `go vet`, `gofmt`, and the hermetic test suite
(`-short`, `LANTERN_OFFLINE=1`) are green across the module. New unit tests
cover the #68 by-key lookup (head, non-head historical, empty-key, and
missing-block cases); the #53 adapter is covered by its existing suite. The live
mainnet node was not touched during development.
