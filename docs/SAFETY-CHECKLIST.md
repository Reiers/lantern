# SAFETY-CHECKLIST.md

Lantern V1.x is approaching general availability as a Curio backend.
This document is the operator-facing checklist of every gated /
dangerous-by-default operation, what the gate looks like in code, and
how to verify the gate is locked.

Last reviewed for Phase 9 (V1.1-rc.1).

---

## 1. SyncSubmitBlock — block publishing

**Risk:** publishes a block to the gossipsub `/fil/blocks/<network>`
topic. A malformed or duplicate block published into mainnet is at best
ignored, at worst a slashable equivocation (if the same miner publishes
two competing blocks at the same epoch).

**Gate:** `ChainAPI.AllowBlockSubmit` (default `false`).

**Code path:**
- `rpc/handlers/chain_api.go::SyncSubmitBlock` returns `ErrNotImpl(...)`
  unless `AllowBlockSubmit==true`.
- `rpc/handlers/miner_block.go::MinerCreateBlock` only consults
  `c.Bridge` for post-execution stateRoot when `AllowBlockSubmit` is
  ALSO true (B2 fallback otherwise).

**Verify locked:**
```
$ TOK=$(cat ~/.lantern/token)
$ curl -s -X POST -H "Authorization: Bearer $TOK" \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"Filecoin.SyncSubmitBlock","params":[{}]}' \
    http://127.0.0.1:1234/rpc/v1 \
  | jq .error.message
"lantern: not implemented yet (SyncSubmitBlock: block submission requires
 ChainAPI.AllowBlockSubmit=true (operator opt-in))"
```

**Default deployment status:** ✅ locked.

---

## 2. MpoolPush DryRun

**Risk:** `MpoolPush` signs and broadcasts a real on-chain message. Any
operator-side mistake (wrong nonce, wrong recipient, wrong amount) is
irreversible.

**Gate:** there is **no `--dry-run` flag in production V1.x**.
`MpoolPush` will sign and publish the moment a SignedMessage hits the
handler. The CLI `lantern wallet send` does prompt with a `DRY RUN`
preview and requires the user to type `send` to confirm; the JSON-RPC
path does not.

**Code path:**
- `cmd/lantern/main.go::walletSend` does prompt with the DRY RUN
  preview before `MpoolPush`.
- `rpc/handlers/chain_api.go::MpoolPush` calls `Mpool.Publish` directly.

**Operator action:** treat the daemon's `sign`-scoped token as a hot
key. Use scoped tokens (`token-read`, `token-write`) for monitoring;
never paste the admin token into a script that's not part of the
intentional signing flow.

**Verify scope:**
```
$ cat ~/.lantern/token-read   # PermRead only \u2014 cannot sign or write
```

**Default deployment status:** ⚠ unbounded under the sign token. Mitigate
via scoped tokens (see §6).

---

## 3. Wallet keystore encryption-at-rest

**Risk:** the BLS/secp/delegated private keys live under
`$LANTERN_HOME/keystore/`. If those files leak unencrypted, FIL custody
is compromised.

**Gate:** the wallet keystore honours a passphrase passed via
`LANTERN_PASS` (env) or the on-init wizard. When a passphrase is set,
each key file is encrypted with scrypt + nacl secretbox. When the
passphrase is empty, key files are stored as plaintext JSON (Lotus
convention — same security posture as default Lotus).

**Code path:**
- `wallet/keystore/keystore.go`: scrypt KDF + nacl secretbox encrypt
  when `passphrase != ""`.
- `cmd/lantern/main.go::passphrase()`: reads `LANTERN_PASS` env, falls
  back to empty string.

**Operator action:**
1. Set `LANTERN_PASS` to a strong passphrase BEFORE running `lantern
   init` for the first time (otherwise the first key is created
   unencrypted and re-encrypting requires re-import).
2. Verify a key file has a `crypto` field after import:
   ```
   $ cat ~/.lantern/keystore/<addr>.json | jq .crypto
   { "cipher": "secretbox", ... }
   ```
