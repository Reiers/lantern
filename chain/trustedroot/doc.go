// Package trustedroot produces and persists Lantern's TrustedRoot: a small
// in-memory tuple (Epoch, TipSetKey, TipSetCID, StateRoot,
// ParentMessageReceipts, ParentWeight, F3Instance, F3Cert, AncestorRoots) that
// the rest of the node treats as ground truth for "what the chain is right
// now."
//
// The Build pipeline:
//
//  1. Start from a hard-coded genesis CID and the F3 network manifest.
//  2. Stream block headers from a HeaderSource, validate each via
//     chain/header, build canonical tipsets.
//  3. Stream F3 finality certificates from an F3CertSource, validate via
//     chain/f3.
//  4. Consolidate into a TrustedRoot at the highest F3-finalized epoch (or
//     the head minus a safety buffer, pre-F3).
//  5. Persist to BadgerDB so subsequent boots resume in seconds.
//
// See docs/design/TRUSTED-ROOT.md for the full specification.
package trustedroot
