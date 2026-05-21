//
// LanternApp.swift
//
// Menu-bar app entry point. Per INSTALLER-SPEC.md §6b (V1.2 GA):
//
//   - Menu-bar icon shows the chain head epoch + peer count at a
//     glance
//   - Click → popover with `lantern info`-shaped status
//   - Right-click → controls (start/stop/restart/logs/quit)
//   - Quorum status indicator: ● green (all sources agree)
//                              ● amber (N−1 agree)
//                              ● red (quorum lost)
//
// Talks to the local Lantern daemon via JSON-RPC at 127.0.0.1:1234.
// The token is read from ~/.lantern/token. The popover refreshes
// every 5 seconds while open.
//
// MVP scope: emoji indicator + epoch + peers + a couple of buttons.
// Settings pane (quorum sources, JWT scopes, beacon toggle) is
// V1.2.1.
//

import SwiftUI
import AppKit

@main
struct LanternApp: App {
    @StateObject private var status = DaemonStatus()

    var body: some Scene {
        // MenuBarExtra is the SwiftUI native way to live in the menu bar
        // on macOS 13+. The label rerenders every time `status` changes.
        MenuBarExtra {
            MenuBarPopoverView(status: status)
        } label: {
            MenuBarLabelView(status: status)
        }
        .menuBarExtraStyle(.window)
    }
}

// MARK: - Menu bar label

struct MenuBarLabelView: View {
    @ObservedObject var status: DaemonStatus

    var body: some View {
        HStack(spacing: 4) {
            Image(systemName: "lantern.fill")
                .symbolRenderingMode(.palette)
                .foregroundStyle(.primary, status.indicatorColor)
            if status.isRunning, let head = status.headEpoch {
                Text("\(head)")
                    .font(.system(size: 11, design: .monospaced))
            }
        }
    }
}

// MARK: - Popover

struct MenuBarPopoverView: View {
    @ObservedObject var status: DaemonStatus

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Image(systemName: "lantern.fill")
                    .font(.title2)
                    .foregroundStyle(.tint)
                Text("Lantern")
                    .font(.headline)
                Spacer()
                QuorumIndicator(state: status.quorumState)
            }

            Divider()

            if status.isRunning {
                statusBlock
            } else {
                stoppedBlock
            }

            Divider()

            HStack(spacing: 6) {
                Button("Refresh") { status.refresh() }
                Button(status.isRunning ? "Stop" : "Start") {
                    if status.isRunning { status.stopDaemon() } else { status.startDaemon() }
                }
                Spacer()
                Button("Quit") { NSApp.terminate(nil) }
            }
        }
        .padding(14)
        .frame(width: 280)
        .onAppear { status.startPolling() }
        .onDisappear { status.stopPolling() }
    }

    @ViewBuilder private var statusBlock: some View {
        VStack(alignment: .leading, spacing: 6) {
            row("Head epoch", "\(status.headEpoch.map(String.init) ?? "–")")
            row("Peers", "\(status.peerCount ?? 0)")
            row("Network", status.network ?? "–")
            if let anchor = status.bootstrapAnchor {
                row("F3 anchor", "instance \(anchor.instance)")
                row("Anchored at", anchor.capturedAt)
            }
        }
        .font(.system(.body, design: .monospaced))
    }

    @ViewBuilder private var stoppedBlock: some View {
        VStack(alignment: .leading, spacing: 6) {
            Label("Daemon not running", systemImage: "exclamationmark.triangle")
                .foregroundStyle(.orange)
            Text("Start with: lantern daemon")
                .font(.system(.callout, design: .monospaced))
                .foregroundStyle(.secondary)
        }
    }

    private func row(_ k: String, _ v: String) -> some View {
        HStack {
            Text(k).foregroundStyle(.secondary)
            Spacer()
            Text(v)
        }
    }
}

// MARK: - Quorum indicator

struct QuorumIndicator: View {
    let state: QuorumState
    var body: some View {
        HStack(spacing: 4) {
            Circle()
                .fill(state.color)
                .frame(width: 8, height: 8)
            Text(state.label)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }
}

enum QuorumState {
    case green       // all sources agree
    case amber       // N−1 agree
    case red         // quorum lost
    case unknown     // not yet probed

    var color: Color {
        switch self {
        case .green: return .green
        case .amber: return .orange
        case .red: return .red
        case .unknown: return .gray
        }
    }
    var label: String {
        switch self {
        case .green: return "Quorum OK"
        case .amber: return "Quorum at edge"
        case .red: return "Quorum lost"
        case .unknown: return "Quorum: unknown"
        }
    }
}
