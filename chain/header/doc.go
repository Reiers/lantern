// Package header implements Filecoin block-header chain validation. It is the
// Lantern equivalent of the header-only subset of Lotus' chain/sync.go: parent
// linkage, BLS aggregate signature verification over the block, beacon entry
// referencing (delegated to chain/beacon), election-proof signature checks,
// and tipset construction.
//
// What this package does NOT do (intentionally): execute messages, validate
// state transitions, or instantiate a VM. Lantern is a light client; message
// execution lives in full nodes (Forest, Lotus, the future pure-Go FVM).
//
// See docs/design/MODULES.md and docs/design/TRUSTED-ROOT.md for context.
package header
