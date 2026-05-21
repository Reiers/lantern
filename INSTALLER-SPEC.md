# Lantern installer + setup spec

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
4. **Requires ≥ 3 sources to agree** on the same finalized tipset + state root before accepting it as the current head.
5. If 3 sources don't agree, the installer **refuses to continue** and prints a clear error: "Could not reach quorum of 3 independent sources within 60s. Run `lantern bootstrap --debug` to see per-source responses."

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

### 6. Daemon launchd / systemd service (10s)

Filbucket runs as a foreground process. Lantern should be persistent. The installer offers:

- macOS: writes `~/Library/LaunchAgents/io.lantern.daemon.plist`, registers, starts
- Linux: writes `~/.config/systemd/user/lantern.service`, registers, starts
- Either: starts in foreground (default for ephemeral testing)

Asks before installing the service. Adds `lantern stop` and `lantern restart` aliases for the user-service lifecycle.

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

The default quorum sources, baked into the binary:

1. **libp2p mainnet bootstrap peers** — the same list Lotus and Forest use
2. **`gateway.lantern.reiers.io`** — our gateway (optional, falls back gracefully)
3. **`forest-archive.chainsafe.dev`** — ChainSafe's public Forest archive
4. **DHT-discovered Lantern beacons** under `lantern/beacon/v1` rendezvous

Users can:
- Add their own: `lantern init --peer http://my-forest:2345/rpc/v1 --peer http://other-forest:2345/...`
- Require more than 3: `--bootstrap-quorum 5`
- Bring their own Forest: `--peer http://my-forest:2345/rpc/v1 --bootstrap-quorum 1` (single-source if they trust their own infra)

Production deployments (SPs, exchanges) would typically configure their own Forest + 2 public sources for a balanced 3-of-3 quorum.

## Stretch goals (V2 of the installer)

- **Interactive TUI** for advanced options: choose quorum sources, choose JWT scopes, choose beacon-mode-on-startup
- **Mac-style first-run notification** when Curio / other tooling first binds to Lantern
- **`lantern doctor`** subcommand: re-runs the bootstrap quorum check on demand, shows divergence if any source has drifted
- **`lantern beacon` subcommand** (already in Phase 10 scope): turn the local Lantern into a state-serving beacon for the swarm
- **`lantern repair`** subcommand: when the embedded anchor goes stale, re-anchor from the live swarm without a full reinstall

## What this changes for the V1.1 release

The current `v0.1.0-rc.1` tag is what's running on sp.reiers.io and what an early adopter would download. It works, but it relies on the embedded anchor as the sole trust foundation, and the cold-start path goes through the gateway. That's fine for alpha; it's not the V1.1 GA shape.

V1.1 GA ships:

1. The Phase 10 swarm work (libp2p bandwidth, real peer count, beacon subcommand) — in progress
2. The install.sh + `lantern init --bootstrap=N` quorum work — this doc
3. `lantern doctor` + `lantern repair`
4. Reproducible builds + GitHub-Actions-signed release artifacts

That's the GA story: **download, ≤ 3 minutes, fully verified, fully independent of any single provider**.
