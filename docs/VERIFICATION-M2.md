# Milestone 2 verification: external operator runs stock Curio-PDP on Lantern

Recorded 2026-10-31 from operator report #191 and on-chain data.

Milestone 2 criteria:

| Criterion | Where | Status |
|---|---|---|
| BUILD.md walks a new operator from a clean machine to a built, verified Lantern binary | [BUILD.md](../BUILD.md) | Met (operator downloaded the v1.10.1-rc1 release binary; maintainer clean build and byte-for-byte repro recorded below) |
| RUN-PDP.md walks a new operator to a working Lantern daemon serving stock Curio-PDP | [RUN-PDP.md](../RUN-PDP.md), [golantern.io/test](https://golantern.io/test/) | Met (see operator run) |
| An external operator followed both guides and reported back | Operator section below, report #191 | Met |
| This document records identity, hardware, build time and issues | This document | Met |
| Every friction point is fixed or filed as a public issue | Friction table below | Met (every row fixed in-repo or linked to a public issue) |

## Operator

| | |
|---|---|
| Name or handle | amughal (provider name "Mongo2Stor Calibnet") |
| GitHub | @amughal |
| Organisation (if any) | Independent storage provider |
| Relationship to the Lantern project | Independent calibration storage provider; not a Lantern maintainer |
| Operator report issue | #191 |
| Dates of the run (UTC) | 2026-10-30 to 2026-10-31 (still running at time of writing) |

## How this run differs from the docs (and why it is a stronger test)

The operator already ran a native (non-Docker) Curio-PDP provider on calibration inside an LXC container, backed by a Lotus calibration node. Instead of building a fresh Docker stack (RUN-PDP.md steps 5 to 10), the operator:

1. Built/installed Lantern v1.10.1-rc1 and ran `lantern init --network calibration --no-wallet` and `lantern node-type pdp --network calibration` (RUN-PDP.md step 3).
2. Ran `lantern daemon --network calibration --listen 127.0.0.1:<port>` in the same container as Curio (loopback, so no `--allow-remote-rpc` needed).
3. Pointed the existing Curio's `FULLNODE_API_INFO` at Lantern, then removed the Lotus entry so Lantern was Curio's **only** chain node (Curio Chain page: RPC nodes 1/1, Lantern v1.10.1-rc1, in sync).
4. Kept the existing provider registration, PDP key and data sets: no re-registration. The PDP key stays in Curio's database; Lantern holds no keys.

This is the path most existing storage providers would take ("switch my Curio from Lotus to Lantern"), and it exercises Lantern against a provider with real existing client data sets rather than a fresh test provider.

## Environment

| | |
|---|---|
| Machine | LXC container (VM) |
| CPU | 24 cores |
| RAM | 512 GB DDR4 (host) |
| Disk | not reported |
| OS | Ubuntu 22.04.5 LTS |
| Network / HTTPS | Existing public PDP endpoint (unchanged) |
| Curio | Native build (calibnet), from source |
| Previous chain node | Lotus (calibration), removed from FULLNODE_API_INFO at the switch |

## Build or download

| | |
|---|---|
| How Lantern was obtained | Downloaded release binary (RUN-PDP.md step 2.2) |
| Lantern tag | v1.10.1-rc1 (Curio Chain page reports v1.10.1-rc1) |
| Total build time | 0 (binary download). Maintainer clean-room build from source: 84 s |
| Matches published release hash | Release assets are byte-reproducible: independent rebuild of v1.10.1-rc1 with scripts/repro-build.sh matched the published linux/amd64 hash |

## Run

| Step | Result | Notes |
|---|---|---|
| `lantern init --network calibration --no-wallet` + `node-type pdp` | pass | as reported |
| Daemon running, reachable by Curio | pass | Curio Chain page: reachable, in sync, 1 epoch behind Lotus before the switch |
| Curio switched to Lantern only | pass (2026-10-30 ~19:40 UTC) | RPC nodes 1/1 |
| Proofs after the switch | pass | operator reports proofs landing; provePossession txs confirmed on chain (below) |
| Time from switch to first proof | not reported | existing provider with active data sets; proving continued across the switch |

## PDP evidence (calibration, operator)

| Item | Value |
|---|---|
| Provider ID | 32 (calibration ServiceProviderRegistry) |
| Example provePossession txs (Lantern only, 2026-10-31) | `0xdeb226db99b142b50d8e0a486b1401581a8fa531f65a301d33c44f718bcde343`, `0x0810a45d72a0f8b6f7533c2ea2a0d5b9f6d62413fe181277bcf728934186e9a8`, `0xee765b70f50f0d876a2172dc6dd2b5c3e64581886d7be4f69fa64b359c5cef9c` |
| Volume | ~230 provider txs/hour on 2026-10-31, about 70% succeeding; the reverted provePossession txs fail with `ProvingPeriodPassed` (proof landed after the deadline), see friction items 4 and 5 |
| Curio Chain page | Lantern only: `/ip4/127.0.0.1/tcp/2234/http`, reachable, v1.10.1-rc1 Lantern+calibration (screenshot in the operator conversation; sync-lag observation is friction item 4) |

## Maintainer reference run (for comparison)

Fresh Docker path exactly as in RUN-PDP.md, on maintainer hardware (details in #104):
- 2026-10-26: build from clean container 84 s; FWSS registration, data set creation and proving-period init all through a bridge-off Lantern.
- 2026-10-27: 13 consecutive prove windows per data set in 24 h, no manual intervention, 26/26 provePossession txs status 1.
- 2026-10-30: the same provider serves ~29 third-party client data sets (organic synapse-sdk traffic), 231 provePossession txs in 24 h. On-chain reverts in that window were client-side (`DataSetPaymentBeyondEndEpoch` on terminated data sets, rail settlements after termination), not Lantern.
- Mainnet: a test provider registered through Lantern and received a third-party client data set; proofs succeed (not an M2 requirement).

## Friction

| # | Step | Description | Severity | Resolution |
|---|---|---|---|---|
| 1 | Docs | Guides assume a fresh Docker stack; operators with an existing native Curio need a "switch from Lotus" path | doc | Fixed: #189 + golantern.io/test callout |
| 2 | Run | Curio `StorageGCMark`: `get finality tipset: header/store: not found` on a Lantern younger than ~24 h (Curio asks for head-2891) | minor (GC only) | Fixed: #190 (verified header backfill + startup prefetch) |
| 3 | Run | Wallet question: does the PDP wallet need importing into Lantern? (no; Curio signs) | doc | Fixed: #189 (keys stay in Curio) |
| 4 | Run | Curio Chain page often shows Lantern "No, slow (55s behind)" while Lotus stays in sync; a share of proofs land after the proving deadline (`ProvingPeriodPassed`) | high | Filed: #192 (top priority for the next release candidate) |
| 5 | Run | Late or otherwise doomed txs were broadcast and reverted on chain: Lantern's `eth_estimateGas` did not execute calls, so Curio's pre-flight never caught reverts | medium (wastes gas) | Fixed: #195 (closes #193) |
| 6 | Run | Curio `i/o timeout` on `getNextChallengeEpoch` under read bursts (also seen on the maintainer provider) | medium (retried) | Filed with #192 (fix in progress) |

Friction from the maintainers' dry run (F1 to F17) and its resolutions: see the git history of this file and #104 / #184. Highlights: NV29 support (#172), receipts with logs (#173, #177), FEVM opcodes (#178), persistent tx index (#179), eth blocks with txs (#180), operator-aware bootstrap quorum (#176), reproducible releases (#174), docs (#175, #186, #187, #188). Under heavy Curio read load Lantern occasionally exceeded Curio's RPC timeout (retried successfully): friction item 6.

## Operator statement

> "Nothing [blocked me], worked end to end." (operator report #191, "How much did this block you?")

## Maintainer sign-off

| | |
|---|---|
| Verified by | Nicklas Reiersen |
| Date | 2026-10-31 |
| Evidence checked | Operator report #191; provider 32 transactions on a calibration explorer (successful provePossession hashes above); Curio Chain page screenshot showing Lantern as the only RPC node |
