# Lantern v1.7.23 — keystore & service-start fix

**Update if:** Lantern asks you for a passphrase **every** time it starts, or it
**fails to start as a background service** (systemd / launchd) and only works
when you run it by hand (e.g. in `screen`/`tmux`). This release fixes both.
If neither bites you, this update is optional.

## What changed

**Passphrase no longer re-prompts on every start.** Lantern holds no signing
keys of its own (signing lives in your Curio / curio-core wallet), so its
keystore is empty. The old build read an empty keystore as "brand-new node"
on **every** boot and re-asked you to set a passphrase each time. It now
records your choice once and stays quiet on subsequent starts. (#3)

**Background-service install now starts cleanly on a fresh node.** Same root
cause: a first boot with no terminal attached (a systemd/launchd unit) hit the
"no passphrase, no TTY" path and the daemon exited before it could remember
anything — so the service died and only a hand-run in `screen` worked. A node
with no keys has nothing to encrypt, so a keyless keystore + no terminal now
defaults to an unencrypted keystore, records it, and starts. (#1)

## Behavior details

- Empty keystore + no `LANTERN_PASS` + no terminal → starts unencrypted,
  prints one notice, and writes a `.unencrypted` marker so it never re-prompts.
- Want encryption? Set `LANTERN_PASS` (or, for systemd, an
  `EnvironmentFile=/etc/lantern/passphrase`, `chmod 600`) **before first boot**.
- **Safety unchanged:** if a keystore actually *holds* keys and there's no
  passphrase and no terminal, Lantern still refuses to start rather than run
  with an empty passphrase against real keys.

To switch an already-unencrypted node to encrypted later: delete
`<data-dir>/<network>/secrets/keystore/.unencrypted` and set `LANTERN_PASS`.

## Linux note

For a systemd **user** service, run `loginctl enable-linger <user>` so the
service survives logout.

## Not in this release (in progress)

If your node periodically falls behind the chain head with
`api.node.glif.io ... context deadline exceeded` in the logs, that's a known,
separate issue (#53): header backfill still falls back to Glif, so a slow Glif
makes the head lag. Live blocks over libp2p are unaffected; 0–2 epochs behind
is normal. A fix to move backfill onto the p2p path is being worked next.

## Verification

Built `CGO_ENABLED=0`, full keystore/wallet/header-store test suites green
(incl. the keys-present-no-TTY-must-fail-loud invariant), and verified
end-to-end with the real binary: a fresh no-terminal start exits 0 and writes
the marker; the second start is silent.
