// Package api declares the Lotus-compatible JSON-RPC interface and its
// shared result types.
//
// Lantern targets `CurioChainRPC` (Curio's `-tags forest` interface). The
// methods listed here are the 71 entries in CURIO-RPC-SURFACE.md. Each is
// declared as a method on the FullNode interface so the go-jsonrpc
// dispatcher can bind handlers under the `Filecoin.` namespace, matching
// what every Lotus / Curio / sptool client expects.
//
// Types are mostly re-exported from go-state-types or chain/types so we
// don't drift from on-wire shape.
package api
