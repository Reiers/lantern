#!/usr/bin/env bash
#
# Build the Lantern menu-bar app and bundle it as Lantern.app.
#
# Usage:
#   apps/mac/Scripts/build_and_sign.sh [--release|--debug]
#
# Output: apps/mac/.build/Lantern.app
#
# If APPLE_CODESIGN_IDENTITY is set, the .app is signed with that
# identity. Otherwise it's left unsigned (Gatekeeper will block;
# install via `xattr -dr com.apple.quarantine /Applications/Lantern.app`).

set -euo pipefail

cd "$(dirname "$0")/.."

CONFIG="release"
if [[ "${1:-}" == "--debug" ]]; then CONFIG="debug"; fi

echo "▸ Building SwiftPM target (configuration=$CONFIG)..."
swift build -c "$CONFIG"

EXE=".build/$CONFIG/Lantern"
if [[ ! -x "$EXE" ]]; then
  echo "✗ Build did not produce $EXE" >&2
  exit 1
fi

APP=".build/Lantern.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
mkdir -p "$APP/Contents/Resources"

cp "$EXE" "$APP/Contents/MacOS/Lantern"

cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key>             <string>Lantern</string>
  <key>CFBundleDisplayName</key>      <string>Lantern</string>
  <key>CFBundleIdentifier</key>       <string>io.lantern.menubar</string>
  <key>CFBundleExecutable</key>       <string>Lantern</string>
  <key>CFBundlePackageType</key>      <string>APPL</string>
  <key>CFBundleShortVersionString</key> <string>1.2.0</string>
  <key>CFBundleVersion</key>          <string>1</string>
  <key>LSMinimumSystemVersion</key>   <string>13.0</string>
  <key>LSUIElement</key>              <true/>
  <key>NSHighResolutionCapable</key>  <true/>
</dict>
</plist>
PLIST

if [[ -n "${APPLE_CODESIGN_IDENTITY:-}" ]]; then
  echo "▸ Signing with identity: $APPLE_CODESIGN_IDENTITY"
  codesign --force --options runtime --timestamp \
    --sign "$APPLE_CODESIGN_IDENTITY" \
    "$APP"
  codesign --verify --verbose=2 "$APP"
else
  echo "▸ APPLE_CODESIGN_IDENTITY not set; leaving bundle unsigned."
fi

echo "✓ Built $APP"
echo "  Install: cp -R $APP /Applications/"
echo "  Run:     open /Applications/Lantern.app"
