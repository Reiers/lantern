# Phase 9 — Blockers, Decisions, and Known Limitations

Phase 9 was the V1.1 unlock: make Curio actually advance against
Lantern. The headline deliverables were `ChainNotify` end-to-end and a
real Curio binary binding to Lantern on a live SP-class host.

Both shipped. This file pairs with `docs/phase9-part-b-curio-bind.md`
(the binding-evidence) and `docs/SAFETY-CHECKLIST.md` (the operator-
facing safety review).

---

## Headline outcome

- ✅ `Filecoin.ChainNotify` is wired end-to-end via a head-change
  distributor over WebSocket. Validated against a Go client subscribing
  to `ws://localhost:1235/rpc/v1` for 6 minutes against live mainnet:
  14 apply events delivered, epochs 6036012 → 6036025 inclusive (1
  current + 13 advances at the expected ~30s mainnet cadence).
- ✅ Real Curio 1.28.1 binary booted against Lantern as its
  `FULLNODE_API_INFO`, on lex (Linux/amd64, Ubuntu 24.04). Curio's
  task scheduler advanced normally: `ExpMgr` + `AlertManager` cycles
  every loop, RPC server up on 12300, GUI on 4701.
- ✅ Method coverage: 71/71 from CURIO-RPC-SURFACE.md spec. The two
  outstanding stubs in Phase 8 (`ChainNotify`, `StateAccountKey`) now
  return real bytes. Bonus: `StateMinerSectorAllocated` and
  `StateMinerInitialPledgeCollateral` also landed.
- ✅ Safety checklist (8 gates) written and operationalised.

V1.1-rc.1 is ready. The remaining items below are V1.1 GA polish.

---

## What Phase 9 shipped

### Part A — ChainNotify (CRITICAL → DONE)

- New package `chain/headnotify`: fan-out distributor with bounded
  per-subscriber buffers (default 64), drop-slow semantics matching
  Lotus, multi-subscriber fan-out, reorg-aware event sequencing
  (`current → apply* → revert* / apply*` shape).
- `chain/header/store` wired into `cmd/lantern daemon`:
  - Opens BadgerDB at `~/.lantern/headerstore` (configurable).
  - Starts a `Sync` agent backed by `glif.Client` polling every 6s.
  - Hooks the store's `OnHeadChange` callback into the distributor.
  - New daemon flags: `--header-store <path>`, `--sync-interval`,
    `--notify-buf`, `--no-header-store`.
- `Sync` agent hardening:
  - New `BootstrapDepth` option (default = MaxBacktrack). Cold start
    uses 3 to complete in seconds against rate-limited Glif; ongoing
    polls use MaxBacktrack to absorb reorgs.
  - `backfillParents` capped at MaxBacktrack (was: walks to genesis on
    a fresh store — millions of epochs, hung the daemon).
  - Per-epoch fetch errors are now recorded (lastErr) instead of
    silently swallowed.
- `glif.Client.rpcCall` resilience: switched to `json.Decoder` so
  occasional doubled response bodies from Glif's edge no longer error
  the whole sync cycle.
- `rpc/handlers.ChainAPI`:
  - `HeadNotify` field; `ChainNotify` routes through it when wired,
    falls back to the legacy single-subscriber channel otherwise.
  - `ChainHead` now prefers the header store's head over the synthetic
    placeholder.
  - `ChainGetBlock` consults the header store before erroring.

**Coverage:** 9 new test cases in `chain/headnotify/notify_test.go`
(subscribe-current, custom-publish fanout, slow-subscriber drop,
ctx-cancel cleanup, store→OnHeadChange→event fanout, 10-subscriber
fanout). All pass with `CGO_ENABLED=0 go test ./...`.

### Part B — Real Curio binary against Lantern (DONE)

- Cloned `filecoin-project/curio@a07bbf7` on lex
  (`lexluthr@37.202.57.171`), built `curio` v1.28.1 binary with
  `FFI_USE_OPENCL=1 CURIO_NOSUPRASEAL=1` (CUDA not available, OpenCL
  works).
