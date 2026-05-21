// Package bridge defines a soft trust point that Lantern operators can
// opt into when they need VM execution beyond the native Send path.
//
// Why this exists
// ---------------
//
// Lantern's vm package is a gas-accurate execution shell, not a full
// FVM. It handles Send (method 0) end-to-end and gas-accounts every
// other builtin actor method, but it does not execute method bodies.
// That's fine for Curio's hot path (state reads, gas estimation) but
// breaks two flows:
//
//   1. Filecoin.StateCall for non-Send messages: callers (most notably
//      Curio's storage-market PSD verification and any eth_call against
//      an EVM contract) need a real receipt with Return bytes.
//   2. Filecoin.MinerCreateBlock with AllowBlockSubmit=true: the
//      resulting block header needs the correct post-execution
//      ParentStateRoot. Lantern's shell cannot compute that.
//
// The bridge fixes both by delegating to an upstream trusted full node
// (Forest or Lotus) for the narrow operation "compute the
// post-execution state root + receipts for this base + these messages."
//
// Trust model
// -----------
//
// The bridge IS a trust point. When a Lantern operator wires a bridge:
//
//   - For StateCall, the upstream node's reply IS the answer. Lantern
//     does not re-verify it (we have no way to).
//   - For MinerCreateBlock + AllowBlockSubmit=true, the upstream's
//     stateRoot is what we sign into the block header.
//
// What Lantern still verifies independently when a bridge is configured:
//
//   - Header chain validity (BLS sigs, parent linkage)
//   - F3 finality certificates
//   - DRAND beacons
//   - Every IPLD block CID we read for state queries
//
// The bridge is NOT used for read-path methods (StateGetActor,
// StateMinerInfo, etc.) — those continue to verify via the HAMT walker.
// The bridge is bounded to message execution, and operators control
// when to enable it.
//
// Operators who want the strict "no trusted third party" guarantee
// simply leave the bridge unconfigured; StateCall for non-Send returns
// SysErrInvalidReceiver and MinerCreateBlock stays gated.
//
// Operators who run their own Forest/Lotus as a sibling can wire that
// node as the bridge. The trust point is their own infrastructure, not
// a public RPC provider — fully consistent with the Lantern thesis.
//
// See TRUST-MODEL.md for the comprehensive walkthrough.

package bridge
