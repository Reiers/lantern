# Lantern v1.8.0 — security hardening: trust model, bootstrap, auth

**Update if:** you run a Lantern node that holds keys, expose any Lantern
listener beyond loopback, or care about the trust boundary of the boot anchor.
This is a security-hardening release. There are no protocol or wire changes;
the data-integrity model (every fetched block is content-address-verified) was
already sound. These changes close the gaps *around* that core — anchor
authenticity, transport, auth, and key-at-rest.

A proactive self-audit (#60) of the trust/bootstrap/auth surface produced six
fixes. None require new external dependencies.

## Highlights

### Verified boot anchor (#54, the headline)
The daemon previously accepted the chain head from a single source (gateway, or
Glif fallback) on faith. Content-addressing verifies state *under* a root, but
not the *choice* of root — a hostile/compromised gateway or a MITM on the
plain-HTTP `/state/root` fetch could seed a valid-but-non-canonical fork or a
stale head. The boot path now:

- requires agreement from **≥2 independent sources** (gateway + Glif) on
  `(StateRoot, TipSetKey)`,
- **cross-checks against the latest F3 finality certificate** (rejects any
  candidate at/below F3 finality whose tipset differs — fork-below-finality),
- prefers the heavier `ParentWeight` only when F3 proves it safe, and otherwise
  **refuses to boot**.

`--insecure-anchor` restores single-source boot for localhost/dev.

### RPC auth on the write path (#56)
The `Filecoin.*` permission-gating was correct, but `eth_*` methods bypassed it
(underscore-separated names skipped the namespace switch), so the live signing
write path **`eth_sendRawTransaction` was callable with the default
unauthenticated read perms**. It now requires `sign`. Other `eth_*` reads stay
open so dapps/synapse-sdk work tokenless. The daemon also **refuses a
non-loopback RPC bind** without `--allow-remote-rpc`.

### Keystore fail-loud (#58, also closes #2)
An empty passphrase on a keystore that already holds keys (on a write-path node,
*funded signing keys*) was accepted with a warning. It is now **refused** unless
the operator explicitly opts in via `LANTERN_ALLOW_EMPTY_PASS=1` /
`--allow-empty-passphrase`.

### Defense in depth
- **#55** — plain-`http://` gateway URLs are refused unless `--insecure-gateway`
  (loopback exempt). Default gateway is already `https://`.
- **#57** — the dashboard/metrics listener refuses a non-loopback bind without
  `--allow-remote-dashboard`, and **requires a `LANTERN_DASHBOARD_TOKEN`** Bearer
  when so bound.
- **#59** — a built-in trusted beacon floor seeds cert-exchange before DHT
  discovery warms, and the DHT-discovered pool is **capped** (anti-eclipse). F3
  certs were already BLS-verified; this addresses availability/eclipse, not
  forgery.

## Upgrade notes

- **Default (loopback) deployments: no action.** New guards only trigger on
  non-loopback binds or empty-passphrase-with-keys.
- If you **rebind the RPC or dashboard off loopback**, you must now pass the
  corresponding `--allow-remote-*` flag (and set `LANTERN_DASHBOARD_TOKEN` for
  the dashboard).
- If you intentionally run an **unencrypted keystore with keys**, set
  `LANTERN_ALLOW_EMPTY_PASS=1`.
- If you point `--gateway` at a plain-`http://` non-loopback URL, add
  `--insecure-gateway`.

## Verification

CGO-free build, `go vet`, and the hermetic test suite are green across the
module. New unit tests cover every decision path in the six fixes. The live
mainnet node was not touched during development.