- Installed YugabyteDB 2025.1.0.0 as the database backend (full
  PostgreSQL + YCQL — plain Postgres lacked the Cassandra-protocol
  port 9042 Curio's index store requires).
- Configured Curio: `curio config new-cluster f01889` with
  `FULLNODE_API_INFO` pointing at Lantern. All 30+ schema migrations
  applied cleanly.
- Started `curio run --layers gui --nosync`. Task scheduler
  initialised with the expected task list (SendMessage, BalanceMgr,
  ExpMgr, IPNI, Indexing, PDPIndexing, PDPIpni, FixRawSize,
  PDPv0_Indexing, PDPv0_IPNI, AlertManager, PieceCleanup).
- Watched for 10+ minutes. Steady-state observations:
  - Curio's RPC server stayed up on 127.0.0.1:12300.
  - GUI accessible on http://localhost:4701.
  - `ExpMgr` and `AlertManager` tasks ran on each loop.
  - Lantern's header sync advanced 1 tipset per 30s mainnet epoch.
  - No RPC errors from Curio against Lantern.
  - One transient Glif "decode envelope" warning during the bake,
    fixed in commit (json.Decoder switch).

**Caught and fixed in-flight:** Lantern's `Version.APIVersion` was
0x000d0900 (encoded as 0.13.9). Lotus's `FullAPIVersion1.EqMajorMinor`
gates on 0x020300 (2.3.0). Changed to 0x020300 and Curio happily
bound. See `docs/phase9-part-b-curio-bind.md` for the full transcript.

### Part C — 71/71 method coverage (DONE)

- `StateAccountKey`: was returning `ErrNotImpl` since Phase 5. The
  account-state loader was already in
  `state/actors/loaders.go::LoadAccount` (Phase 6), just never wired
  into the handler. Three lines of glue, plus the existing pubkey-
  passthrough for non-ID inputs.
- `StateMinerSectorAllocated`: new method. Bitfield IsSet lookup
  against the miner's `AllocatedSectors` CID. Curio uses this for
  pre-flight sector-number probes.
- `StateMinerInitialPledgeCollateral`: thin shim over the existing
  `StateMinerInitialPledgeForSector`, deriving sector duration from
  `pci.Expiration - pci.SealRandEpoch`. Verified deal weight is
  conservative zero — Curio recomputes precise value at submit time;
  what matters for the preview is the order of magnitude.

### Part D — Safety checklist (DONE)

`docs/SAFETY-CHECKLIST.md` — 8 gates / dangerous-by-default
operations, what the gate looks like in code, how to verify, and
what the operator must do. Includes a status table.

5 gates locked ✅, 1 ⚠ operator-discipline (wallet encryption), 2 🟡
defer-to-V1.1-GA (anchor staleness enforcement, header↔F3 cross-check).

### Part E + F — Release prep + perf (deferred)

V1.1-rc.1 is ready without these. They're nice-to-have for V1.1 GA.

---

## What Phase 9 did NOT close (V1.1 GA priorities)

### B-9-11 — Wallet keystore: refuse-to-start when crypto disabled

**Severity: medium.**

Currently the wallet silently accepts plaintext key files when
`LANTERN_PASS` is unset. Should refuse-to-start if at least one key in
the keystore is plaintext but a passphrase is configured (or vice
versa). One-shot upgrade command:
`lantern wallet rekey --new-pass=$NEW`.

Estimated effort: ~50 LOC + a `wallet/keystore.go` integration test.

### B-9-12 — F3 anchor freshness enforcement

**Severity: medium.**

`chain/f3/anchor.Anchor.CapturedAt` is set but the daemon does not
check it at startup. Phase 9 Part D documents the gap; the fix is:
- Warn when `Now() - CapturedAt > 30d`.
- Hard-fail when `Now() - CapturedAt > 60d`.
- Surface in `lantern info` output.

Estimated effort: ~30 LOC + tests.

### B-9-13 — Release CI / signed binaries

**Severity: medium (V1.1 GA gate).**

`.github/workflows/release.yml` to build on tag push for linux/amd64,
linux/arm64, darwin/arm64. Embed the build timestamp in the version
string. Tag `v0.1.0-rc.1` locally (per task spec, NOT pushed yet —
review first).

Estimated effort: ~1 hour for the workflow + first dry-run release.

### B-9-14 — Deep block validation in net/blockpub

**Severity: low (gated path inert in V1.1).**

`net/blockpub.validateIncoming` only does superficial validation
(presence checks). The deep validation (winning POSt + parent tipset
+ randomness + miner state) is the consumer's responsibility. When
Phase 10 wires a consumer that treats blockpub output as canonical,
deep validation must land first. Tracked as a hard prerequisite.

### B-9-15 — Header store ↔ F3 cert cross-check

**Severity: medium.**

The header sync agent picks canonical-at-epoch from the configured
RPC source. The F3 cert subscriber walks F3 finality independently.
Neither cross-checks the other. A malicious upstream RPC could push a
canonical fork that contradicts the F3 finality the local node knows
about.

Fix: after each `SetHead`, check the F3 subscriber's most-recent
finalized tipset key against the header store's canonical at that
epoch. Mismatch → log alert + refuse to advance until manual review.

Estimated effort: ~80 LOC + integration test.

### B-9-16 — Header store sync: doubled-response root cause

**Severity: low (worked around).**

Phase 9 worked around it with `json.Decoder`. The root cause is Glif's
edge occasionally returning the response body doubled under sync load
(observed: 60 requests/30s spike on cold start). Possible explanations:
- Kong proxy retry that doesn't dedupe.
- HTTP/2 stream-multiplexing bug in Glif's edge.
- TCP-level connection re-use across concurrent in-flight requests.

Not Lantern's bug, but worth filing upstream with Glif when budget
allows.

### B-9-17 — Curio's `IPNI` task connectivity

**Severity: low (Phase 9 Part B observation).**

Curio's IPNI task started but didn't make outbound progress during the
10-minute bake — likely because the lex host has no public-facing
miner address and IPNI advertisement requires the miner's `Multiaddrs`
field to be set. Not a Lantern issue. Document in the Curio
integration runbook so operators don't think IPNI is broken when it's
actually just waiting on miner-info config.

### B-9-18 — `dev/dri/renderD128` permission spam

**Severity: cosmetic.**

Curio probes `/dev/dri/renderD128` (a GPU device) at every command,
which the lex `lexluthr` user can't read. Six "Permission denied" log
lines per command. Curio itself ignores the failure. Workaround:
`sudo usermod -aG render lexluthr`. Document but don't gate the
release.

### Carry-overs from Phase 8 still open

- B-9-8 — F3 cert subscriber persistence (still: restarts re-walk
  from the embedded anchor). Track for V1.1 GA.
- B-9-9 — `StateSearchMsg` cross-test fixture against mainnet
  message-receipt AMTs. Track for V1.2.

---

## Method coverage table — Phase 9 update

Phase 8 left us at 69/71. Phase 9 closed both remaining stubs.

| Method                                | Status | Notes |
|---------------------------------------|--------|-------|
| `Filecoin.ChainNotify`                | ✅ | Live-validated 6 min against mainnet via WebSocket. |
| `Filecoin.StateAccountKey`            | ✅ | Wired existing Phase 6 LoadAccount. |
| `Filecoin.StateMinerSectorAllocated`  | ✅ | Bonus (Curio pre-flight). |
| `Filecoin.StateMinerInitialPledgeCollateral` | ✅ | Bonus (Curio PreCommit preview). |

**71/71 of the spec, plus 2 bonus.**

The remaining items in PHASE8-BLOCKERS.md's coverage table:
- `Filecoin.SyncSubmitBlock`: intentionally gated (`AllowBlockSubmit`,
  see SAFETY-CHECKLIST.md §1). Not a gap, that's the policy.

---

## V1.1 release readiness verdict

| Surface                          | V1.1-rc.1 ready? |
|----------------------------------|------------------|
| State reads                      | ✅ |
| Wallet signing                   | ✅ |
| Gas estimation                   | ✅ |
| Mempool                          | ✅ |
| Header chain validation          | ✅ when wired (default since Phase 9) |
| F3 finality                      | ✅ (subscriber persistence still B-9-8) |
| DRAND beacons                    | ✅ |
| VM execution (Send)              | ✅ |
| VM execution (non-Send)          | 🟡 via bridge only |
| Paych voucher signing            | ✅ byte-exact with Lotus |
| Block production                 | 🟡 gated behind AllowBlockSubmit |
| Live block gossip                | ✅ inert subscriber |
| DHT peer discovery               | ✅ |
| **ChainNotify head-change ticker** | **✅** |
| Header store wired in daemon     | ✅ |
| **Real Curio binary advancing**  | **✅** |

**Recommendation:** cut V1.1-rc.1 from the current main, leave the tag
local for Nicklas's review, do not push. Run a 24-hour bake on lex
(Curio + Lantern continuous) before promoting to V1.1 GA.

---

## Files touched in Phase 9

- `chain/headnotify/{notify,notify_test}.go` (new) — Part A
- `chain/header/store/sync.go` — Part A hardening
- `cmd/lantern/main.go` — daemon wiring
- `rpc/handlers/chain_api.go` — HeadNotify field, ChainHead/Block,
  Version.APIVersion fix, StateAccountKey wire
- `rpc/handlers/state_miner.go` — StateMinerSectorAllocated
- `rpc/handlers/state_compute.go` — StateMinerInitialPledgeCollateral
- `api/fullnode.go` — interface additions
- `net/glif/client.go` — json.Decoder resilience
- `docs/SAFETY-CHECKLIST.md` (new) — Part D
- `docs/phase9-part-b-curio-bind.md` (new) — Part B evidence
- `PHASE9-BLOCKERS.md` (new) — this file

Every commit is a single logical change, granular history preserved
for review. Nothing pushed to origin (per task constraint).
