# Lantern menu-bar app (macOS)

Native SwiftUI menu-bar companion for the local Lantern daemon. Per
[INSTALLER-SPEC.md §6b](../../INSTALLER-SPEC.md), V1.2 GA scope.

## What it does (MVP)

- **Menu-bar icon** with the chain head epoch next to a 🪔 glyph
- **Click** → popover with chain head, peer count, F3 anchor instance,
  network, and a quorum-state indicator (● green / ● amber / ● red)
- **Buttons** to refresh, start/stop the daemon (via `lantern service`),
  and quit the app
- **Updates every 5 seconds** while the popover is open

Talks to the local Lantern daemon at `http://127.0.0.1:1234/rpc/v1`,
authenticated with the JWT in `~/.lantern/token`. Reads
`~/.lantern/bootstrap-anchor.json` for the F3 anchor metadata and
derives the quorum-state indicator from anchor recency (green < 24h,
amber < 7d, red otherwise).

## Building

```sh
cd apps/mac
swift build -c release             # builds the executable
Scripts/build_and_sign.sh          # wraps it in Lantern.app
```

Output: `apps/mac/.build/Lantern.app`. Install with:

```sh
cp -R apps/mac/.build/Lantern.app /Applications/
open /Applications/Lantern.app
```

Optional signing: `APPLE_CODESIGN_IDENTITY="Developer ID Application: …"
Scripts/build_and_sign.sh`. Without signing, run
`xattr -dr com.apple.quarantine /Applications/Lantern.app` to bypass
Gatekeeper on the dev machine.

## Architecture

- `Sources/Lantern/LanternApp.swift` — `MenuBarExtra` scene + popover
  view
- `Sources/Lantern/DaemonStatus.swift` — `ObservableObject` that polls
  the local JSON-RPC and exposes `@Published` chain state to the view
- `Package.swift` — SwiftPM manifest (no Xcode project)
- `Scripts/build_and_sign.sh` — wraps the SwiftPM binary into a
  proper `.app` bundle with `Info.plist`

The app is `LSUIElement=true` so it doesn't create a Dock icon — it
only lives in the menu bar.

## What's deferred to V1.2.1 / V1.3

- Real-time quorum state via a dedicated daemon endpoint (today the
  indicator is a recency heuristic on the anchor file)
- Settings pane: quorum sources, JWT scopes, beacon-mode toggle
- "View logs" button that tails `~/.lantern/lantern.log`
- Sparkle auto-update once we ship signed/notarised release artifacts
- Login-item registration on first launch
