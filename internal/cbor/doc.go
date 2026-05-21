// Package cbor wraps github.com/whyrusleeping/cbor-gen utilities used by
// generated CBOR codecs across Lantern. Kept thin: most cbor-gen code lives in
// the package whose types it serializes (chain/types/cbor_gen.go, etc.).
package cbor
