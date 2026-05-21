# README updates to apply after Phase 10 lands

Hold queue for README edits. Apply once Phase 10 lands cleanly and we cut V1.1.

## 1. Correct the "~1 GB" footprint claim

Today's README says:
> A Filecoin light node that boots in minutes with ~1 GB instead of 76 GB.

The truth is better:
- Cold-start download: **~35 MB** (the binary itself)
- Steady-state disk: **~150 MB** (binary + header store + F3 cert subscriber state + wallet)
- Beacon operators (opt-in `lantern beacon` mode): up to 5 GB configurable

Update the headline + the architecture diagram caption. Keep the "~1 GB" framing only where it refers to V1.2's optional bootstrap bundle (if we ship one).

Suggested new headline:
> **A Filecoin light node that boots in minutes with ~150 MB of disk instead of 76 GB.**

## 2. Add F3 participation clarification

Make clear what Lantern does with F3 and what it does not. Add a short section, probably under or near the "Do I need to run a Filecoin node?" block:

> ### Does Lantern participate in F3?
>
> Lantern is **F3-aware but not F3-participating**.
>
> - ✅ **Verifies F3 finality certificates** — every cert's BLS aggregate signature is checked locally against the evolving power table. Lantern's trust anchor is an F3-finalized tipset.
> - ✅ **Uses F3 fast-finality** — when you query the chain via Lantern, you get the F3-finalized view, not the EC-finalized view 7.5 hours behind.
> - ❌ **Does not vote in GPBFT** — F3 participation requires on-chain storage power and is for Storage Providers only. Light clients have no power and cannot vote.
> - ❌ **Does not publish finality certificates** — only GPBFT committee members publish certs.
>
> If you're a Storage Provider running Curio + Lantern, your existing Curio still participates in F3 the normal way. Lantern verifies the certs it produces; it doesn't replace the participation path.

## 3. Update status matrix

After Phase 10 ships:
- V1.1 row → ✅ Shipped (libp2p stats, Bitswap primary, beacon subcommand)
- V1.2 row → 🔄 In progress (installer, bootstrap quorum, Mac app)
- Method coverage → still 71/71, but note that NetPeers / NetBandwidthStats / NetAutoNatStatus now return real data instead of stubs

## 4. Update the architecture diagram

Change the fetch hierarchy footer from:
```
local cache → Bitswap → HTTP gateway
```
to:
```
local cache → Bitswap (preferred beacons → full swarm) → HTTP gateway (last-resort)
```

And add a "Lantern beacon" box showing it as one of the peer types in the swarm.

## 5. Add `lantern beacon` to the quick-start

After the daemon section, add:

> ### Help the network (optional)
> ```sh
> ./lantern beacon --cache-size 5GiB
> ```
> Runs as a state-serving Bitswap peer. Other Lantern users can fetch verified state from you. Zero cost: you're already on libp2p, the cache holds blocks you've already verified yourself.

## 6. Trim "Why no CGo?" section

Currently long. Now that the binary is shipped and the no-CGo promise is proven, the section can be 3 lines:

> Pure-Go BLS via gnark-crypto. Pure-Go libp2p, Bitswap, badger, DRAND. Single static binary, runs on any OS without a build toolchain. That promise survived through Phase 9; it's no longer aspirational.

## 7. Add "V1.2 GA targets" callout

A small block after the status matrix, signaling the real GA milestone:

> 🎯 **Heading toward V1.2 GA**: install.sh one-liner, 5-of-N source quorum at bootstrap, SwiftUI menu-bar app, reproducible signed builds. See [INSTALLER-SPEC.md](INSTALLER-SPEC.md).

---

Source: in-session conversation 2026-05-21 21:23-21:28 CPH.
