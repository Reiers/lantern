# #51 — Stale-restart auto-heal + keystore-safety (the "down for a week" footgun)

Status: **SCOPED, not started** · Found: 2026-06-18 (tester loop)
· Blocks: public tester announcement (every tester who restarts after a
multi-day pause hits the same trap)

## The bug, in one sentence
After a multi-day daemon stop, Lantern boots stuck on the on-disk anchor
+ headerstore from the last run, and the only currently-documented
recovery (`rm -rf ~/.lantern/headerstore` and/or `bootstrap-anchor.json`)
sits **next to the keystore in the same data dir**, so the natural user
recipe destroys irreplaceable secrets.

## Reproduction (live, 2026-06-18 11:30→11:50 CEST)
1. Daemon last ran 2026-06-09 with anchor + headerstore persisted.
2. Today: `LANTERN_PASS='' ~/.lantern/lantern daemon --metrics 127.0.0.1:9092`.
3. Status UI: `Chain head 6,090,619 · updated 8d ago · Status: Reconnecting`.
   `lantern_fetch_total{source="gateway"} 0` — v1.7.17 mainnet fallback
   does NOT engage on cold-start; it only engages on a failed gossip
   parent fetch in steady state.
4. `rm -rf ~/.lantern/headerstore` → restart → still stuck on the same
   tip, because `bootstrap-anchor.json` pins the start point.
5. `rm -f ~/.lantern/bootstrap-anchor.json && rm -rf ~/.lantern/mainnet`
   + restart → catches live head in seconds (5-source F3 picks the live
   anchor), but `~/.lantern/mainnet/keystore/` was wiped in step 5.
   **Wallets deleted.** (In this run the affected keystore happened to
   be empty test addresses, so no loss — but the path is the same for a
   user with real FIL.)

## Why this is a release blocker
Every public tester who runs the daemon, then sleeps the laptop / closes
the lid / goes on vacation, will come back to this exact state. The
"obvious" fix anyone (including us) reaches for is to clear state —
which today means picking through `~/.lantern/` with `rm -rf` and hoping
they don't catch a key directory.

## Two unrelated classes of state share one directory
The root cause is that we colocate things with very different lifetimes:

| Class | Files | Recoverable? | Lifetime |
|---|---|---|---|
| **User secrets** | `mainnet/keystore/`, `jwt-secret`, `token*`, `keystore/` (top-level, currently empty) | ❌ Irreplaceable | Forever |
| **Chain state** | `mainnet/headerstore/`, `bootstrap-anchor.json` | ✅ Rebuilt from network | Per-session, can be stale |

…and our installer/restart/recover story doesn't separate them.

## Fixes to ship (proposed staging)

### Stage 1 — stop the bleeding (small, ship as v1.7.18)
1. **Auto-refresh `bootstrap-anchor.json` when stale.**
   On daemon start: if `bootstrap-anchor.json` mtime > 24h (configurable
   `--bootstrap-max-age`, default 24h) OR the anchored epoch is more
   than `N` epochs behind the F3-finality height we can quickly poll
   from any one source, **re-run the 5-source F3 bootstrap and overwrite
   the anchor file**. With the v1.7.17 gateway fallback in place this is
   safe (we always have a path to advance the head); without auto-refresh
   the fallback can't help because the anchor pins us to an old tip.

2. **Headerstore self-prune on impossible-catchup.**
   On boot, after the anchor is refreshed: if the headerstore's highest
   tipset is more than `M` epochs behind the new anchor (default
   M = 28800, ~10 days), drop forward state and resync from the anchor.
   Same lifetime/recovery class as the anchor itself, so this is safe.

3. **`lantern reset --chain-state` subcommand.**
   Documented, named, opt-in, explicit. Does exactly steps 1+2 + clears
   bitswap blockstore. **Refuses to touch any of: `keystore/`,
   `mainnet/keystore/`, `jwt-secret`, `token*`.** This becomes the
   official recovery story.

4. **README + status UI hint.**
   When the status UI shows "node is several minutes/hours/days behind",
   point at `lantern reset --chain-state`, NOT at `rm -rf`.

### Stage 2 — defense in depth (separate issue, breaking-change)
5. **Move keystore out of the per-network state dir.**
   Settle on a single top-level `~/.lantern/keystore/` (or, better,
   `~/.lantern/secrets/` containing `keystore/`, `jwt-secret`, `token*`),
   and have the network-state dirs (`mainnet/`, `calibration/`)
   contain **only** rebuildable chain state. Needs a migration step
   on first run of the new version that moves any
   `mainnet/keystore/*` → `secrets/keystore/*` (and same for calibration)
   and leaves a stub README in the old location.

6. **Auto-backup of `secrets/` on every daemon start.**
   Tar `secrets/` to `~/.lantern/backups/secrets-YYYYMMDD-HHMMSS.tar`,
   keep last 7. Costs ~kilobytes per run. Means even a "user `rm -rf`
   the whole lantern dir" event has a same-machine recovery path until
   the user wipes the backups too.

7. **Installer pre-flight.**
   `install.sh` already has a preflight; add: if `~/.lantern/mainnet/keystore/`
   or `~/.lantern/secrets/keystore/` exists and is non-empty, make a
   `~/.lantern/secrets-pre-install-YYYYMMDD-HHMMSS.tar` next to the data
   dir before doing anything else. Costs ~nothing, saves the upgrade case.

## Scope this issue
Stage 1 only (1, 2, 3, 4). Stage 2 (5, 6, 7) gets its own issue once we
agree on the secrets/ layout — that one is breaking and needs a migration
test plan.

## Tester-announcement gate
**Do not post the Curio-channel tester announcement (draft at
`projects/lantern/ANNOUNCEMENT-DRAFT-2026-06-17.md`) until Stage 1 ships
as v1.7.18.** Otherwise the first 24h of "I tried it, came back the next
weekend" feedback will be people who lost keys to a recipe we gave them.

## Acceptance
- Daemon stopped for 9 days, restarted with the v1.7.18 binary, with no
  user intervention, catches live head within 1 minute and shows
  Status: Online.
- `lantern reset --chain-state` is documented in README + `--help`, and
  removes only chain state. Running it with `--include-secrets` is
  refused unless `--i-know-this-deletes-my-keys` is passed.
- Status UI's "node is behind" panel links to the documented recovery,
  not to manual `rm`.
- No path in the README, installer, status UI, or `--help` instructs
  the user to `rm` anything under `~/.lantern/` directly.