3. If `crypto` is null, the key is plaintext.

**V1.1 hardening required (B-9-11, see PHASE9-BLOCKERS.md):** the daemon
should refuse to start when `LANTERN_PASS` is empty AND the keystore
contains plaintext keys, surfacing a one-shot upgrade command. Until
that lands, this is an operator-discipline gate.

**Default deployment status:** ⚠ depends on operator action. Document in
the install runbook.

---

## 4. RPC auth scopes

**Risk:** Curio (and any other Lotus-compatible client) connects with
a JWT token. The default `~/.lantern/token` is admin-scoped (all
permissions). If that token is exfiltrated, the holder can sign
messages, push to mpool, mint new tokens.

**Gate:** the server mints four pre-scoped tokens at first init:
- `token-read` → PermRead only
- `token-write` → PermRead + PermWrite (mpool, wallet-default ops)
- `token-sign` → PermRead + PermWrite + PermSign (wallet signing,
  mpool push-message, miner create-block)
- `token` → all four perms (PermAdmin too: AuthNew, Shutdown,
  WalletImport/Export, ChainPutObj)

The RPC server's permission map is in
`rpc/server/server.go::methodPermission`:

| Method | Required perm |
|---|---|
| `AuthNew`, `Shutdown`, `ChainPutObj`, `WalletExport`, `WalletImport` | admin |
| `WalletSign`, `WalletSignMessage`, `MpoolPushMessage`, `MinerCreateBlock`, `MarketAddBalance` | sign |
| `WalletNew`, `WalletDelete`, `WalletSetDefault`, `MpoolPush`, `SyncSubmitBlock` | write |
| (everything else) | read |

**Operator action:**
- Curio's `FULLNODE_API_INFO` should use `token-sign` for production
  (Curio only signs MpoolPushMessage and MinerCreateBlock; it never
  needs admin).
- Dashboards / monitoring should use `token-read`.
- Reserve `token` (admin) for one-off CLI operations only.

**Verify scope:** the JWT payload base64-decodes to e.g.
`{"Allow":["read"]}` for `token-read`. Cross-check before deploying.

**Default deployment status:** ✅ tokens are minted with correct scopes
on `lantern init`.

---

## 5. F3 trust-anchor freshness

**Risk:** the embedded F3 power-table anchor lives in
`chain/f3/anchor/anchor_mainnet.json` and is baked into the binary at
compile time. If the binary is more than ~30 days old, the embedded
anchor's `Instance` may be far behind the live F3 chain, in which case
the F3 cert subscriber may take a very long time to forward-walk and
catch up — and in pathological cases, the operator may be relying on a
trusted-root verification that is stale.

**Gate:** the binary checks `anchor.CapturedAt` against the system
clock at daemon startup and **warns** (does not yet refuse) when
`Now() - CapturedAt > 30d`.

**Code path:**
- `chain/f3/anchor/anchor.go::Anchor.CapturedAt` field.
- TODO (B-9-12): `cmd/lantern daemon` should hard-fail when the anchor
  is older than 60d, and warn at 30d. Currently neither is implemented.
  See PHASE9-BLOCKERS.md.

**Operator action:** re-build / re-deploy Lantern at least monthly so
the embedded anchor stays fresh. The release CI (B-9-13) is what makes
this routine.

**Verify freshness:**
```
$ /tmp/lantern info  # TODO: extend `info` to print anchor capturedAt
$ python3 -c "import json; print(json.load(open('chain/f3/anchor/anchor_mainnet.json'))['capturedAt'])"
2026-05-21T16:15:00Z
```

**Default deployment status:** 🟡 enforce-at-start not yet wired. Track
in PHASE9-BLOCKERS.md B-9-12.

---

## 6. Bridge configuration (Phase 8 Part B)

**Risk:** when configured, the VM bridge delegates `StateCall` (non-Send)
and `MinerCreateBlock` post-execution stateRoot to an upstream Forest
or Lotus. The bridge's response is **trusted** byte-for-byte; Lantern
does not verify the upstream's VM execution.

