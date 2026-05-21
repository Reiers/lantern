# HAMT mainnet fixtures

Each `*.block` file is a single IPLD-DAG-CBOR block fetched from Glif's
`Filecoin.ChainReadObj` at the time pinned in `meta.json`. The file name is
the CID v1 string.

These fixtures let `state/hamt` tests run a real-mainnet HAMT walk against a
deterministic snapshot, with no network access at test time.

To regenerate (network required):

```
cd state/hamt && go run ./testdata/gen
```

The generator walks from the current Glif chain head through the state root,
into the actors HAMT, and down to a small set of known actors (f00, f04, f099,
f01000). It writes each visited block to this directory.
