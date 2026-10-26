# Lantern — Trust Model

This document specifies exactly what a Lantern node trusts unconditionally,
what it verifies locally, what it does not trust, and what changes when the
optional VM bridge is wired (Phase 8 Part B).

The goal: an operator should be able to read this page and know, on a
per-operation basis, who is on the hook for the correctness of their result.

---

## 1. What Lantern trusts unconditionally

The unconditional trust roots are small, hardcoded, and reproducible.

| Trust root                         | Source                                       | Audit notes |
|------------------------------------|----------------------------------------------|-------------|
| Genesis tipset CID (mainnet)       | `rpc/handlers/extra.go` (`mainnetGenesisCID`) | Cross-verifiable against `api.node.glif.io` `Filecoin.ChainGetGenesis`. |
| F3 power-table anchor              | `chain/f3/anchor/anchor_mainnet.json` (embedded) | Captured from a Forest full node at a documented instance. Re-pinnable on a quarterly cadence. |
| DRAND network config (chain hashes, public keys, periods) | `build/drand.go` | Pulled verbatim from `lotus@v1.36 build/drand.go`. Public records cross-checked against `drand.cloudflare.com`. |
| Built-in actor manifest CIDs       | `state/actors/bundles.go`                    | Generated verbatim from `lotus@v1.38.3 build/builtin_actors_gen.go` by `go run ./state/actors/internal/genbundles -lotus v1.38.3`. CIDs are reproducible by hashing the `builtin-actors` releases. |
| Network upgrade schedule (epoch -> network version) | `build/upgrades.go` | Heights from `lotus@v1.38.3 build/buildconstants/params_{mainnet,calibnet}.go`, upgrade -> network version from `chain/consensus/filcns/upgrades.go`. |
| Cryptographic primitives           | `crypto/sigs/bls`, `crypto/sigs/secp`, `crypto/sigs/delegated` | Pure-Go (gnark, kyber-bls12381, dcrec, golang.org/x/crypto). No CGo. |
| Filecoin network constants         | `build/buildconstants.go`                    | Lifted verbatim from Lotus's `params_shared_vals.go`. |

These trust roots are checked into the binary. Compromising them requires
either compromising the build pipeline or compromising Lotus / Forest at the
source.

---

## 2. What Lantern verifies locally

Everything past the trust roots is verified at runtime, on the host running
the binary. There is no remote oracle that can lie to a Lantern node about
any of the following without being detected.

### 2.1 Header chain

Lantern is an **F3-anchored light client**, not a full re-executing
validator. It deliberately does NOT do full Lotus-style block validation,
because cryptographic election-proof / winning-PoSt verification requires
`filecoin-ffi` (Rust), which Lantern's zero-CGo / one-binary design
excludes by construction. Be precise about what that means per-field:

**What every ingested header IS verified for:**

* **CID integrity** — the header's CID is re-derived and checked against the
  bytes (`header.VerifyBlockHeaderCID`). A peer cannot serve a header under
  a CID it doesn't hash to.
* **BLS / signature presence and shape** — structural validation
  (`chain/header/header.go::ValidateHeader`, `ValidateTipsetShape`),
  modelled on `lotus@v1.36 chain/sync.go`.
* **Parent linkage** — a header is only adopted when its parents are present
  in the local store (inline-backfilled if needed).
