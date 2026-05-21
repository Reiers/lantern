//
// DaemonStatus.swift
//
// Lightweight client for the local Lantern JSON-RPC daemon. Polls
// every 5 seconds while the popover is open. Reads:
//
//   - Filecoin.ChainHead          → headEpoch
//   - Filecoin.NetPeers           → peerCount
//   - Filecoin.Version            → network
//   - ~/.lantern/bootstrap-anchor.json → anchor metadata + quorum
//
// All fields are @Published so SwiftUI rerenders on update. Failures
// (no token, no daemon) downgrade isRunning to false instead of
// throwing — the popover handles the stopped state explicitly.
//

import Foundation
import SwiftUI

@MainActor
final class DaemonStatus: ObservableObject {
    @Published var isRunning: Bool = false
    @Published var headEpoch: Int? = nil
    @Published var peerCount: Int? = nil
    @Published var network: String? = nil
    @Published var bootstrapAnchor: BootstrapAnchor? = nil
    @Published var quorumState: QuorumState = .unknown

    var indicatorColor: Color {
        if !isRunning { return .gray }
        return quorumState.color
    }

    private var pollTimer: Timer?
    private let endpoint = URL(string: "http://127.0.0.1:1234/rpc/v1")!

    func startPolling() {
        refresh()
        pollTimer?.invalidate()
        pollTimer = Timer.scheduledTimer(withTimeInterval: 5, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }
    }

    func stopPolling() {
        pollTimer?.invalidate()
        pollTimer = nil
    }

    func refresh() {
        Task { @MainActor in
            await self.fetchEverything()
        }
    }

    private func fetchEverything() async {
        let token = (try? String(contentsOf: lanternHome().appendingPathComponent("token"))
            .trimmingCharacters(in: .whitespacesAndNewlines)) ?? ""

        // Daemon probe via /healthz.
        var healthOK = false
        if let (data, response) = try? await URLSession.shared.data(from: URL(string: "http://127.0.0.1:1234/healthz")!),
           let http = response as? HTTPURLResponse, http.statusCode == 200, !data.isEmpty {
            healthOK = true
        }
        self.isRunning = healthOK
        guard healthOK else {
            self.headEpoch = nil
            self.peerCount = nil
            return
        }

        if let head: Int = await rpcInt(method: "Filecoin.ChainHead", token: token, path: "Height") {
            self.headEpoch = head
        }
        if let peers: Int = await rpcArrayLen(method: "Filecoin.NetPeers", token: token) {
            self.peerCount = peers
        }
        if let ver: String = await rpcString(method: "Filecoin.Version", token: token, path: "Version") {
            self.network = ver
        }
        self.bootstrapAnchor = BootstrapAnchor.load()

        // Quorum state: best-effort heuristic from the anchor's recency.
        // V1.2.1 will add a real `Filecoin.NodeStatus`-shaped endpoint
        // that exposes the live quorum tally; for the MVP we mark
        // green if a fresh anchor exists, amber if it's >24h old,
        // red if missing.
        self.quorumState = quorumFromAnchor(self.bootstrapAnchor)
    }

    func startDaemon() {
        Task {
            _ = try? await runLantern(args: ["service", "start"])
            refresh()
        }
    }

    func stopDaemon() {
        Task {
            _ = try? await runLantern(args: ["service", "stop"])
            refresh()
        }
    }

    // MARK: - low-level helpers

    private func rpcInt(method: String, token: String, path: String) async -> Int? {
        guard let result = await rpc(method: method, params: [], token: token) else { return nil }
        if let n = result[path] as? Int { return n }
        if let n = (result[path] as? NSNumber)?.intValue { return n }
        return nil
    }

    private func rpcString(method: String, token: String, path: String) async -> String? {
        guard let result = await rpc(method: method, params: [], token: token) else { return nil }
        if let s = result[path] as? String { return s }
        return nil
    }

    private func rpcArrayLen(method: String, token: String) async -> Int? {
        // Some RPCs return arrays at top level; we accept either
        // `result: [...]` or `result: { Peers: [...] }`.
        guard let raw = await rpcRaw(method: method, params: [], token: token) else { return nil }
        if let arr = raw as? [Any] { return arr.count }
        if let dict = raw as? [String: Any], let arr = dict["Peers"] as? [Any] { return arr.count }
        return nil
    }

    private func rpcRaw(method: String, params: [Any], token: String) async -> Any? {
        var req = URLRequest(url: endpoint)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        if !token.isEmpty { req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        let body: [String: Any] = [
            "jsonrpc": "2.0",
            "id": 1,
            "method": method,
            "params": params,
        ]
        guard let data = try? JSONSerialization.data(withJSONObject: body) else { return nil }
        req.httpBody = data
        guard let (resp, response) = try? await URLSession.shared.data(for: req) else { return nil }
        guard let http = response as? HTTPURLResponse, http.statusCode == 200 else { return nil }
        guard let json = try? JSONSerialization.jsonObject(with: resp) as? [String: Any] else { return nil }
        return json["result"]
    }

    private func rpc(method: String, params: [Any], token: String) async -> [String: Any]? {
        return await rpcRaw(method: method, params: params, token: token) as? [String: Any]
    }
}

// MARK: - bootstrap-anchor.json

struct BootstrapAnchor: Codable {
    let instance: Int
    let epoch: Int
    let tipsetKey: [String]
    let stateRoot: String
    let capturedAt: String
    let network: String

    static func load() -> BootstrapAnchor? {
        let url = lanternHome().appendingPathComponent("bootstrap-anchor.json")
        guard let data = try? Data(contentsOf: url) else { return nil }
        let dec = JSONDecoder()
        return try? dec.decode(BootstrapAnchor.self, from: data)
    }
}

func lanternHome() -> URL {
    if let env = ProcessInfo.processInfo.environment["LANTERN_HOME"], !env.isEmpty {
        return URL(fileURLWithPath: env)
    }
    return FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".lantern")
}

func quorumFromAnchor(_ a: BootstrapAnchor?) -> QuorumState {
    guard let a = a else { return .red }
    // ISO 8601 like 2026-05-21T20:17:44.794942Z
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let captured = f.date(from: a.capturedAt) ?? Date.distantPast
    let age = Date().timeIntervalSince(captured)
    if age < 24 * 3600 { return .green }
    if age < 7 * 24 * 3600 { return .amber }
    return .red
}

// MARK: - shelling out to `lantern`

@discardableResult
func runLantern(args: [String]) async throws -> String {
    let task = Process()
    task.executableURL = lanternBinaryURL()
    task.arguments = args
    let pipe = Pipe()
    task.standardOutput = pipe
    task.standardError = pipe
    try task.run()
    task.waitUntilExit()
    let data = try pipe.fileHandleForReading.readToEnd() ?? Data()
    return String(data: data, encoding: .utf8) ?? ""
}

func lanternBinaryURL() -> URL {
    // 1. ~/.lantern/lantern (canonical install location)
    let local = lanternHome().appendingPathComponent("lantern")
    if FileManager.default.isExecutableFile(atPath: local.path) {
        return local
    }
    // 2. /usr/local/bin/lantern (the install.sh symlink)
    let sym = URL(fileURLWithPath: "/usr/local/bin/lantern")
    if FileManager.default.isExecutableFile(atPath: sym.path) {
        return sym
    }
    // 3. Fallback: PATH lookup via /usr/bin/env.
    return URL(fileURLWithPath: "/usr/bin/env")
}