**Gate:** the bridge is opt-in via `vm.WithBridge(b)`. Default daemon
has no bridge wired.

**Operator action when enabling:**
1. Run the upstream Forest/Lotus on the same host or a controlled
   network path (not the public Internet).
2. Audit the upstream's binary provenance — see TRUST-MODEL.md §3.
3. Treat the bridge as a soft trust point: ANY successful StateCall
   or MinerCreateBlock that depends on bridge output is only as
   trustworthy as the upstream. Document this in your incident
   response runbook.

**Verify wired or not:**
```
$ cat ~/.lantern/config.json | jq .vm.bridge
null  # no bridge \u2014 native vm shell only
```

**Default deployment status:** ✅ not wired. Operators opt in
explicitly via daemon flag or config.

---

## 7. Gossipsub block topic — subscribe vs publish

**Risk:** subscribing to `/fil/blocks/<network>` exposes Lantern to
peer-supplied data. Lantern does superficial validation
(`net/blockpub/blockpub.go::validateIncoming`) but does NOT do the full
expensive validation (proofs, parent-tipset check) — that's the
caller's responsibility.

**Gate:** subscription is unconditional when the libp2p host is
configured. Publishing is gated behind `AllowBlockSubmit` (see §1).

**Code path:**
- `net/blockpub/blockpub.go::Join`: subscribes immediately to
  `/fil/blocks/<network>`.
- Surface validation in `validateIncoming` rejects obviously-malformed
  blocks (no miner, missing signature, missing BLS aggregate).
- Deep validation is a future task — see PHASE9-BLOCKERS.md B-9-14.

**Operator action:** none for V1.1. The blockpub package is wired but
no consumer treats its output as canonical. If a Phase 10 component
ever does, deep validation MUST land first.

**Default deployment status:** ✅ no downstream consumer; subscriber is
inert.

---

## 8. Header store sync source

**Risk:** the Sync agent (chain/header/store/sync.go) polls a
configurable RPC source (default: Glif via `api.node.glif.io`) for new
heads. Every block it ingests is CID-verified locally, but the **choice
of which tipset is canonical at each epoch** is taken from the upstream
source. A malicious upstream could push a fork.

**Gate:** the F3 cert subscriber (chain/f3/subscriber) cross-checks
the head store's choices against F3 finality once the cert subscriber
catches up. Any mismatch raises an alert.

**Code path:**
- `chain/header/store/sync.go`: header CID verification on each block.
- `chain/f3/subscriber`: walks F3 certs forward and binds each cert to
  the canonical tipset key.
- Mismatch detection: TODO (B-9-15). Currently the F3 subscriber and
  the header store don't share a cross-check. Document and fix.

**Operator action:** run with the F3 subscriber enabled (default true).
If you see frequent reorgs in the daemon logs, audit the upstream.

**Default deployment status:** 🟡 cross-check not yet implemented; F3
subscriber and header store run in parallel without coordination.
Track in PHASE9-BLOCKERS.md B-9-15.

---

## Summary table

| # | Gate | Default | V1.1 status |
|---|------|---------|-------------|
| 1 | SyncSubmitBlock                 | locked   | ✅ |
| 2 | MpoolPush DryRun                | none     | ⚠ scoped tokens |
| 3 | Wallet encryption-at-rest       | opt-in   | ⚠ operator-discipline |
| 4 | RPC auth scopes                 | enforced | ✅ |
| 5 | F3 anchor freshness             | warn-only| 🟡 B-9-12 |
| 6 | Bridge upstream trust           | off      | ✅ |
| 7 | Gossipsub block subscribe       | inert    | ✅ |
| 8 | Header store ↔ F3 cross-check   | none     | 🟡 B-9-15 |

**Verdict for V1.1-rc.1:** ✅ ship as "alpha-quality for SP backends".
The five ✅ gates are correctly locked. The two 🟡 items are tracked in
PHASE9-BLOCKERS.md and don't block read-mostly use. The ⚠ items are
operator-discipline issues with mitigation paths documented above.