* **Heaviest-ParentWeight fork choice** (#79) — the running gossip head is
  advanced only when a candidate's `ParentWeight` strictly exceeds the
  current head's. A competing valid-but-lighter fork is rejected. This is
  Filecoin's fork-choice rule applied with pure header arithmetic.

**What is NOT verified (by design, no-ffi):**

* **Election-proof VRF validity** — Lantern checks that `ElectionProof` is
  *present* for non-genesis heights, but does NOT cryptographically verify
  the VRF (that needs the proofs/ffi path).
* **Winning-PoSt** — not verified, same reason.
* **Full message re-execution** — not performed.

**What carries the trust instead:** the boot anchor (multi-source quorum on
the F3-finalized tipset, §below) plus the F3 finality cert chain (§2.2,
BLS-aggregate-verified). Past the finalized depth, the head is followed
optimistically with the fork-choice + linkage checks above — the same
probabilistic tip every node follows until F3 finalizes it. The residual
eclipse/fork-selection exposure on the un-finalized tip is documented in
§5 and tracked in #79 / #80; finality (F3) is what fully closes it.

> Note: an earlier version of this section claimed election-proof and
> weight were "re-verified" on every header back to genesis. That
> overstated the code (weight fork-choice landed in #79; election-proof
> VRF is presence-checked, not verified). Corrected for accuracy.

### 2.2 F3 finality

* F3 certificates are walked forward from the embedded anchor's instance.
* Each certificate's BLS aggregate is verified against the current
  power-table snapshot. Power-table diffs in each certificate are applied
  to advance the table.
* `chain/f3/subscriber` persists the (instance, powerTable, finalizedTipSet)
  triple to Badger so restarts don't re-verify from the anchor.

### 2.3 DRAND beacons

* DRAND beacon entries referenced by block headers are verified locally
  using the public DRAND key + chain hash from `build/drand.go`.
* No DRAND-network-provided value is trusted without BLS-checking it
  against the embedded public key.

### 2.4 State (IPLD HAMT/AMT) reads

* `state/hamt` and `state/amt` recompute the CID of every block they fetch
  and refuse to use a block whose computed CID doesn't match what the
  parent link said it should be. Implementation: `hamt.VerifyBlockCID`.
* `state/accessor.Accessor` chains these into "(stateRoot, addr) →
  actorState," and the actor's state is decoded with the canonical
  go-state-types/builtin/v{N} cbor-gen decoder — no Lantern-internal
  CBOR plumbing for these state blobs.

### 2.5 Wallet & signing

* Local key material lives in `~/.lantern/keystore`, AES-GCM-encrypted with
  a passphrase. Lantern never exfiltrates private keys.
* All signing is local. BLS uses gnark; secp uses dcrec; delegated (f4)
  uses keccak + secp.

### 2.6 Mpool publish

* `MpoolPush` validates the signature locally (`crypto/sigs.Verify` for
  BLS / secp / delegated) before publishing on the gossipsub
  `/fil/msgs/<network>` topic.

---

### 2.7 Observed-data EC finality (FRC-0089) — a computed bound, not a proof

`chain/ecfinality` (#96) computes an upper bound on the probability that
a tipset at depth D could be reorged, from the OBSERVED block counts per
epoch in the local header store. Under healthy conditions the 2^-30
bound is met around depth ~30 (~15 min) instead of the static worst-case
900. Lantern uses it as an honesty instrument (dashboard, stats) and as
an input for retention decisions - NOT as a substitute for F3: the
calculation assumes the observed chain is the honest heavy chain, so its
guarantees are only as good as the head path that observed it (see 3.3).
Where F3 finality is available it always takes precedence; the useful
semantics for consumers is `finalized = max(ec-finalized, f3-finalized)`.
A node with a shallow observed window (< 30 epochs of history) reports
"not computable" rather than an over-confident number.

### 2.8 Running-head quorum (v1.10.0, Milestone 1)

Past the boot anchor, Lantern follows the un-finalized tip from gossip.
The running-head quorum (`chain/headcheck`) makes sure that tip is the
one the rest of the network is on, without trusting any single source.

**Every 30 s** (`DefaultInterval`) the monitor asks independent voters
for their head and compares it with ours at a lookback of 3 epochs:

* **Tipset key, not height** (#152). A voter agrees only if the tipset it
  reports at the lookback epoch is the one on our canonical chain. A
  same-height eclipse fork with a different key is a disagreement, not a
  match.
* **Independence per operator** (#153). RPC voters are counted per
  upstream operator, not per URL: the Lantern gateway votes as its
  upstream (it advertises `upstream` in `/state/root`), so gateway + Glif
  is one voter. An operator-distinct RPC (chain.love on mainnet, Filfox on
  calibration, or anything passed via `--head-check-rpc`) adds a second.
* **Peer groups** (#154). Heads observed from libp2p peers (Hello
  handshakes and gossip block forwarders) vote too, one vote per network
  group: IPv4 /16, IPv6 /32. Non-IP transports are ignored (Sybil-cheap);
  observations older than 10 minutes are dropped. A peer head agrees if
  it is on our canonical chain or is a sibling of it (same parents);
  otherwise it is forked. Peers behind our head are not counted unless
  they are forked. This is what gives a bridge-off node
  (`--no-fallback-rpc`) a quorum with zero trusted RPC.
* **Fork choice across sources.** When voters disagree, the candidate
  backed by the most independent voters wins, then the heaviest
  `ParentWeight`. At least 2 independent agreeing voters are required
  (`DefaultMinAgree`).

**The adoption gate.** A `diverge` result closes the gate: the gossip
head is not advanced until the quorum re-corroborates it. Backfill only
persists ancestors (#155); moving the head is one guarded decision on the
tip (fork choice, weight-monotonic check, gate). A node that is simply
behind reports `behind` and keeps catching up rather than deadlocking
(#162). Every adopted child must have a strictly larger `ParentWeight`
than its parent (#156).

**Where it runs.** The standalone `lantern daemon` (#160) with RPC voters
and peer groups. The embedded `pkg/daemon` runs the RPC voters; it has no
Hello service yet, so it does not collect peer-group votes (follow-up).
Disable with `--no-head-check` (not recommended).

**Observability.** `/metrics` exports `lantern_headcheck_status`,
`lantern_headcheck_voters{outcome}` (agree, disagree, lagging, forked,
peer_groups), `lantern_headcheck_rounds_total`,
`lantern_headcheck_diverged_total` and
`lantern_head_rejected_total{reason}`.

**Boot anchor while F3 is stale** (#167). `init`, `repair`, the dashboard
renew action and the bridge-off auto-reset anchor on the F3 bootstrap
quorum when its finality is within 24 h of the wall-clock head. If F3
finality is older, they anchor on EC multi-source agreement (independent
operators, same per-operator counting) at the live head, or refuse.
`bootstrap-anchor.json` records which (`source`).

**Honest boundary.** Network groups raise the cost of faking a peer
majority; they do not make it impossible. `ParentWeight` is taken from
headers and only checked for monotonicity, not recomputed (that needs
power state). The quorum is an eclipse and single-source defence on the
un-finalized tip; F3 finality (section 2.2) is what fully closes it.

### 2.9 Bootstrap quorum: distinct operators, zero conflicts (F10/F11)

`lantern init`, `repair`, `doctor`, the dashboard renew action and the
bridge-off auto-reset all run the same probe (`chain/bootstrap`): ask every
source for its latest F3 finality certificate in parallel, then accept a
finality only when

1. sources run by at least **3 distinct operators** (`--bootstrap-quorum`,
   default 3 on mainnet and calibration) return the same (instance,
   tipset key, power table), and
2. **no responder conflicts**: if any source, counted or not, names a
   different tipset for an instance another source answered, the probe
   fails with `ErrConflict`. It is not retried and no majority overrides it.
   F3 finality is unique per instance (GPBFT safety), so a same-instance
   disagreement means a source is lying, broken, or on another network.
   An answer for a different instance is lag, not conflict. After the
   threshold is met the probe keeps listening for 3 s so a slightly slower
   conflicting answer is still seen.

**Why operators, not endpoints.** The pre-F10 rule was "5 sources agree",
counted per endpoint. On 2026-10-26 the reachable mainnet sources were the
Glif RPC, the chain.love RPC, the chain.love libp2p bootstrap peer and one
ChainSafe peer. Glif is served from chain.love infrastructure (same IP
46.229.170.7 as api.chain.love, `x-chainlove-lb-*` load-balancer headers, the
same non-release Lotus build, a bare HEAD redirects to filecoin.chain.love),
so three of those four "independent" votes were one operator, and
`--bootstrap-quorum 4` (the workaround users reached for) meant **two**
operators. Counting operators is strictly stronger even with a lower
number: today's default needs 3 different organisations, where the old
default of 5 could be met by 2.

**How a source is attributed** (`bootstrap.OperatorOf`,
`OperatorOfMultiaddr`):

* RPC endpoints: the registrable domain of the URL host, folded through a
  small evidence-backed alias table (`glif.io -> chain.love`,
  `chainsafe.dev -> chainsafe-fil.io`).
* libp2p bootstrap peers: the DNS domain in the bootstrap multiaddr. Peers
  that come from a `/dnsaddr` root are attributed to the domain that
  publishes the TXT records. A peer listed with several addresses is one
  source. A bare IP counts as its own operator; a bare peer ID as itself.
* `--peer` URLs: the URL's domain, so a node you run yourself is its own
  operator and adds a vote.
* The Lantern gateway is not counted unless `--count-gateway`; it votes as its
  upstream (Glif, therefore chain.love).

This is a heuristic. It cannot prove two domains are different
organisations, only collapse the duplicates we know about. Every alias in
the table carries the evidence and the date it was observed.

**Default source set (verified 2026-10-26).** Bootstrap peers are Lotus
v1.38.3's `build/bootstrap/*.pi` verbatim; `/dnsaddr` roots are resolved at
runtime with an embedded snapshot as the offline fallback.

| Network | Operator | Sources |
|---|---|---|
| mainnet | chain.love | api.chain.love RPC, api.node.glif.io RPC, bootstrap.filecoin.chain.love peer |
| mainnet | chainsafe-fil.io | bootstrap-mainnet-{0,1,2} peers (only -1 serves F3 cert-exchange today) |
| mainnet | filfox.info | filfox.info RPC |
| mainnet | ankr.com | rpc.ankr.com/filecoin RPC |
| calibration | chain.love | api.calibration.node.glif.io RPC, bootstrap.calibration.filecoin.chain.love peer |
| calibration | chainsafe-fil.io | bootstrap-calibnet-{0,1,2} peers (only -1 serves F3 cert-exchange today) |
| calibration | filfox.info | calibration.filfox.info RPC |
| calibration | ankr.com | rpc.ankr.com/filecoin_testnet RPC |

Four operators per network, three required: one can be down. Dropped in
the refresh: the filincubator Venus peer (no DNS record) and the devtty.eu
peer (TCP and QUIC time out). Some public endpoints reject Go's default
HTTP User-Agent (filfox answered 403), so Lantern identifies itself as
`lantern/<version>`.

**Stale F3.** Mainnet F3 has been stuck at instance 466453 (epoch 5824156)
since spring 2026, about 231 days behind the live head on 2026-10-26. The
probe still reaches agreement on that certificate, and #167 then refuses to
anchor on it: init falls back to EC head agreement at the live head. In
that fallback a stale certificate is now ignored entirely (F11), because
"consistent with F3" is vacuous for a months-old finality: every live head
is a descendant. Without that, the old "F3-consistent heaviest" rule
accepted a single operator's head. The EC fallback requires 2 distinct
operators to agree on (state root, tipset key), compared at a common epoch
a few epochs below the lowest sampled head when the heads straddle an epoch
boundary. That is a lower bar than the 3-operator F3 quorum; it is the bar
the #54 boot anchor already used, and it is documented in
`bootstrap-anchor.json` as `"source": "ec-multi-source"`. Calibration F3 is live, so
calibration anchors on the F3 quorum directly.

**No anchor, no daemon (F11).** `lantern daemon` used to fall back silently
to a gateway+RPC head when init had not written an anchor. That path is
weaker than init's quorum, so the daemon now refuses to start without
`bootstrap-anchor.json` and tells the operator to run `lantern init` (or
`lantern doctor` to see what failed). `--allow-rpc-anchor` restores the old path
with a loud warning. Devnet and `--insecure-anchor` keep their existing explicit
single-endpoint behaviour.

**Tradeoffs, stated plainly.**

* Liveness vs safety: a single misbehaving default source can block
  bootstrap (any conflict fails). It cannot steer it. Failing closed is the
  intended behaviour; `--peer` lets an operator add a source they trust.
* 3 operators is the floor for a fresh install. `--bootstrap-quorum 2` or `1`
  is possible and the failure hint offers it, but each step down is one
  fewer organisation an attacker has to compromise.
* Independence of Ankr's Filecoin backend is not provable from outside. It
  runs a different Lotus build from Glif/chain.love and Filfox, and it is
  never needed alone: the other three operators already meet the default.

---

## 3. What Lantern does NOT trust

These are deliberately untrusted. A Lantern node treats responses from any
of the following as input that must clear local verification before use.

### 3.1 RPC providers (Glif, infura, etc.)

The daemon boots from the anchor written by `lantern init` (section 2.9) and
uses `gateway.lantern.reiers.io` and Glif for cold blocks and fallback reads. Both responses are immediately
CID-verified locally; if a provider lies about the state root, the
verifier rejects the response. **Lantern never returns unverified data
to its caller, no matter what an upstream RPC says.**

### 3.2 State-serving peers (Bitswap / HTTP gateways)

Same story: any IPLD block fetched from any peer is hash-verified before
use. A malicious peer can DoS (refuse to serve) but cannot lie.

### 3.3 Gossipsub peers

Inbound block + message gossip is decoded then validated by the consumer
(`chain/header.ValidateBlock` for blocks; `crypto/sigs.Verify` for
messages). Malformed traffic is dropped.

Head adoption additionally applies (v1.9.0): heaviest-ParentWeight fork
choice, the running-head quorum and adoption gate (section 2.8), and
optional head-source corroboration (#80): a head advance requires
forwarding by N distinct peers or one trusted floor peer. Peers earn
score through first-delivery history on the blocks/msgs topics (#97) and
lose it for invalid messages and IP-colocation; the trusted floor
(bootstrap/beacon/direct peers) is connmgr-protected and cannot be
evicted by a dial flood. Honest boundary: peer IDs are Sybil-cheap, so
corroboration raises eclipse cost only in combination with scoring and
the floor. It is hardening, not finality; finality comes from F3
(section 2.2) and the observed-data EC finality bound (section 2.7).

### 3.4 Forest / Lotus nodes that are NOT the wired bridge

A Forest or Lotus node that happens to be a libp2p peer of ours is in the
"state-serving peer" category above: trusted to serve content, not to
compute it. We CID-verify everything it gives us.

---

## 4. The VM bridge (Phase 8 Part B) — a documented soft trust point

When an operator wires a bridge (`ChainAPI.WithBridge(...)` or
`vm/bridge.NewForestBridge(...)`), Lantern delegates two narrow operations
to that bridge's upstream node:

1. **`Filecoin.StateCall` for non-Send messages.** The native vm shell
   cannot execute builtin actor methods (it gas-accounts them but
   returns no payload). With a bridge configured, StateCall for any
   method other than 0 routes through the bridge's
   `ComputeStateRoot(base, epoch, [msg])`. The receipt the bridge
   returns IS the receipt Lantern returns to the caller. Lantern does
   NOT re-verify the receipt's exit code, return bytes, or gas usage
   against an independent computation — we have no independent
   computation.

2. **`Filecoin.MinerCreateBlock` with `AllowBlockSubmit=true`.** The
   block's `ParentStateRoot` is set to whatever root the bridge
   computes from applying the selected messages against the parent
   tipset's stateRoot. That root is signed into the block header by the
   miner's worker key.

### 4.1 Bridge trust scope — what the bridge CAN and CANNOT do

A malicious or compromised bridge upstream **can**:

- Lie about a single `StateCall` result. Worst case: an operator's
  toolchain (Curio, lotus CLI's `state call`) sees an incorrect
  exit code or return value. This can mislead a deal-acceptance
  decision but does NOT cause the operator to publish bad chain data.
- Lie about a `MinerCreateBlock` ParentStateRoot. Worst case: the
  block the operator signs and publishes is rejected by the network
  (because the stateRoot won't match what honest peers compute). The
  operator wastes a winning-PoSt opportunity but does NOT damage other
  operators or fork the chain.

A malicious or compromised bridge upstream **cannot**:

- Cause Lantern to accept an incorrect header chain (header validation
  is local, BLS-checked).
- Cause Lantern to accept an incorrect F3 finality (F3 verification is
  local, BLS-aggregate-checked).
- Cause Lantern to return incorrect state read results (HAMT/AMT
  walking is local, CID-verified).
- Cause Lantern to accept fake DRAND beacons (verified locally against
  the embedded public key).
- Cause Lantern to sign a message the operator did not authorise (key
  material is local; signing API is local).
- Exfiltrate the operator's keys.

In short: the bridge is bounded to message-execution semantics, on the
operator's own active path. It is NOT a chain-level oracle.

### 4.2 The bridge as an auditor (`--vm-crosscheck`, #98)

The same bridge connection can optionally AUDIT the read path: once a
minute Lantern compares its canonical tipset at head-3 against the
bridge node's and alarms loudly on divergence. This adds no new trust
(the bridge is already trusted for block production) and changes no
behavior (observe-only; reads are never answered by the bridge). It
catches the class of attack full re-execution would catch - a fabricated
chain whose headers verify but whose state was never honestly executed -
as long as the operator's bridge node is honest and unpartitioned.

### 4.3 Recommended operator configurations

| Profile                | Bridge | Trust posture |
|------------------------|--------|---------------|
| Strict light-client    | None   | Zero non-cryptographic trust. StateCall for non-Send returns `SysErrInvalidReceiver`. MinerCreateBlock is gated. |
| SP backend             | Operator's own Forest/Lotus on the same LAN | The bridge upstream is the operator's own infrastructure. Identical trust posture to running a full node themselves. Strongly recommended for SPs. |
| Light + deal flow      | Operator's own Forest as a sidecar | Bridge used only for the few `StateCall` paths Curio needs (e.g. storage-market PSD verification). All chain reads still verified locally. |
| **Don't do this**      | A third-party public RPC | Equivalent to "trust your RPC provider." Defeats the point of running Lantern in the first place. We don't ship a default public bridge for this reason. |

### 4.4 Bridge provenance + auditability

`vm/bridge.Bridge.Provenance()` returns an opaque tag (`forest@<host>`,
`lotus@<host>`, etc.) that Lantern uses in trace logs. When StateCall
falls back to the native vm shell after a bridge error, the bridge's
provenance is printed so operators can see which upstream declined.

There is no on-chain attestation of bridge correctness. We treat it
like any other internal sidecar: the operator is responsible for
running a node they trust.

---

## 5. Threat model summary

A short answer to "what can attackers controlling X do to a Lantern user?"

| Attacker controls...                                         | Worst case for the Lantern user |
|--------------------------------------------------------------|---------------------------------|
| The default HTTPS gateway (gateway.lantern.reiers.io)         | DoS only. Every byte they serve is locally CID-verified. |
| The Glif fallback RPC                                        | DoS only. Same verification. |
| One or more libp2p Bitswap peers                              | DoS only. Same verification. |
| All currently-connected gossipsub peers (eclipse)             | At most a stale/lighter fork on the *un-finalized* tip, never a chain-rule-violating or finalized-state lie. Defenses stack: heaviest-ParentWeight fork choice (#79) forces an attacker to out-*weight* the real chain (control real winning power) rather than just spam sybil peers; trusted bootstrap/beacon peers are connmgr-protected and un-evictable (#80) so the peer table can't simply be crowded out; the running-head quorum (`chain/headcheck`, section 2.8) compares the head's tipset key against independent upstream operators and libp2p peer groups and closes the adoption gate on divergence; and the header-propagation gate only re-gossips CID-verified blocks so a Lantern node can't be turned into an amplifier for a fake chain. F3 finality fully closes the tip exposure once active on mainnet. |
| The operator's wired Bridge (when configured)                 | Wrong `StateCall` receipts and wrong post-execution stateRoots for blocks the operator publishes. NOT: header acceptance, F3, state reads. |
| The operator's local disk (Badger cache)                      | DoS by corruption. Reads still get re-verified on next access. Loss of mempool history. |
| The operator's local network                                  | DoS only. Lantern doesn't talk plaintext for anything security-bearing (RPC + gossipsub are TLS / secio). |
| The DRAND public network                                      | Lantern refuses any beacon whose BLS sig doesn't verify under the embedded public key. No worst case. |
| The Filecoin network majority (>67% F3 power)                 | Game over for Filecoin generally; out of scope for Lantern. |

---

## 6. Operator checklist

Before running Lantern with a bridge in production:

- [ ] The bridge upstream is infrastructure I own (Forest/Lotus on my
      own host, my own LAN, my own VPC). Not a public RPC.
- [ ] The bridge's RPC endpoint is reachable only from my Lantern node,
      not the public internet (or, if exposed, is behind authentication
      and TLS).
- [ ] `AllowBlockSubmit` is `false` unless I am explicitly attempting
      live block production AND I trust my bridge for stateRoot
      computation.
- [ ] I have read this document and understand which operations the
      bridge influences and which it does NOT.

---

## 7. Why this is still better than the status quo

The default Filecoin operator-facing posture (Curio against a Lotus full
node, lotus CLI against api.node.glif.io) trusts the RPC provider for
**everything**: state reads, gas estimates, finality, deal proposals.

Lantern's posture, even with a bridge configured, is:

- All chain-level acceptance (headers, F3, DRAND, state) verified locally.
- Bridge influence limited to message-execution receipts the operator's
  own application consumes (Curio's PSD path; lotus CLI's `state call`).
- The bridge is a deliberate, documented, optional opt-in. Operators who
  don't want any soft trust point have a 100%-local mode available.

**No upgrade in Lantern silently expands the trust scope.** If a future
release adds new operations to the bridge surface, that change must be
documented here and accompanied by a new BLOCKERS file entry. The trust
model is a public contract.
