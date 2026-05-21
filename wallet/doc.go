// Package wallet provides an encrypted-at-rest local keystore plus a
// signing facade for the three Filecoin signature types Lantern supports:
//   - BLS (G1 pubkey, G2 sig) via crypto/sigs/bls (pure-Go gnark backend)
//   - secp256k1 via crypto/sigs/secp (lifted from Lotus, pure-Go)
//   - delegated (Ethereum-shaped f4 addresses) via crypto/sigs/delegated
//     (lifted from Lotus, pure-Go)
//
// Keystore on-disk format is compatible with Lotus' lib/keystore (one JSON
// file per key, AES-GCM-wrapped with a passphrase-derived key) so users can
// import/export to and from a Lotus install.
//
// Subpackages:
//   - wallet/keystore — disk-backed encrypted key store
//   - wallet/mnemonic — BIP-39 mnemonic <-> seed
//
// The top-level wallet package wires these together behind a small
// goroutine-safe Wallet type that the RPC handlers (WalletNew, WalletSign,
// WalletList, ...) call into.
//
// No CGo. No filecoin-ffi.
package wallet
