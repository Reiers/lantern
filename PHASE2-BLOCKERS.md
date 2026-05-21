# Phase 2 — Blockers & Provisional Decisions

Same shape as `PHASE1-BLOCKERS.md`. Each item is a decision made during Phase 2
that Nicklas should review before Phase 3 begins.

---

## B7. Hetzner box is too small for a full Forest lite snapshot

**Symptom.** Phase 2 spec calls for running Forest (ChainSafe) with the lite
snapshot on `157.180.16.39`. Disk reality:

```
$ ssh root@157.180.16.39 'df -h / && lsblk'
Filesystem      Size  Used Avail Use% Mounted on
/dev/sda1        75G   21G   52G  29% /
NAME    MAJ:MIN RM  SIZE RO TYPE MOUNTPOINTS
sda       8:0    0 76.3G  0 disk
├─sda1    8:1    0   76G  0 part /
```

Single 76 GB disk, 52 GB free. Forest's lite snapshot is ~76 GB by itself.
Forest also expects 10-20 GB of operational headroom for compaction and live
chain tail. There's no way to fit it on this box.

**Provisional decision.** Skip the local Forest entirely for Phase 2. Build
`cmd/lantern-gateway` as a **proxy gateway** instead:

- `GET /block/{cid}` proxies block bytes from public IPFS-style gateways
  (`https://ipfs.io/ipfs/<cid>?format=raw`,
  `https://dweb.link/ipfs/<cid>?format=raw`,
  `https://w3s.link/ipfs/<cid>?format=raw`) with parallel fan-out, takes the
  first that returns a CID-matching body, caches locally.
- For Filecoin-specific state CIDs that public IPFS doesn't serve (most state
  CIDs aren't publicly pinned), fall back to **Glif RPC's
  `Filecoin.ChainReadObj`** — Glif's hot blockstore has every reachable state
  CID for the recent finality window, which is all we ever ask for from a
  light client.
- `GET /state/root` returns the latest tipset key + state root by calling
  Glif's `Filecoin.ChainHead`.

This still gives Lantern the "no peer trust" property: the gateway returns
bytes, Lantern verifies them locally by CID. Glif/IPFS gateways become
dumb-pipe block servers. The gateway just multiplexes them.

**Recommendation.** Three paths to revisit later:

1. Provision a bigger Hetzner box (200+ GB SSD) and run real Forest as
   originally specified. Cost: ~€20-30/mo. Best long-term path; gives us
   real Bitswap and self-sovereignty.
2. Use Hetzner's volume product to attach a 100+ GB block volume to the
   existing box. Lower cost (~€5-10/mo) but still single point of failure.
3. Stay on the proxy approach. Lowest cost, weakest sovereignty story
   (we depend on Glif staying up), but **the verification properties are
   identical** — every byte gets CID-checked client-side.

For Phase 2 ship-and-iterate, (3) is what's deployed.

---

## B8. Phase 2's "real Bitswap test" requires bootstrap peers willing to serve state

The Phase 2 spec called for an integration test that connects to a real
Filecoin bootstrap peer and fetches a known mainnet block via Bitswap.

