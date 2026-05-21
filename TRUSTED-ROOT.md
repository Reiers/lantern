# Trusted Root — Technical Spec

The trusted root is Lantern's central guarantee: a small in-memory tuple
that the rest of the node treats as ground truth for "what is the chain
right now." Every state read funnels through it.

This document defines the type, how it's produced, how it's persisted,
how reorgs invalidate downstream caches, and what guarantees it gives the
state accessor layer.

---

## 1. The type

```go
// In package github.com/Reiers/lantern/chain/trustedroot.

type TrustedRoot struct {
    Epoch                 abi.ChainEpoch     // tipset height
    TipSetKey             types.TipSetKey    // canonical key (sorted CIDs)
    TipSetCID             cid.Cid            // CID of canonical-form TipSetKey bytes
    StateRoot             cid.Cid            // ParentStateRoot of this tipset
    ParentMessageReceipts cid.Cid            // ParentMessageReceipts AMT root
    ParentWeight          types.BigInt       // for fork-choice
    BeaconRound           uint64             // latest drand round in tipset

    // F3 finality witness (nil for pre-F3-finalized tips during catch-up)
    F3Instance            uint64             // certs instance number
    F3Cert                *certs.FinalityCertificate

    // Bookkeeping
    AcceptedAt            time.Time          // wall clock at first acceptance
    AncestorRoots         []cid.Cid          // last N (default 100) state roots
                                             // for short-history lookups
}
```

The `AncestorRoots` slice is what lets `state.Accessor` answer queries at
recent past tipsets (within finality bound) without re-walking the header
chain each time.

### Invariants

T1. `TipSetKey` is the canonical-form key: block CIDs sorted ascending by
    (`Ticket`, `CID`), matching `types.NewTipSetKey`.
T2. `StateRoot == TipSet.Blocks()[0].ParentStateRoot` and every other
    block in the tipset has the same `ParentStateRoot`. (Filecoin tipset
    rule.)
T3. `Epoch == TipSet.Height()`. Null rounds are tolerated: the tipset's
    height can exceed the previous accepted root's height by more than 1.
