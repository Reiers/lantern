# Phase 1 — Blockers & Provisional Decisions

These are decisions made by the implementing subagent that Nicklas should
review before Phase 2 begins. Each one carries a recommendation and a
fallback path. Nothing here blocks Phase 1's "definition of done" (build
succeeds, tests pass, integration runner matches Glif), but each item is a
real choice with downstream impact.

---

## B1. F3 initial power table cannot be seeded from a public RPC

**Symptom.** The mainnet F3 manifest references
`InitialPowerTable = bafy2bzacecklgxd2eksmodvhgurqvorkg3wamgqkrunir3al2gchv2cikgmbu`.
To verify the cert chain forward from instance 0, we need the actual
`gpbft.PowerEntries` list — not just the CID. We tried:

- `Filecoin.ChainReadObj({/: <cid>})` → Glif returns "blockstore get: ipld:
  could not find <cid>". The CID isn't in their hot blockstore.
- `Filecoin.F3GetECPowerTable(...)` / `F3GetF3PowerTable(...)` → "method
  not found" on Glif.
- Public IPFS gateways (`ipfs.io`, `dweb.link`, `cloudflare-ipfs.com`,
  `w3s.link`, `gateway.pinata.cloud`) → all fail to find providers within
  the 30s window.

**Provisional decision.** `chain/trustedroot.Build` accepts an
`InitialPowerTableLoader` callback. `cmd/lantern-phase1` does **not** run a
full F3 cert chain validation; instead it just decodes the latest cert and
prints metadata, while the TrustedRoot is built from header-chain
validation only. The cross-check vs Glif (epoch, tipset CID, state root,
parent message receipts, parent weight) still passes exactly, which is the
documented Phase 1 success criterion.

**Recommendation.** Phase 2 should:

1. Stand up at least one "Lantern gateway" node (full Forest or Lotus on
   hetzner) that exposes the initial power table over either Bitswap or
   a plain HTTPS `/state/<cid>` endpoint. This is the same "gateway"
   pattern called out in `SCOPE.md` open question 2.
2. Optionally embed a hash-pinned snapshot of the initial power table
   straight into the binary (~50 MB at most), with a release-key
   signature. This makes Lantern fully self-contained on first boot at
   the cost of a binary-size bump on every F3 power-table rotation.
3. Once we can seed the initial table, wire `trustedroot.Build` end-to-
   end through the cert chain in `cmd/lantern-phase1` and add the
   matching test.

Both paths converge on the same observation: F3 cert-chain verification
requires *something* that knows the initial power table bytes. The
verification *logic* (`chain/f3.VerifyCertChain`) is built and exercised by
unit tests; only the seed is missing.

---

## B2. Beacon-entry verification uses only quicknet (post-FIP-0063)

`cmd/lantern-phase1` runs each block's beacon entries through the quicknet
DRAND config. For the recent mainnet epochs we tested (≥ epoch 6,035,000),
this matches reality — quicknet activated at epoch 3,692,920 — and we get
exact verification on 5/5 entries per tipset.

If the runner is invoked against an older epoch (pre-quicknet activation),
we would need to switch to chained mainnet and supply the previous round
signature. The verifier code (`chain/beacon`) handles both modes; the
runner just doesn't have the historical chained-mainnet wiring yet because
Phase 1 only needs to validate "current" head.

**Provisional decision.** Document, ship as-is. Phase 2 wires both drand
chains into a `chain/beacon.Schedule` mirroring Lotus.

---

## B3. We do not verify block signatures or BLS message aggregates

Per the architecture decision in `MODULES.md`, Phase 1 deliberately defers:

- Per-block BLS/secp signature against the worker pubkey (requires state).
- BLSAggregate over messages (requires the in-block messages, which are
  not in the header stream).
- Election proof signature (requires worker pubkey).

The Lantern header validator (`chain/header.ValidateHeader`) explicitly
documents these as deferred. Phase 4 closes them when the state accessor
exists.

**Implication.** A header-only Lantern light client trusts *header CID
hashing + parent linkage + F3 finality + beacon randomness* but not the
in-block signatures. The F3 cert chain still gives strong finality
guarantees once B1 is resolved; without F3, a header-only client is
vulnerable to a 2/3+ collusion of miners producing a fork with valid
beacon entries but invalid block sigs. The cure is F3 verification, which
is built but unseeded.

---

## B4. Lotus uses `dgraph-io/badger/v2`; Lantern uses `badger/v4`

The MODULES.md plan said `badger/v4`. We followed that. Worth flagging
that Lotus' own modules pin v2; if we ever want to share a database file
format (e.g. read an existing Lotus blockstore on disk), we'll need a
migration. Phase 1 has no such requirement.

**Recommendation.** Keep v4. It has better performance and is the path
forward; Lotus is a transient consumer.

---

## B5. TrustedRoot persistence is JSON, not CBOR

For Phase 1 simplicity, `chain/trustedroot/codec.go` encodes the
TrustedRoot as JSON with the F3 cert embedded as its CBOR bytes. This is
fine for now (the struct is ~500 bytes), but a long-running node will want
CBOR-gen everywhere for read/write speed and forward-compat.

**Recommendation.** Add CBOR-gen tags in Phase 2 once we know the
TrustedRoot's schema is stable. The on-disk JSON is human-readable for
debugging (`badger get tr:current | jq`) which is a nice transient property.

---

## B6. We have not yet implemented re-org detection

`TRUSTED-ROOT.md` §4 describes a re-org event channel (`HeadChange`).
Phase 1 builds a one-shot root from a static head snapshot and exits, so
re-org logic is not exercised. The structure is straightforward to add in
Phase 2 once the header store accumulates over time.

---

## Open question for Nicklas

The cleanest unblocker for B1 is option (1): a Lantern gateway. That's
called out in `SCOPE.md` open question 2 anyway. Suggest we lift it
forward to early Phase 2: stand up `gateway.lantern.reiers.io` on the
Hetzner box that already runs a Lotus or Forest node, expose Bitswap +
`/state/<cid>` HTTPS, and switch the integration runner to use it.

Once that's done the F3 cert-chain pass becomes a one-line addition in
`cmd/lantern-phase1` and we get full end-to-end finality verification.
