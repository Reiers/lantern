# F3 wiring — notes for Phase 6

## What landed today (Phase 5 wrap-up, commit 56c70c1)

- `chain/f3/anchor/`: persistent trust-anchor structure + canonical JSON encoding
- `cmd/lantern-f3-anchor`: capture tool that pulls F3GetF3PowerTable + F3GetLatestCertificate from Forest/Lotus and writes the anchor
- `anchor_mainnet.json`: live anchor at GPBFT instance 466,453, 690 entries, ~100 KB
- Embedded via go:embed → `anchor.Embedded("mainnet")` returns a ready-to-use Anchor
- Tests: load + materialise + JSON roundtrip, all green

## What Phase 6 should do with it

1. Build `chain/f3/subscriber`:
   - Construct initial PowerTable from `anchor.Embedded(network).PowerTable()`
   - Pull certificates starting at `anchor.Instance` from Forest via `Filecoin.F3GetCertificate(N)` (or via the public gateway once it proxies F3 methods — currently it doesn't, gateway proxies only ChainHead + ChainReadObj)
   - For each cert: verify with current PowerTable (use the existing `chain/f3.VerifySingleCert`), apply power diff, advance
   - Persist `(instance, powerTable, finalizedTipSet)` to Badger so we don't re-verify from anchor every restart

2. Wire into `chain/trustedroot.Build`:
   - Accept an `F3CertSource` (interface already exists in chain/trustedroot/trustedroot.go:64)
   - On Build, pull certs from anchor.Instance → latest, populate `TrustedRoot.F3Cert` and `F3Instance`
   - The Build pipeline already has a slot for this (the manifest unmarshal in chain/f3/f3.go works; just the cert source was missing)

3. Re-pin schedule:
   - Anchor should be re-captured monthly to keep the cert-walk depth manageable
   - Long-term: ship a `lantern anchor refresh` subcommand that pulls a fresh anchor
   - For now, the captured anchor is at instance 466,453; current is around 467,000+, so ~600 instances to walk. That's seconds.

## Gateway repointing (deferred from today)

Today's pragmatic call: gateway stays on Glif backend, lex Forest is for F3
anchor capture only. Reasoning:

- Glif proxy works at 100% with the new retry hardening (commit 4d3a845)
- Repointing requires either exposing Forest RPC publicly on lex (firewall +
  auth surface) or running a reverse SSH tunnel (lex → Hetzner). Both are
  fragile compared to "trust Glif for proxy, verify everything cryptographically"
- Real long-term answer is a dedicated Lantern gateway box with Forest local,
  Bitswap exposed, and a real F3 cert API endpoint. That's Phase 8 (infra).

Phase 6 can pull F3 certs directly from gateway.lantern.reiers.io if we extend
the gateway to proxy Filecoin.F3GetCertificate / F3GetLatestCertificate, OR
Phase 6 can connect directly to the public lex Forest if we expose 2345
publicly with auth, OR Phase 6 can run its own local F3 cert client over
libp2p (the GoF3 library supports this natively).

Recommend: Phase 6 adds F3GetCertificate to gateway proxy whitelist + uses
that. Smallest change, cleanest interface, no fragile tunnels.