T4. `F3Cert.GPBFTInstance == F3Instance` and `F3Cert.ECChain` contains
    `TipSetKey` (or one of its ancestors within the cert's chain segment).
T5. Once `F3Cert != nil`, the (TipSetCID, StateRoot) pair is **final**:
    no future TrustedRoot can override an ancestor at or before
    `F3Cert.ECChain.Base().Epoch`.

The struct is small (~500 bytes including ancestor slice). It's the
**only** trusted state the rest of the node sees.

---

## 2. Production pipeline

```
                +-----------------------------+
                | Genesis CID (hardcoded)     |
                | + initial power table       |
                +--------------+--------------+
                               |
                  (1) header-chain bootstrap
                               v
                +-----------------------------+
                | Header store                |
                | (epoch -> []BlockHeader)    |
                +--------------+--------------+
                               |
                  (2) F3 cert chain validation
                               v
                +-----------------------------+
                | F3 follower                 |
                | (instance -> Cert)          |
                | (epoch    -> finalized TS)  |
                +--------------+--------------+
                               |
                  (3) consolidate
                               v
                +-----------------------------+
                | TrustedRoot                 |
                +--------------+--------------+
                               |
                  (4) hand off
                               v
                +-----------------------------+
                | state.Accessor              |
                +-----------------------------+
```

### Step 1 — Header chain bootstrap

**Trust anchor:** the mainnet genesis CID
`bafy2bzaceapyg2uyzk7vueh3xccxkuwbz3nxewjyguoxvhx77malc2lzn2ybi`
(lifted from Lotus `build/genesis.go`), plus the genesis power table baked
into the F3 manifest.

**Procedure:**

1. Fetch headers genesis → current head. Two sources:
   - **HTTPS boot bundle** (preferred): a signed `.car.zst` from
     `https://bootstrap.lantern.reiers.io/mainnet/<latest-epoch>.bundle`.
     Bundle manifest is signed by a Lantern release key (Ed25519 pubkey
     ships in the binary).
   - **libp2p header sync**: subscribe to `/fil/blocks/<network>` and
     request historical ranges via a small custom protocol
     `/lantern/header-sync/1.0.0` (TBD; or run boxo's bitswap on the
     headers' CIDs directly). Slower but no trusted bundle source.
2. For each header in order from epoch 0 to head:
    - Verify CID by hashing CBOR-encoded block header.
    - Verify parent CIDs match the previous tipset's block CIDs.
    - Verify BLS aggregate signature over message-meta against
      header-declared BLS pubkeys. Uses `crypto/bls` (gnark-based).
    - Verify the per-block BLS or secp signature on the header itself
      (against the worker pubkey for the miner — pubkey lookup is itself
      a state query, so during bootstrap we **temporarily defer** the
      block-sig check until step 4 has the state root for that epoch, then
      verify in a second pass).
    - Verify the election proof (BLS over `(beacon-entry, sector-id,
      vrf-input)`).
    - Verify DRAND beacon entries against the configured drand chain
      using `chain/beacon`.
    - Compute `ParentWeight` and check `Weight` field accuracy if
      present.
3. Group blocks into tipsets by `(height, parents)`. Apply tipset rule
   T2 to derive `(epoch, tipsetKey, stateRoot)`.

After step 1 the header store contains every header from genesis to
current head with parent linkage cryptographically intact, but we don't yet
know which fork is "the" chain.

### Step 2 — F3 cert validation

Filecoin chooses a finalized fork via F3 (GPBFT-based finality gadget).
F3 emits `FinalityCertificate` objects per GPBFT instance, each:
- Identifies a tipset (`ECChain` field) as finalized.
- Lists the power table at that instance.
- Carries an aggregate BLS signature over the GPBFT decision from at
  least 2/3 of voting power.
- Encodes a power-table **diff** for the next instance.

**Procedure:**

1. Load the network manifest from `go-f3/manifest` (genesis power table
   + bootstrap epoch + network name). Set `prevPowerTable` to manifest's
   initial power table, `nextInstance = manifest.InitialInstance`.
2. Pull all certs from `nextInstance` to the latest via the
   `/f3/certexchange/0.0.1` libp2p protocol (`go-f3/certexchange`).
   Fallback: HTTPS, same gateway as the boot bundle.
3. Call `certs.ValidateFinalityCertificates(verifier, network,
   prevPowerTable, nextInstance, base, certs...)`. This is a single
   call in `go-f3/certs/certs.go:91` that:
   - Verifies each cert's BLS aggregate against the power table for its
     instance.
   - Confirms ≥2/3 of power signed.
   - Applies `PowerTableDiff` to derive the next instance's power table.
   - Checks chain continuity between certs (cert N's base = cert N-1's
     head).
   Returns: `(nextInstance, finalPowerTable, finalChain, err)`.
4. Persist each `(instance, cert)` to Badger
   (`f3:cert:<instance>` → cbor cert).
5. The cert chain's terminal `ECChain.Head().Key` is our finalized
   tipset key; mark it in the header store.

### Step 3 — Consolidate into TrustedRoot

The producer of `TrustedRoot` selects the tipset to publish:

```
selected =
    if F3Follower.HasCert(latestF3FinalizedEpoch)
       and HeaderStore.Has(latestF3FinalizedTipSetKey):
        max(latestF3FinalizedEpoch, headerStore.UnfinalizedHead-30)
    else:
        headerStore.UnfinalizedHead - 30
```

The "−30 epochs" buffer covers Filecoin's pre-F3 expected-fork-depth bound
(900-epoch finality fallback, but practical reorgs are <30 epochs).

Construction:

```go
func Consolidate(hs *HeaderStore, f3 *F3Follower) (*TrustedRoot, error) {
    tip, err := hs.TipSetAtCanonical(epoch)         // canonical fork
    if err != nil { return nil, err }

    cert, instance, _ := f3.LatestCertCoveringEpoch(epoch)

    return &TrustedRoot{
        Epoch:                 tip.Height(),
        TipSetKey:             tip.Key(),
        TipSetCID:             cidOf(tip.Key()),
        StateRoot:             tip.ParentState(),
        ParentMessageReceipts: tip.Blocks()[0].ParentMessageReceipts,
        ParentWeight:          tip.ParentWeight(),
        BeaconRound:           tip.Blocks()[0].BeaconEntries.Last().Round,
        F3Instance:            instance,
        F3Cert:                cert,
        AcceptedAt:            time.Now(),
        AncestorRoots:         hs.LastNStateRoots(100),
    }, nil
}
```

### Step 4 — Hand off to state.Accessor

`state.Accessor` holds an atomic pointer to the current `*TrustedRoot`.
Every read takes a snapshot at entry:

```go
func (a *accessor) GetActor(ctx, addr) (*types.Actor, error) {
    tr := a.current.Load()  // atomic.Pointer[TrustedRoot]
    return hamtLookup(ctx, a.cache, tr.StateRoot, addr)
}
```

If a reorg happens mid-read, the old `tr` is still hash-valid; the next
call sees the new root. No locking needed inside the accessor.

---

## 3. Steady state

After boot, the producer subscribes to:

- gossipsub `/fil/blocks/<network>` for new blocks
- gossipsub `/f3/granite/certificates/<network>` for new F3 certs

On each new block:

1. Validate as in Step 1.2 against in-memory parent state.
2. Insert into header store.
3. If it's part of a heavier tipset, update `unfinalizedHead`.
4. Recompute `TrustedRoot` if the selected tipset changed.

On each new F3 cert:

1. Run `certs.ValidateFinalityCertificates` with current
   `prevPowerTable` and the single new cert.
2. Persist + advance `prevPowerTable`.
3. Recompute `TrustedRoot` (will normally jump `epoch` forward by
   the F3 instance's covered epochs, usually 5-15 epochs).
4. Fire `OnFinalize(F3Cert.ECChain.Head())` so caches above can pin
   anything that depended on now-final state.

The producer publishes a small "HeadChange" event channel; this satisfies
`ChainNotify` in `CURIO-RPC-SURFACE.md` (Curio's `lib/chainsched`
subscriber).

---

## 4. Reorgs

Pre-F3 finality, Filecoin chains can reorg. F3 finality bounds the worst-
case reorg to "epochs not yet covered by an F3 cert" (~30s of recent
blocks in practice).

### Detection

A reorg is any new accepted tipset whose parent ancestry diverges from the
current `TrustedRoot.TipSetKey` ancestry within the F3-unfinalized window.

```go
func onNewTipSet(newTS *types.TipSet) {
    curr := tr.Load()
    if isAncestor(curr.TipSetKey, newTS.Parents()) {
        // forward extension, advance
        ...
    } else {
        // sibling fork — compare weight
        if newTS.ParentWeight().GT(curr.ParentWeight) {
            reorg(curr, newTS)
        }
    }
}
```

### Invalidation flow

A reorg invalidates two layers of downstream cache:

1. **State cache** (`state/cache`): every HAMT/AMT node whose CID was
   reachable from `oldStateRoot` **but not** from `newStateRoot` is now
   orphaned. We don't proactively delete — IPLD blocks are
   content-addressed, an orphaned node is harmless (it just won't be
   asked for again). LRU eviction will reclaim it.
2. **High-level result caches** (e.g., `miner.LoadState`, address
   resolutions cached longer than one tipset): explicitly invalidated by
   epoch. Each result-cache entry tags itself with `(tipsetCID,
   stateRoot)`; on reorg we sweep entries whose `tipsetCID` is no longer
   on the canonical chain.

### F3 cuts off reorgs

Once an F3 cert finalizes epoch E, the `selected` formula clamps
`TrustedRoot.Epoch ≥ E`. No new accepted tipset below E can change the
trusted root. We additionally:
- Mark the finalized branch in the header store; alternate branches at
  or below epoch E are pruned.
- Result-cache entries flagged `final = true` after their epoch is
  passed by F3; never invalidated thereafter.

### Reorg event API

```go
type HeadChange struct {
    Type string         // "apply" or "revert"
    Val  *types.TipSet
}

func (tr *Producer) Subscribe() <-chan []HeadChange
```

Format mirrors Lotus `api.HeadChange`. Curio's `lib/chainsched`
consumes this verbatim.

---

## 5. Persistence

Two Badger KV namespaces:

| Prefix              | Key              | Value                          |
|---------------------|------------------|--------------------------------|
| `hdr:`              | `<epoch>:<cid>`  | CBOR-encoded `BlockHeader`     |
| `hdr:idx:`          | `<epoch>`        | `[]cid.Cid` (blocks at epoch)  |
| `hdr:canonical:`    | `<epoch>`        | `cid.Cid` (selected block, or aggregate tipset key) |
| `f3:cert:`          | `<instance>`     | CBOR `certs.FinalityCertificate` |
| `f3:powertable:`    | `<instance>`     | CBOR power table snapshot      |
| `tr:current`        | `(singleton)`    | CBOR `TrustedRoot`             |
| `tr:final:`         | `<epoch>`        | finalized `(tipsetKey, stateRoot)` pairs |

On startup:
1. Load `tr:current`, sanity-check against header store and latest cert.
   If header store missing required blocks, re-bootstrap (this happens if
   the user wiped only part of the data dir).
2. Re-fetch any missing blocks/certs to catch up to live network.
3. Publish first `TrustedRoot` to `state.Accessor` before any RPC handler
   starts listening.

Cold-start (no `tr:current`): full bootstrap from boot bundle.

---

## 6. Guarantees to state.Accessor

The state accessor sees a single `*TrustedRoot` snapshot per read. From
that snapshot it gets:

G1. **CID hash guarantee.** Every fetched HAMT/AMT node is verified
    against the CID requested. A malicious peer cannot serve wrong bytes;
    worst case is unavailability.
G2. **Root authenticity.** `StateRoot` is the `ParentStateRoot` of a
    tipset that is either (a) F3-finalized via a cert chain rooted at the
    genesis power table, or (b) within ≤30 epochs of head and chosen by
    heaviest-tipset rule with full BLS signature validation.
G3. **Read consistency.** A single RPC handler invocation reads only
    from the snapshot it captured at entry. A reorg arriving mid-call
    cannot return mixed-tipset state.
G4. **Reorg notification.** Caches above the accessor receive
    `[]HeadChange` on every reorg in causal order
    (`revert` of old chain, then `apply` of new).
G5. **Finalization notification.** Result caches are told when an epoch
    becomes F3-final so they can promote entries to permanent.
G6. **Bounded staleness.** During steady state, the gap between live
    network head and `TrustedRoot.Epoch` is at most one block-time + one
    F3-instance-duration (typically <60s). During boot, the gap is the
    bootstrap latency (target ≤5 minutes).

### What it does NOT guarantee

- **Liveness on availability.** If no honest peer serves a HAMT node we
  need, the read blocks until timeout. That's by design (better than
  returning wrong data). Mitigation: run gateway nodes.
- **Pre-F3 finality.** During the F3-unfinalized window (latest ~30
  epochs), short reorgs may revert state. Callers who need finality
  should use `--final` mode (TBD CLI flag, Phase 4) that clamps reads to
  `F3Cert.ECChain.Head()` epoch.
- **Validity of off-chain data.** Beacon entries, sector commitments,
  proofs — all are validated by header sig + state structure, but
  Lantern does not re-execute messages, so it cannot detect a
  hypothetical bug in the network's own state-transition function. This
  is the same trust posture as Forest's RPC-only mode.

---

## 7. Failure modes

| Failure | Behaviour |
|---------|-----------|
| Boot bundle signature invalid | Refuse to start, prompt for libp2p-only sync. |
| Boot bundle older than F3 head | Accept, then catch up via gossipsub + cert exchange. |
| Cert chain breaks (bad signature, power-table mismatch) | Halt the F3 follower, alert. Header chain alone still produces TrustedRoot (without F3 finalization, just weight-based). |
| Header sig invalid mid-stream | Drop the bad block, mark peer as misbehaving. Tipset selection falls back to other valid block(s) at same epoch. |
| All peers refuse to serve a state CID | Read blocks until timeout (default 30s), return `ErrStateUnavailable`. Cache eviction retains the CID's request log so retries can pick fresh peers. |
| Local clock skew > 30s | F3 cert timestamps look invalid; we warn but accept (signatures are clock-independent). |
| Reorg deeper than F3 finality bound | **Impossible by F3 protocol assumption**; if observed, treat as catastrophic, halt the node, refuse new reads, page operator. |

---

## 8. Test plan

T1. Unit: golden-file test of `Consolidate` against a captured mainnet
    epoch with attached F3 cert.
T2. Property: random reorgs within a generated header chain; assert
    `HeadChange` events apply/revert in correct causal order.
T3. Integration: run against calibration network for 24h, verify every
    epoch's `TrustedRoot.StateRoot` matches a reference Forest node's
    `ChainHead().ParentState()` for the same epoch.
T4. Adversarial: serve doctored HAMT nodes from a custom Bitswap peer;
    confirm `state.Accessor` rejects on CID mismatch and switches peers.
T5. F3 regression: load known-good cert chain from mainnet, mutate one
    bit, confirm `certs.ValidateFinalityCertificates` errors.
T6. Recovery: kill the node mid-bootstrap, restart, verify it resumes
    from persisted header range without re-validating already-stored
    blocks.

---

## 9. Open questions

Q1. **Header-sync transport.** Build a small `/lantern/header-sync/1.0.0`
    libp2p protocol, or lean on Bitswap over block CIDs (one round-trip
    per header — slow at boot)? Suggest: implement the dedicated
    protocol; ~200 LOC.

Q2. **Trust anchor for the boot bundle.** Use a Lantern release Ed25519
    key (operationally simple) or fetch over TLS to a hard-pinned
    `*.lantern.reiers.io` (operationally even simpler, slightly weaker
    threat model)? Suggest: do both — sign with release key, serve via
    TLS, validate both. Defense in depth.

Q3. **`MinerCreateBlock` and the SP write path.** Block production
    requires speculative state execution to compute the new state root.
    Without a VM, Lantern can't validate the block it produces. Options:
    (a) defer SP block production until a pure-Go FVM exists; (b) trust
    one of our gateways to compute + return the new state root, only the
    final signature happens locally; (c) ship a partial pure-Go FVM only
    for the actor methods miners call (`SubmitWindowedPoSt`, `ProveCommit`,
    `PreCommit`). Suggest: go with (b) for V1 — keeps Lantern light, SP
    trusts one gateway operator for block packing only, F3 still ratifies
    or rejects the resulting block. Phase 7 decision point.
