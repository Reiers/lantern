# Phase 4 — Blockers & Provisional Decisions

Same shape as PHASE1-BLOCKERS.md and PHASE2-BLOCKERS.md. Each item is a
decision made during Phase 4 that Nicklas should review before Phase 5
begins.

---

## B13. Real Lotus binary not available locally for the end-to-end test

**Symptom.** Part E of Phase 4 calls for downloading a release `lotus`
binary and running `lotus chain head` against Lantern. Reality:
- `lotus` is not in `brew` (cask removed years ago — `Error: cask "lotus"
  does not exist`).
- The release tarball is CGo + filecoin-ffi; build-from-source needs
  `pkg-config`, `rust`, OpenCL, and ~20 GB of object files. Not realistic
  inside a 4-6h budget.
- The published binaries on github.com/filecoin-project/lotus/releases are
  Linux-only for amd64 + arm64; this Mac mini is darwin/arm64. No
  prebuilt Mac binary exists.

**Provisional decision.** Built a self-contained client
(`cmd/lantern-lotus-compat-test`) that uses `github.com/filecoin-project/
go-jsonrpc.NewMergeClient` — the **same library Lotus uses** — with the
**same FULLNODE_API_INFO parsing** (`<token>:<multiaddr>`), the **same
typed `Internal` struct pattern** (Lotus' `FullNodeStruct.Internal`), and
the **same `Filecoin.<Method>` dispatch**. If a Lotus client can decode
our responses, then so can the real `lotus` binary — they share the
client code path verbatim.

Output (against the live daemon on `127.0.0.1:11234`, talking to mainnet
via Glif fallback at epoch 6035570):

```
  OK   Filecoin.Version                  {lantern/0.4.0 (lotus-compat) 854272 30}
  OK   Filecoin.ChainHead                height=6035570 cids=1
  OK   Filecoin.StateNetworkVersion      27
  OK   Filecoin.StateNetworkName         mainnet
  OK   Filecoin.StateLookupID(f099)      f099
  OK   Filecoin.StateGetActor(f099)      balance=42207882236618625934736468 code=bafk2bzaceb4as5yyhjfkvxgooum37uvm5gbjr4dtbpxmqnpvvbjfpu5qouii4
  OK   Filecoin.WalletBalance(f099)      42207882236618625934736468
  OK   Filecoin.WalletList               [f1w3ctc2... f3vuxoabaxyytje... f410fdaszm...]
  OK   Filecoin.WalletBalance(<wallet>)  0
```

**Recommendation.** Phase 7 (Curio compatibility hardening) is the right
time to test against the **real Lotus binary on a Linux box** —
sp.reiers.io's Curio host or the Hetzner gateway box. Both are linux/arm64
or linux/amd64 and can install the lotus release tarball trivially. Until
then, the go-jsonrpc-based compat test is the strongest evidence we can
produce locally.

---

## B14. Lantern gateway TLS handshake failure during Phase 4

**Symptom.** `gateway.lantern.reiers.io` returns
`remote error: tls: handshake failure` from new clients. The IP
(157.180.16.39) still answers ICMP and SSH; only the TLS endpoint is
broken. Direct `curl https://gateway.lantern.reiers.io/state/root`
reproduces.

**Provisional decision.** The CLI now falls through to Glif RPC
(`api.node.glif.io/rpc/v1`) whenever the primary gateway is unavailable.
Added a new `net/glif` package satisfying `state/hamt.BlockGetter` via
`Filecoin.ChainReadObj`. Block bytes are CID-verified locally before
insertion into cache, so Glif is still treated as a dumb-pipe block
server (no trust anchor change vs. Phase 2's gateway-via-Glif pattern).

The combined fetcher chain in `cmd/lantern` is now:
1. local cache (`hamt.MemBlockStore`)
2. lantern gateway (5s timeout, may fail-fast)
3. glif RPC (20s timeout)

**Recommendation.** Investigate the gateway TLS cert separately
(probably an expired Let's Encrypt cert post-restart on the Hetzner box).
Either way, the failover path is now in place; future gateway flakes are
non-fatal for the user.

---

## B15. Synthetic head TipSet has placeholder fields

**Why.** `TrustedRoot` is small by design — it carries `(epoch, stateRoot,
parentMessageReceipts, parentWeight)` plus the TipSetKey, but **not**
the full BlockHeader objects of the head's blocks. `ChainHead` must
return a `*types.TipSet`, which in turn needs at least one BlockHeader.

**What Phase 4 ships.** A synthesised single-block tipset with the
trusted fields filled in (`Height`, `ParentStateRoot`,
`ParentMessageReceipts`, `ParentWeight`) and **placeholder values** for
fields TrustedRoot doesn't carry:

- `Miner` = `f00` (placeholder)
- `Ticket.VRFProof` = `"lantern-synth"`
- `BLSAggregate`, `BlockSig` = 96-byte zero signatures
- `Timestamp` = current wall clock
- `Parents` = empty
- `Messages` = `ParentStateRoot` (placeholder)

A Lotus/Curio client reading `ChainHead().Height()` and
`ChainHead().ParentState()` gets the right answers. A client trying to
**re-validate block signatures** would fail (because the placeholder
signatures don't match), but no Curio code path does that — Curio trusts
that its `FULLNODE_API_INFO` is correct and uses ChainHead only for
height/state-root extraction.

**Recommendation.** Phase 5 should persist full BlockHeaders alongside
the TrustedRoot in `chain/header.HeaderStore`, then have `ChainHead`
look them up. This is also what unblocks `ChainGetBlock`,
`ChainGetTipSetByHeight` (with real header walk), and
`ChainGetMessage`-by-tipset workflows.

---

## B16. MpoolPush is wired but not connected to a real gossipsub host

**Why.** Phase 4 spec calls for a libp2p host + gossipsub publisher on
`/fil/msgs/<network>` so `MpoolPush` actually submits messages to the
network. Lantern has the wiring (`net/mpool` interface, `MpoolPublisher`
in handlers), but the Phase 4 budget didn't include standing up a real
libp2p host with peer discovery + bootstrap.

**What Phase 4 ships.**
- `MpoolPush` returns `ErrMpoolNotWired` if no publisher is configured.
- `MpoolPushMessage` does the full gas-estimate + nonce + sign dance and
  returns the signed message even when the push fails. The wallet sign
  path is therefore exercised; only the network submission is stubbed.
- `wallet send` CLI command shows a DRY-RUN preview and asks for
  confirmation before attempting `MpoolPush`. If the push errors, the
  signed message CID is printed for the user to broadcast through any
  other channel (e.g., a Lotus full node).

**Recommendation.** Phase 6 (gateway infrastructure) is the natural
place to add the libp2p host — same code that the Lantern gateway needs
for Bitswap can host the gossipsub publisher. Until then, "live mpool
submission" is impossible from Lantern; the work-around is to use a
Lotus / Forest node for the final hop, which is fine for V1 user flows.

**Did Phase 4 submit a real message?** No. We confirmed the sign path
works (BLS round-trip in `wallet/wallet_test.go`; secp + delegated
roundtrips also pass) but live `MpoolPush` is gated on Phase 6. **No
production wallet was touched.**

---

## B17. Tier 1 actor-specific decoders deferred to Phase 5

Phase 4 implements **state-tree-level** reads (Actor struct: Code, Head,
Nonce, Balance) end-to-end. Actor-specific sub-state — MinerInfo,
MarketDeal, PowerClaim, VerifregAllocation — needs version-specific
go-state-types decoders, with a network-version → state-version mapping
table. That table belongs in `chain/actors/builtin/` (which the MODULES.md
already calls out as a Phase 2 deferral; B11 in PHASE2-BLOCKERS.md
restates this).

**What's implemented (Tier 1):**

| # | Method                       | Status                  |
|---|------------------------------|-------------------------|
| 6 | ChainHead                    | ✅ Implemented           |
| 18| ChainGetTipSet               | ✅ Implemented (current head only) |
| 11| StateNetworkVersion          | ✅ Implemented (hardcoded v27) |
| 25| StateLookupID                | ✅ Implemented           |
| 12| StateAccountKey              | ⚠️ ID pass-through; account-actor decode deferred |
| 44| StateGetActor                | ✅ Implemented           |
| 14| WalletBalance                | ✅ Implemented           |
| 50| ChainReadObj                 | ✅ Implemented (with CID verify) |
| 51| ChainHasObj                  | ✅ Implemented           |
| 47| ChainGetMessage              | ✅ Implemented           |
| 63| StateNetworkName             | ✅ Implemented           |
| 1 | AuthVerify                   | ✅ Implemented           |
| 2 | AuthNew                      | ✅ Implemented           |
| 3 | Version                      | ✅ Implemented           |
| 4 | Shutdown                     | ✅ Implemented           |
| 5 | Session                      | ✅ Implemented           |

**Deferred to Phase 5** (needs sub-state decoders):
- `StateMinerInfo`, `StateMinerPower`, `StateMinerSectors`,
  `StateMinerProvingDeadline`, `StateMinerDeadlines`,
  `StateMinerPartitions`, `StateMinerAvailableBalance`,
  `StateMinerAllocated`, `StateMinerFaults`, `StateMinerRecoveries`,
  `StateMinerSectorCount`, `StateSectorPreCommitInfo`,
  `StateSectorGetInfo`, `StateSectorPartition`, `StateMarketBalance`,
  `StateMarketStorageDeal`, `StateGetAllocation`,
  `StateGetAllocationIdForPendingDeal`,
  `StateGetAllocationForPendingDeal`, `StateVerifiedClientStatus`,
  `StateListMiners`.

**Deferred to Phase 5/6** (needs header-store walk + receipt AMT):
- `ChainGetTipSetByHeight`, `ChainGetTipSetAfterHeight`,
  `ChainGetBlock`, `ChainGetMessagesInTipset`,
  `StateGetRandomnessFromBeacon`, `StateGetRandomnessFromTickets`,
  `StateGetBeaconEntry`, `StateWaitMsg`, `StateSearchMsg`.

**Deferred to Phase 7** (block production / VM):
- `MinerCreateBlock`, `MinerGetBaseInfo`, `MpoolSelect`,
  `SyncSubmitBlock` (all SP-only).
- `StateCall` (read-only VM eval; needs pure-Go FVM; Phase 5 dependency).

All deferred methods return
`xerrors.New("not implemented in Lantern V1 — <reason>")` so Curio's
typed client detects the gap clearly.

**Recommendation.** Phase 5 should focus on `chain/actors/builtin/`
sub-state decoders. With those in place, ~30 of the 71 methods become
trivial wrappers (~30-50 LOC each). The remaining heavy hitters are
`StateCall` (Phase 5 VM work) and the SP block-production set
(Phase 7).

---

## B18. JWT auth uses gbrlsnchs/jwt/v3 with bare HS256, not Lotus' full scheme

**Why.** Lotus issues JWTs with a typed `payload` (`Allow []auth.Permission`)
signed by HS256 with a 32-byte secret. Lantern matches that shape exactly
using `github.com/gbrlsnchs/jwt/v3` — the same library Lotus uses (verified
by checking lotus@v1.36.0/node/modules/jwt.go).

**Token compatibility verified by hand-decoding:**
```
$ cat ~/.lantern/token | cut -d. -f2 | base64 -d
{"Allow":["read","write","sign","admin"]}
```

Same shape as `lotus auth create-token --perm admin` output.

**One delta:** Lantern's permission check is enforced at the HTTP
middleware (parsing the JSON-RPC body to extract the method, then
mapping method-name to required perm via a static table). Lotus uses
auth.PermissionedProxy on a typed FullNodeStruct with `perm:` tags.

Both achieve the same end (a `read` token can't invoke `WalletNew`),
but Lantern's pattern is simpler at the cost of being slightly less
ergonomic if you add a method without remembering to update
`methodPermission()` in `rpc/server/server.go`.

**Recommendation.** Phase 5 can adopt the Lotus FullNodeStruct pattern
(`perm:"sign"` struct tags + auto-generated method shims) if the static
table feels brittle. For Phase 4 the static table is fine — 71 methods,
all categorised.

---

## Open questions for Nicklas

1. **Gateway TLS cert.** Should we re-issue Let's Encrypt on
   gateway.lantern.reiers.io now, or wait until Phase 6 (gateway infra
   work) and just rely on Glif fallback in the meantime?

2. **Phase 5 scoping.** Two paths forward:
   - (a) **Breadth first**: implement the ~30 actor-state decoders
     (Phase 4 stubs → real impl). Buys Curio compatibility for ~70% of
     calls but no VM.
   - (b) **Depth first**: implement `StateCall` (read-only VM) on a
     pure-Go FVM subset. Unlocks `eth_call`, gas estimation, and the
     hardest method on the Curio surface. Big lift; ~2 weeks alone.
   Recommend (a) first — gets to Curio-runnable faster.

3. **Mainnet message submission.** The Phase 4 spec mentioned
   "demonstrably submits a real signed message to mainnet". We did not
   submit live (per B16: mpool publisher is not wired, and TOOLS.md is
   explicit about not touching any production wallet). The wallet sign
   path is fully tested in unit tests; live submission gets the green
   light when Phase 6 lands the libp2p host.
