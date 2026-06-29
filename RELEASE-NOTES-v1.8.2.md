# Lantern v1.8.2 — bridge-off read-path coverage for stock Curio

**Update if:** you run **upstream Curio** (not curio-core) behind Lantern in a
zero/low-Glif configuration and see provider-lookup or settlement tasks fail
with `FEVM method requires --vm-bridge-rpc`. curio-core users were never
affected (see below). No protocol or wire changes, no new external dependencies.

## The bug (#69)

A field report: a node running stock upstream Curio behind Lantern, bridge-off,
failed its `Settle` task on a loop:

```
ERROR harmonytask  Do() returned error  {"type": "Settle",
  "error": "failed to get provider: chain: FEVM method requires --vm-bridge-rpc pointing at a Forest/Lotus node"}
```

The Settle task makes two **view** `eth_call`s against `ServiceProviderRegistry`
(`isRegisteredProvider`, `getProviderByAddress`). Both local-missed in Lantern,
fell back to the VM bridge, and with no bridge configured returned the error
above. (This is unrelated to the v1.8.1 sync fixes #53/#68 — those were about a
node falling *behind* head; this is a read-path coverage gap that shows up even
on a fully-synced node.)

## Why it worked under curio-core but not stock Curio

Lantern's FEVM state prefetcher warms a set of contract storage subtrees into
the local cache on every head advance, so subsequent `eth_call`s against those
contracts are served locally instead of falling back to the bridge. **It only
warmed the addresses its consumer told it to** (`Config.FEVMPrefetchAddrs`).

- **curio-core** injects that set itself (`cmd/curio-core/fevm_prefetch.go`):
  PDPVerifier, FWSS, ServiceProviderRegistry, and USDFC. That's why the
  zero-Glif read path — including the registry reads the Settle task needs —
  has always worked end-to-end under curio-core.
- **Stock upstream Curio** has no such wiring and injects nothing. With an empty
  address list the prefetcher never even started, so *every* contract `eth_call`
  local-missed and fell to the bridge. Lantern had no built-in knowledge of the
  well-known contract addresses, so it couldn't self-warm to cover the gap.

## The fix

Lantern now ships its own **built-in per-network warm-set** of the well-known
Filecoin PDP contract proxies — PDPVerifier, FilecoinWarmStorageService (FWSS),
ServiceProviderRegistry, and USDFC — for both mainnet and calibration. At
startup it:

1. resolves the built-in set for the node's network,
2. merges it with any addresses the consumer supplied (consumer entries win
   ordering; de-duped by canonical, case-insensitive form; unparseable consumer
   entries are kept verbatim, never silently dropped), and
3. starts the prefetcher whenever the merged set is non-empty.

So the zero-Glif read path now works for **any** Lotus-API consumer behind
Lantern — including stock upstream Curio — with **no extra wiring and no
`--vm-bridge-rpc`**. curio-core's injected set continues to work unchanged and
remains extensible. The built-in addresses are kept in sync with
`filecoin-project/curio` `pdp/contract/addresses.go`.

## On `--vm-bridge-rpc` as a workaround

You *can* set `--vm-bridge-rpc https://api.node.glif.io/rpc/v1` to mask this —
the failing reads would then route through Glif — but with v1.8.2 you shouldn't
need it. As with the v1.8.1 sync fixes, defaulting the bridge on would route
every node silently back through Glif and re-mask exactly the kind of bug this
release fixes. It stays optional by design.

## Upgrade notes

- No configuration changes required. Drop in the new binary and restart.
- No new external dependencies.

## Verification

CGO-free build, `go vet`, `gofmt`, and the hermetic test suite (`-short`,
`LANTERN_OFFLINE=1`) are green across the module. New unit tests cover
per-network warm-set resolution, calibration name aliases, fresh-copy
isolation, and the built-in/consumer merge + de-dup (case-insensitive overlap,
empty inputs, unparseable-verbatim). The live mainnet node was not touched
during development.
