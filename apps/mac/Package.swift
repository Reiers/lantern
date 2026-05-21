// swift-tools-version:5.9
//
// Lantern Mac menu-bar app — MVP per INSTALLER-SPEC.md §6b.
//
// Builds with SwiftPM only (no Xcode project). Run:
//
//   cd apps/mac
//   swift build -c release
//
// The executable lives at .build/release/Lantern. Scripts/build_and_sign.sh
// wraps it into a proper .app bundle suitable for /Applications.
//
// The app is intentionally tiny: a menu-bar icon, a popover showing
// chain head epoch + peer count + quorum status, and right-click
// controls for the local daemon. Auto-update via Sparkle is deferred
// to V1.3.

import PackageDescription

let package = Package(
    name: "Lantern",
    platforms: [
        .macOS(.v13),
    ],
    products: [
        .executable(name: "Lantern", targets: ["Lantern"]),
    ],
    dependencies: [],
    targets: [
        .executableTarget(
            name: "Lantern",
            path: "Sources/Lantern"
        ),
    ]
)
