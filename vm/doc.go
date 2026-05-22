// Package vm implements a minimal, pure-Go execution shell sufficient
// for Curio's read-only and gas-estimation calls against Lantern.
//
// # Scope and non-scope
//
// Filecoin's reference FVM (ref-fvm, Rust) executes WASM-compiled actor
// code with a full gas-metered runtime, syscalls, and IPLD store. Lantern
// cannot import ref-fvm (CGo) and there is no pure-Go FVM in the
// ecosystem today. So this package is deliberately **not** a full VM.
//
// What this package DOES:
//
//   - Decode and dispatch a Filecoin Message against the built-in actor
//     method tables shipped in go-state-types (v17/v18).
//   - Execute Account → Account `Send` (method 0) end-to-end: balance
//     check, value transfer, gas accounting. This is the only on-chain
//     write path we model with full fidelity.
//   - For every other built-in actor method, run "gas accounting only":
//     return a synthetic `MessageReceipt` with `ExitCode=0`, `Return=nil`,
//     and a `GasUsed` value derived from the message size, method
//     parameters and the canonical Filecoin gas schedule. **State is
//     not mutated.** This matches what Lotus' `GasEstimateGasLimit`
//     actually needs: it binary-searches for a gas value that succeeds
//     without caring about the receipt body.
//   - For user-deployed actors (EVM, native FVM actors v3+) and any
//     unknown method, return `ExitCode=SysErrUnsupportedMethod`.
//
// What this package DOES NOT do:
//
//   - It does not execute actor logic (no PreCommit/ProveCommit math, no
//     PoSt verification, no FIL+ allocation accounting).
//   - It does not produce a new state root. `ApplyMessage` returns the
//     input state root unchanged (modulo a synthetic per-message receipt
//     CID we accumulate in an AMT for block-template assembly).
//   - It does not run cron, vesting, reward issuance, or any of the
//     per-block "implicit" messages a full Filecoin block would.
//
// Why this is enough for Phase 7:
//
//   - StateCall: callers only need ExitCode + GasUsed + ReturnValue for
//     gas estimation and dry-run validation. For Curio's writes
//     (PreCommit, ProveCommit, PoSt, etc.) the network's full nodes
//     execute the message for real; Lantern only has to produce a
//     plausible gas envelope so Curio can sign+broadcast.
//   - GasEstimateMessageGas: binary search for the gas limit becomes
//     trivial because our VM never charges more than a fixed per-method
//     ceiling. We compose this with the real on-chain `BaseFee` and a
//     mempool-derived premium percentile.
//   - MinerCreateBlock (Phase 7 Part C): in dry-run mode the block's
//     `ParentStateRoot` field is taken verbatim from the parent tipset
//     (i.e. "no change"). We document this clearly: Lantern cannot
//     produce a *publishable* block, but it can produce a syntactically
//     valid `*types.BlockMsg` Curio's WinPoSt task can hand to its
//     normal signing pipeline.
//
// Provenance
//
//   - Gas constants and per-message costs come from
//     go-state-types/builtin/v18 (in particular the price-list lifted
//     from Lotus 1.36's `chain/vm/gas_v15.go`).
//   - Method dispatch uses the public `Methods` map shipped in
//     `go-state-types/builtin/v{N}/<actor>/methods.go`.
//   - No code is lifted from `lotus/chain/vm` directly.
//
// All limitations are documented in PHASE7-BLOCKERS.md.
package vm
