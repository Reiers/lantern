# Lantern installer + setup spec

> **Status (Phase 11):** Implementation **shipped** in v0.5.0 / V1.2-rc.1. The bootstrap quorum, `install.sh`, `lantern doctor`, `lantern repair`, `lantern service`, native Mac menu-bar app, and tag-triggered release pipeline are all in main. See `PHASE11-BLOCKERS.md` for the delivery report and `docs/phase11-install-evidence.md` for an end-to-end transcript proving 5 independent sources agreed on the same finalized tipset on real mainnet.

The promise: one command on a fresh Mac, three minutes later you have a working Filecoin light node that cryptographically agrees with the canonical mainnet chain, with **no dependency on Glif or any single provider**.

```sh
curl -fsSL https://get.lantern.reiers.io | bash
```

## What it does, in order

### 1. Preflight (5s)

- Verifies `brew`, `git`, basic build tools.
- Detects macOS arm64 vs Intel, picks the right pre-built `lantern` binary URL.
- (Linux: same flow, picks `linux/amd64` or `linux/arm64`.)
- Skips everything if `~/.lantern/lantern` already exists and `--reinstall` not passed.

### 2. Download the binary (30s)

- Pulls the latest signed release binary from `github.com/Reiers/lantern/releases/latest/...`
- Verifies SHA-256 against the release notes' published hash.
- Installs to `~/.lantern/lantern` and symlinks `/usr/local/bin/lantern` (with `sudo` if needed, asks first).

### 3. Trust bootstrap — the part that matters (60–90s)

This is the new piece. **No single source determines the trust anchor.** The installer:

1. **Connects to libp2p mainnet** using the canonical bootstrap peer set baked into the binary (the same list every Lotus / Forest / Curio uses).
2. **Reaches out to N independent chain-head sources in parallel**:
   - 5+ libp2p peers via direct queries
   - Embedded gateway at `gateway.lantern.reiers.io` (if reachable)
   - Public Forest archive APIs at ChainSafe (`forest-archive.chainsafe.dev`)
   - Any Lantern beacons announced via DHT under `lantern/beacon/v1` rendezvous
   - Optional: user-configured trusted nodes via `--peer` flags
3. **Asks each source: "give me the F3 finality certificate for the latest GPBFT instance"**
4. **Requires ≥ 5 sources to agree** on the same finalized tipset + state root before accepting it as the current head. Five is cheap (cold-start only, runs once) and a meaningful step beyond "trust one provider." SP-grade deployments can raise this further with `--bootstrap-quorum`.
5. If 5 sources don't agree, the installer **refuses to continue** and prints a clear error: "Could not reach quorum of 5 independent sources within 60s. Run `lantern doctor` to see per-source responses."

This is the killer property: the installer mathematically cannot be fooled by a single compromised gateway, RPC provider, or even by us (the Lantern project) running a malicious beacon. **Verification quorum at install time is the trust foundation everything else builds on.**

After quorum: the validated finalized tipset becomes the live trust anchor. From there, every subsequent block, beacon, and state CID is verified locally via BLS + content-addressing, exactly as today.

### 4. Embedded F3 anchor cross-validation (30s)

The binary ships with a pre-pinned F3 power-table snapshot. The installer **cross-validates** it against what the live swarm reports:

- Walks F3 certs from the embedded anchor's instance forward to the live finalized instance (typically 50-200 certs, seconds to verify)
- If walk succeeds: the embedded anchor is good, we're on the canonical chain
- If walk fails: refuses to start, prints the divergence point, suggests `lantern bootstrap --reset-anchor` to capture a fresh anchor from the swarm

This catches the "shipped a stale or forged anchor" failure mode without any reliance on a trusted publisher.

### 5. Wallet + JWT setup (15s)

Same as today's `lantern init`:
- Generate a fresh BLS wallet (offered, can skip)
- Mint JWT auth tokens (admin / sign / write / read)
- Write `~/.lantern/` files with sensible perms

### 6. Daemon: user picks the lifecycle (10s)

Filbucket runs as a foreground process. Lantern can be either. The installer **asks the user** which they prefer:

- **Background service** — macOS launchd / Linux systemd user service. Survives reboot, restarts on failure, logs to `~/.lantern/lantern.log`. Best for production / SP use.
- **Foreground** — just starts `lantern daemon` in the current shell. Good for testing, debugging, or one-off use. Survives until the shell closes.
- **Skip** — install only, start manually later with `lantern daemon`.

Default depends on context: non-interactive (`--yes`, `LANTERN_YES=1`) defaults to background service. Interactive default is foreground (less surprising for first-run users; they can re-run with `lantern service install` later).

