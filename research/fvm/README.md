# research/fvm — pure-Go Filecoin FVM prototype

Research-tier code. **Not** part of the shipping `lantern` binary. Owns
its own `go.mod` so wazero + go-multihash + refmt do NOT enter the main
module's dependency graph (per #24, "keep Lantern small"). Tracks the
Stage C sub-issues of #89.

## Hard safety line

**This is not a consensus-safe FVM.** It runs real Filecoin v17 builtin
actors and executes their code end-to-end, but:

- Gas is not metered to ref-fvm fidelity (mechanism only, tracked in #128).
- `verify_post`, `verify_seal`, and the aggregate/replica verify family
  are ABI-correct stubs (tracked in #130 Tier 2, shares crypto core with
  #88).
- `verify_signature` and `verify_consensus_fault` share the same crypto
  core dependency; the default signature verifier is `RejectAllVerifier`
  so the module cannot report a consensus fault or a valid signature
  until a real BLS12-381 verifier is plugged in.

**Do NOT wire this into block validation** until every Tier 2 syscall
lands with vector-matched fidelity. A byte-wrong FVM forks the node off
the network.

## What runs today

- **C1 read + write actor execution** — the real v17 `account` actor's
  `Constructor` and `PubkeyAddress` methods execute end-to-end under
  wazero with byte-exact ABI. Constructor's written state root is
  byte-equal to the canonical account State CID.
- **C2 gas transformer** — pure-Go WASM binary rewriter that injects
  charge + out-of-gas checks and re-encodes the module. Mechanism
  proven, fidelity gated on #128.
- **C3 send recursion** — real call-manager: shared `StateTree` with
  transactional `Snapshot`/`Restore`, address resolution (init HAMT
  semantics), value transfer, revert on non-zero exit, read-only
  propagation. A synthetic caller-actor WASM (hand-emitted, 137 bytes)
  proves nested-from-WASM send through `send.send` → nested frame →
  target actor return propagation.
- **C4 Tier 1** — `compute_unsealed_sector_cid` (stack-based SHA254
  merkle over pieces; matches Filecoin's canonical zero-tree vectors
  through level 5) and `verify_consensus_fault` (CBOR block-header
  parser + three-fault detection with pluggable `SignatureVerifier`).

## Layout

- `fvmkernel/` — the kernel: syscall dispatch, state tree, machine
  orchestrator, CBOR parser, CommD + fault detection.
  - `kernel.go` — 38 hostcalls across 11 modules; 20+ implemented for
    real.
  - `statetree.go` — actor tree + address map + 128-bit `TokenAmount`
    + transactional snapshot/restore.
  - `machine.go` — call-manager: `Send(from, to, method, params, value)`
    with frame semantics + rollback.
  - `commd.go` — pure-Go CommD (unsealed sector CID) computation.
  - `consensus_fault.go` — header parsing + fault detection.
  - `cbor.go` — minimal DagCBOR decoder for the shapes the crypto
    syscalls need.
  - `testactor_caller.go` — synthetic caller-actor WASM emitter.
  - `abi.go` + `hostcalls_ext.go` — ref-fvm-lifted syscall struct
    layouts + additional module registrations.
  - `executor.go` — the invocation driver.
- `gasmeter/` — the WASM binary rewriter (C2 mechanism).
- `cmd/` — CLI helpers:
  - `fvmrun` — run one method invocation of an actor from the shell.
  - `extract` — decode a builtin-actors CAR into per-actor WASMs.
  - `spike`, `smartstubs` — the original probe programs.

## Running

```sh
go test ./fvmkernel/ ./gasmeter/
go run ./cmd/fvmrun fvmkernel/testdata/account.wasm
```

To regenerate the actor WASM set from a public bundle:

```sh
curl -L -o mainnet.car https://github.com/filecoin-project/builtin-actors/releases/download/v17.0.0/builtin-actors-mainnet.car
go run ./cmd/extract mainnet.car
```

## Dependency firewall

The main `lantern` module MUST NOT transitively depend on wazero /
go-multihash / refmt. `.github/workflows/ci.yml` enforces this: the
main-module `go list -deps ./...` output is scanned and CI fails if
any research-only dependency leaks in.

If the FVM ever becomes consensus-safe (all Stage C + vector-matching
passes), promotion into the main module is an explicit, reviewable
step — not an accident of import.
