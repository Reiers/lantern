# Lantern v1.7.24 — complete `eth_getTransactionReceipt` fields

**Update if:** you use `cast receipt`, ethers/web3 receipt parsing, or anything
that reads the transaction sender, recipient, or gas price off a receipt, and
you saw deserialize errors like `missing field 'from'` or empty cost/fee
fields. If you only read `status` + `blockNumber` off receipts, this update is
optional.

## What changed

Lantern's locally-served `eth_getTransactionReceipt` (the zero-Glif write path)
returned a minimal receipt: `status`, `blockNumber`, `gasUsed`, hashes, and
empty logs. It **omitted the standard `from`, `to`, `contractAddress`, and
`effectiveGasPrice` fields**.

That tripped strict eth clients — `cast receipt` and ethers both fail to
deserialize a receipt with no `from` — and made it impossible to compute a
transaction's fee (`effectiveGasPrice × gasUsed`) from a locally-served receipt.

This release adds those fields:

- **`from`** / **`to`** — taken from the locally-tracked sent-tx record (the
  same record that already powers `eth_getTransactionByHash`). `to` is `null`
  for contract-creation transactions.
- **`contractAddress`** — present (currently `null`; Lantern V1 does not yet
  reconstruct the created-contract address). Having the key present is enough
  for strict clients to deserialize.
- **`effectiveGasPrice`** — the transaction's `maxFeePerGas`, a conservative
  stand-in. Lantern V1 does not reconstruct the per-tipset base fee at receipt
  time, so this is an upper bound rather than the exact effective price; clients
  needing the precise value can still fall back to the bridge.

## Behavior details

- Only the **locally-served** path (transactions this node broadcast) gains the
  fields. Bridge-forwarded receipts are unchanged.
- No protocol or storage changes. Drop-in binary replacement.
- `status`, `blockNumber`, `gasUsed`, `transactionHash`, `logs`, `logsBloom`,
  `type` are unchanged.

## Notes

- Version string is a clean semver (`v1.7.24`) with no internal-branch suffix.
- Build is CGO-free; no new external dependencies.
