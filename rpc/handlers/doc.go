// Package handlers contains the implementation of api.FullNode that
// Lantern's RPC server registers. The handlers route reads through
// state/accessor and writes through wallet + a gossipsub publisher.
//
// Each method maps 1:1 to an entry in CURIO-RPC-SURFACE.md, marked with a
// `// Tier N` comment so reviewers can find what's done vs deferred.
//
// Tier 1 (1-20): implemented against state/accessor + trustedroot.
// Tier 2 (21-30): implemented where feasible, otherwise stubbed with a
//
//	clear `xerrors.New("not implemented in Lantern V1 — ...")`.
//
// Tier 3 (31-71): the long tail; partially implemented or stubbed.
// Tier 4: deferred (StateCall, MinerCreateBlock — need a VM).
//
// All stub methods return an error matching the Lotus convention
// `xerrors.New("not implemented in Lantern V1 — requires VM, see Phase 5")`
// so Curio can detect the gap cleanly.
package handlers