Adds `lantern stop` and `lantern restart` aliases for the user-service lifecycle.

### 6b. Native Mac app (optional, V1.2)

For users who want zero-CLI Lantern, ship a SwiftUI menu-bar app:

- Menu-bar icon shows current chain head epoch + peer count at a glance
- Click for a popover with `lantern info` status
- Right-click for daemon controls (start / stop / restart, view log)
- Quorum status indicator: green if all configured sources currently agree, amber if N−1, red if quorum lost
- Settings pane: choose quorum sources, JWT scopes, beacon mode toggle
- Auto-updates via Sparkle once we ship signed release artifacts

Lives at `apps/mac/` (mirrors filbucket's layout). SwiftPM only, no Xcode project required. Optional install at the end of the bash flow:

```
? Install the Lantern menu-bar app? [Y/n]
```

If yes: download signed `.app`, install to `/Applications/Lantern.app`, register it as a login item if the user opts in. The app talks to the local Lantern daemon via the same JSON-RPC; no extra trust surface.

### 7. Final state (10s)

- Prints `lantern info` output
- Prints the FULLNODE_API_INFO string
- Prints next-steps hints:
  - "Connect Curio / Boost: export FULLNODE_API_INFO=..."
  - "Query the chain: lantern chain head"
  - "Run as a beacon for others: lantern beacon"
- Closes with a quote (filbucket-style flavor) and exits.

Total time: **~3 minutes on a Mac with decent network**.

## What the user sees

```
$ curl -fsSL https://get.lantern.reiers.io | bash

       🪔  Lantern
       Pure-Go Filecoin light node
       no CGo, no 76 GB snapshot, no third-party trust

  ▸ Preflight
    ✓ macOS 26.4 / arm64
    ✓ Homebrew, git, curl found
    ✓ ~/.lantern/ ready

  ▸ Downloading Lantern v0.2.0-rc.1 (darwin/arm64)
    ✓ 36 MB downloaded
    ✓ SHA-256 verified
    ✓ Installed to /usr/local/bin/lantern

  ▸ Trust bootstrap — establishing chain head from swarm
    ⠋ Connecting to libp2p... 8 peers
    ⠙ Querying F3 finality from sources...
    ⠹    ✓ libp2p peer 12D3KooW... (epoch 6,036,160)
    ⠸    ✓ libp2p peer 12D3KooW... (epoch 6,036,160)
    ⠴    ✓ ChainSafe Forest archive (epoch 6,036,160)
    ⠦    ✓ gateway.lantern.reiers.io (epoch 6,036,160)
    ✓ 4 sources agree on F3-finalized tipset at instance 467,103
    ✓ State root: bafy2bzacecmh...
    ✓ Trust anchor established

  ▸ Cross-validating embedded F3 anchor
    ✓ Embedded anchor (instance 466,353, captured 2026-05-21)
    ✓ Walked 750 certs forward, BLS aggregate sigs verified at each step
    ✓ Embedded anchor cleanly walks to live head

  ▸ Wallet + auth
    ✓ Created BLS wallet f3r53...rq5r27pl3yn
    ✓ JWT tokens minted (admin, sign, write, read)

  ▸ Daemon service
    ? Install Lantern as a launchd user service to run in background?  [Y/n] y
    ✓ Installed io.lantern.daemon.plist
    ✓ Service started

  ✓ Lantern is running

    Status:        lantern info
    Connect Curio: export FULLNODE_API_INFO='eyJ...:/ip4/127.0.0.1/tcp/1234/http'
    Try a query:   lantern chain head
    Serve others:  lantern beacon

    "The lighter the node, the brighter the chain."

  Lantern home: ~/.lantern
  Logs:         tail -f ~/.lantern/lantern.log
```

Errors are equally well-shaped: red `✗`, single-line summary, actionable next step.

## Implementation: `cmd/lantern/init.go`

Today's `lantern init` is a minimal wizard. Expanding it into a full bootstrap-aware setup is the right home. The shell `install.sh` becomes a thin wrapper:

1. Download binary
2. `lantern init --bootstrap=3` (the new flag enforcing ≥ 3-source quorum)

Then `init` itself does all the trust-bootstrap work in Go. This keeps the cryptographic guarantees on the strict path (compiled Go) instead of trying to do quorum logic in bash.

## What the shell installer does that init doesn't

- OS detection + dependency check
- Pre-build binary selection (avoids requiring Go on the user's machine)
- launchd / systemd service registration (OS-specific, fine in bash)
- Symlink to `/usr/local/bin` so `lantern` works without PATH gymnastics

Everything else is `lantern init` in Go.

## Multi-source quorum is the architectural win

We have been treating "embedded F3 anchor" as the trust foundation. With the multi-source quorum at bootstrap time:

- Today's anchor: trusted because the build is reproducible and we publish it
- New bootstrap quorum: trusted because **3 mutually-independent sources cryptographically agree on the chain head**

Both layers exist. The quorum is **defense in depth** on top of the anchor. An attacker would need to compromise:

1. Our build pipeline (to ship a bad anchor) **AND**
2. ≥ 2 of the user's chosen quorum sources (to make the bootstrap accept the same forged chain)

That's an unrealistic threat surface for a light client. Most existing Filecoin tooling (Boost, web wallets, dashboards) has one trust point — whichever RPC they pin. Lantern would have **three independent ones at startup** by default, configurable to more.

## Configurable quorum sources

The default quorum candidates, baked into the binary, ranked by independence:

1. **libp2p mainnet bootstrap peers** — the same list every Lotus, Forest, and Curio uses. We query ≥5 of them directly via libp2p; if they all respond with the same head, that's already a real quorum from independent operators (the bootstrap list spans Protocol Labs, ChainSafe, Glif, and others, on geographically diverse infra).
2. **`forest-archive.chainsafe.dev`** — ChainSafe's public Forest archive, completely independent operator from us.
3. **DHT-discovered Lantern beacons** under `lantern/beacon/v1` rendezvous — ad-hoc community state-serving nodes. Counted in the quorum once we have a healthy ecosystem of beacons; today, treated as a bonus source.
4. **User-configured `--peer` flags** — the user's own Forest / Lotus / RPC endpoint(s). Counted toward quorum.
5. **`gateway.lantern.reiers.io`** — our gateway. **NOT counted in the quorum by default.** If the user already trusts us they trust the binary; the quorum's value comes from independent operators. The gateway is still used as a fast fallback for raw IPLD block fetches *after* the trust anchor is established — just not part of the bootstrap consensus.

Users can:
- Add their own: `lantern init --peer http://my-forest:2345/rpc/v1 --peer http://other-forest:2345/...`
- Lower the quorum (not recommended unless you really know your sources): `--bootstrap-quorum 3`
- Single-source mode for users who run their own Forest + trust it: `--peer http://my-forest:2345/rpc/v1 --bootstrap-quorum 1`
- Raise the quorum for paranoid deployments: `--bootstrap-quorum 7`

Default is **5 agreeing sources**. From the available pool, we draw enough to reach 5. The math: if a user adds zero `--peer`s, the default mix is ≥5 libp2p peers + ChainSafe Forest archive + any DHT-announced Lantern beacons reachable in 60s. Cheap because it runs once at install time, not on every query.

## Stretch goals (V2 of the installer)

- **Interactive TUI** for advanced options: choose quorum sources, choose JWT scopes, choose beacon-mode-on-startup
- **Mac-style first-run notification** when Curio / other tooling first binds to Lantern
- **`lantern doctor`** subcommand: re-runs the bootstrap quorum check on demand, shows divergence if any source has drifted
- **`lantern beacon` subcommand** (already in Phase 10 scope): turn the local Lantern into a state-serving beacon for the swarm
- **`lantern repair`** subcommand: when the embedded anchor goes stale, re-anchor from the live swarm without a full reinstall

## V1.2 is the GA target (V1.1 stays as alpha + Phase 10)

The current `v0.1.0-rc.1` tag is what's running on sp.reiers.io. It works, but it relies on the embedded anchor as the sole trust foundation and the cold-start path goes through our gateway. That's fine for alpha. It is not the GA shape we want.

**V1.1** ships the Phase 10 swarm work — real libp2p peer count + bandwidth in the RPC surface, Bitswap as primary fetch path, `lantern beacon` subcommand. Still has the embedded anchor as sole trust foundation; the live-quorum bootstrap is V1.2.

**V1.2** ships the install story. Phase 11 delivery status:

1. ✅ `install.sh` + `lantern init --bootstrap-quorum=5` multi-source quorum bootstrap
2. ✅ `lantern doctor` + `lantern repair` subcommands
3. ✅ `lantern service install/uninstall/start/stop` (launchd + systemd)
4. ✅ SwiftUI menu-bar app at `apps/mac/` (MVP — settings pane is V1.2.1)
5. ✅ Reproducible builds + GitHub Actions release workflow; first signed-and-notarised macOS release is a separate step (B-11-03)
6. 🟡 `get.lantern.reiers.io` serving `install.sh` — trivial DNS/worker setup (B-11-06), pending Cloudflare config

That's the GA story: **download, ≤ 3 minutes, fully verified, fully independent of any single provider, optional Mac app for non-CLI users**.