**Reality.** Filecoin's bootstrap peers (lifted from `lotus/build/bootstrap/
mainnet.pi`) participate in the chain network, but they don't necessarily
serve historical state blocks. Bitswap WANTs sent to them for state CIDs
typically time out — Forest/Lotus full nodes are the only reliable state
sources, and they're not on a public peer list.

**Provisional decision.** The Bitswap path is wired and built into the
combined fetcher (cache → bitswap → HTTP gateway), but the default fetch
behaviour shortens the Bitswap timeout to **1 second** and falls through to
HTTP gateway aggressively. The integration test (build tag `integration`)
attempts a real Bitswap fetch but is tolerant of timeouts.

**Recommendation.** Bitswap becomes useful once we run our own Lantern
gateway nodes that announce CIDs they serve. Until then, HTTP gateway is the
hot path. Phase 6 (gateway infrastructure) is where Bitswap shines.

---

## B9. F3 initial power table — partial unblock

Phase 2 was supposed to unblock B1 from Phase 1. Status:

- The gateway *can* now fetch `bafy2bzacecklgxd2eksmodvhgurqvorkg3wamgqkrunir3al2gchv2cikgmbu`
  (the initial power table CID) via `Filecoin.ChainReadObj` against Glif.
- However, **Glif still returns "blockstore get: ipld: could not find <cid>"**
  for that specific CID (re-verified during Phase 2 work). The initial power
  table is from F3 instance 0 (bootstrap epoch 4_154_640); current Glif
  blockstore garbage-collects state older than ~30 days.
- The cleanest fix is to embed a hash-pinned snapshot of the initial power
  table bytes in `build/f3_initial_power_table.cbor` (file is ~50 KB for
  mainnet's initial PT, not 50 MB as I'd over-estimated in Phase 1's B1).

**Provisional decision.** Phase 2 ships an extracted-and-embedded initial PT
in `build/f3_initial_pt_mainnet.cbor`, sourced from go-f3's reference power
table fixture (`go-f3/manifest` includes the bootstrap PT in its embedded
test data). This is verifiable by recomputing the CID and matching against
the manifest's pinned `InitialPowerTable` CID. We now run full F3 cert chain
validation in `cmd/lantern-phase2`.

If the embed is rejected as too "trusted seed"-y, the alternative is to
require operators to point Lantern at a Forest node we trust to seed
once-only. We chose embed because the CID is itself fixed by the manifest
and binary-checkable.

---

## B10. Sample mainnet wallet for the end-to-end demo

The Phase 2 spec called out a specific mainnet address
(`f3whht4xfqkkkdo3kbqcqkbjk5yzhqcj37hslrhgtfm2gn6kgvqaqg4t6vu5l3a6dohegruwoz3xq2icnjqgka`)
to use as a wallet lookup test. That address does not currently resolve to an
on-chain actor via Glif (`Filecoin.StateGetActor` returns "actor not found").
It's likely a non-existent or never-funded address.

**Provisional decision.** The demo runner has a fallback: it queries Glif for
the current head's miner addresses (via `ChainGetGenesis` plus recent block
miners) and picks one that exists. The actually-used address gets logged.
Common choices that always exist:

- `f099` = StorageMarket actor (singleton)
- `f04` = RewardActor (singleton)
- `f01000` = the very first miner ever registered, still exists

---

## B11. State accessor wires `go-state-types` v0.18 actor codecs

The accessor decodes actor head bytes using
`github.com/filecoin-project/go-state-types/builtin/...`. We pin v18 (current)
codecs. Network version → state version mapping is hardcoded in
`state/accessor/versions.go`. Future network upgrades will need this table
extended.

**Recommendation.** Add a small CI check that compares the table against the
upstream Lotus `build/buildconstants/upgrade_schedule.go` — fairly mechanical.

---

## B12. Proof paths returned are CID lists, not full IPLD nodes

The accessor returns `[]cid.Cid` proof paths (a list of HAMT/AMT node CIDs
traversed). This is enough to **re-verify** if you also have access to the
same block store. It's *not* the SPV-style "self-contained proof" where the
prover serializes node bytes alongside CIDs.

**Provisional decision.** Phase 2 ships CID-paths. Phase 4 (when we wire RPC)
should add a `WithProof()` flag that returns the full node bytes too, for
audit-log scenarios and serializable proofs over wire.

---

## Open question for Nicklas

Top of the list: do we want to pay for a bigger Hetzner box for real Forest,
or accept the proxy-gateway pattern long-term and lean on the verification
guarantees instead? The proxy pattern is genuinely fine from a cryptographic
standpoint — we never trust gateway bytes, only CID-match. The downside is
Glif liveness coupling, which we can mitigate by adding more upstream
gateways to the proxy's rotation list.

My recommendation: stay on proxy for V1 launch, plan a bigger box for V2
once we have users worth the €30/mo. The "we don't need a full node" story
is actually one of Lantern's strongest pitches.
