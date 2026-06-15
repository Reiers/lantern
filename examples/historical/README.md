# Historical phase demos

These are the standalone end-to-end integration runners written during
Lantern's initial phase-by-phase build-up. Each `phaseN/main.go` was a
one-shot demo that proved a specific capability against live mainnet
(usually in dry-run mode) before that capability was folded into the real
`cmd/lantern` daemon and covered by the regular test suite.

They are kept for historical reference and as worked examples of driving
the lower-level packages directly. They are **not** part of the product
build: `cmd/lantern` is the daemon, and live validation now happens via the
unit/integration tests plus the calibration `cc-smoke` deployment.

| Demo | Proved |
|------|--------|
| `phase1` | Download a slice of recent mainnet state via Glif, validate headers locally, build a TrustedRoot, cross-check. |
| `phase2` | Talk to `gateway.lantern.reiers.io` for the finalized head and walk back. |
| `phase5` | Serve a Curio-shaped query stream (StateMinerInfo / Power / ProvingDeadline / AvailableBalance / MarketStorageDeal) end-to-end. |
| `phase6` | Message flow + randomness + persistent header store + libp2p mempool against live mainnet (dry-run). |
| `phase7` | VM shell + gas estimation + miner-base-info + block-template assembly + paych voucher round-trip (dry-run). |

> These were moved here from `cmd/lantern-phase*` on 2026-06-15 to keep the
> `cmd/` build surface limited to the shipping binaries
> (`lantern`, `lantern-gateway`, `lantern-f3-anchor`, `lantern-lotus-compat-test`).
