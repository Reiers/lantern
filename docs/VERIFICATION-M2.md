# Milestone 2 verification: external operator runs stock Curio-PDP on Lantern

**Status: TEMPLATE, to be filled in from the external operator's run.** Replace every `<placeholder>` and remove this line when complete.

Milestone 2 criteria:

| Criterion | Where | Status |
|---|---|---|
| BUILD.md walks a new operator from a clean machine to a built, verified Lantern binary | [BUILD.md](../BUILD.md) | <Met / Not met> |
| RUN-PDP.md walks a new operator to a working Lantern daemon serving stock Curio-PDP | [RUN-PDP.md](../RUN-PDP.md) | <Met / Not met> |
| An external operator followed both guides and reported back | Operator section below, report issue <link> | <Met / Not met> |
| This document records identity, hardware, build time and issues | This document | <Met / Not met> |
| Every friction point is fixed or filed as a public issue | Friction table below | <Met / Not met> |

## Operator

| | |
|---|---|
| Name or handle | <name> |
| GitHub | <@handle> |
| Organisation (if any) | <org> |
| Relationship to the Lantern project | <none / other, describe> |
| Operator report issue | <https://github.com/Reiers/lantern/issues/N> |
| Dates of the run (UTC) | <start> to <end> |

## Environment

| | |
|---|---|
| Machine | <vendor / model, VM or bare metal> |
| CPU | <model, cores / threads> |
| RAM | <GB> |
| Disk | <type and size for the stack; for piece storage> |
| OS | <distribution, version, kernel> |
| Network | <public IP / behind NAT; how HTTPS was exposed: Cloudflare Tunnel, own TLS, reverse proxy> |
| Docker / Compose | <docker version> / <docker compose version> |
| Curio | image <filecoin/curio-pdp:calibnet@sha256:...>, curio repo commit <sha> |
| Hardware summary | <one line: CPU / RAM / disk, as entered in the operator report> |

## Build or download (BUILD.md, RUN-PDP.md step 2)

| | |
|---|---|
| How Lantern was obtained | <built from source per BUILD.md / downloaded release binary / one-line installer> |
| Lantern tag | <v1.10.1 or later> |
| Go (if built) | <go version output> |
| Build command | <as in BUILD.md step 4 / deviations> |
| Time: prerequisites | <s> |
| Time: Go install | <s> |
| Time: clone + checkout | <s> |
| Time: go build | <s> |
| **Total build time** | **<s>** (or "not built") |
| `lantern version` | <output> |
| Binary sha256 | <hash> |
| Matches published release hash | <MATCH / mismatch / not checked>; method: <scripts/repro-build.sh, sha256sum -c on the download> |

## Run (RUN-PDP.md)

| Step | Pass / fail | Time taken | Notes |
|---|---|---|---|
| 2.1 Docker Engine + Compose >= 2.24.4 | <pass / fail> | <min> | <docker / compose versions> |
| 2.2 Lantern on PATH (v1.10.1+) | | | <built / downloaded> |
| 3. `lantern init --network calibration --no-wallet` + `node-type pdp` | | | <quorum line: N distinct operators agree (need 3); anchor epoch> |
| 4. Daemon as a systemd user service on docker0 with `--allow-remote-rpc` | | | <option A / B; listen address; StateNetworkVersion> |
| 5. `.env` (SKIFF_HTTP_DOMAIN set before first start, FULLNODE_API_INFO with `/ip4/<bridge-ip>`), compose up with three files | | | <migration time; no Forest container; Chain page Reachability ok> |
| 6. Admin GUI over SSH tunnel (port 4701) | | | |
| 7. Storage attached (>= 100 GiB free) | | | <capacity> |
| 8. PDP wallet created and funded (>= 6 tFIL) | | | <faucet used> |
| 9. Public HTTPS, `/pdp/ping` | | | <Curio GUI tunnel / hand-run cloudflared / own domain and TLS; 503 before wallet, 200 after> |
| 10. FWSS registration | | | <minutes to provider ID> |
| 11. Client data set created (`upload-test.mjs`) | | | <minutes to data set ID; extra USDFC needed?> |
| 12. Proofs observed | | | <number of consecutive proving periods> |
| **Time to first proof** | | **<h:mm from data set creation to first provePossession>** | |

Lantern daemon command line exactly as run (must not contain `--vm-bridge-rpc` for a bridge-off result):

```
<lantern daemon --network calibration --listen <bridge-ip>:1234 --allow-remote-rpc ...>
```

`FULLNODE_API_INFO` host part (token redacted): `<redacted>:/ip4/<ip>/tcp/1234/http`

## PDP evidence (calibration)

| Item | Value | Explorer link |
|---|---|---|
| Provider ID | <id> | |
| PDP wallet address | <0x...> | <link> |
| FWSS registration tx | <0x...> | <link> |
| Data set ID | <id> | |
| CreateDataSet tx | <0x...> | <link> |
| AddPieces tx | <0x...> | <link> |
| First NextProvingPeriod tx | <0x...> | <link> |
| First PossessionProven tx | <0x...> | <link> |
| Later PossessionProven txs | <0x...>, <0x...> | |
| `upload-test.mjs status` output (through Lantern) | <paste> | |

All provider transactions above were signed by Curio-PDP and published through the operator's Lantern (`eth_sendRawTransaction`), with no Lotus or Forest in the stack.

## Friction

Every problem the operator hit, however small. Each row needs either a fix (PR link) or a public issue.

| # | Guide / step | Description | Severity (blocker / major / minor / doc) | Resolution |
|---|---|---|---|---|
| 1 | <RUN-PDP.md step N> | <what happened> | <severity> | <fixed in #PR / filed as #issue / upstream issue link> |

Friction found during the maintainers' own dry run before the external run (for reference):

| ID | Description | Resolution |
|---|---|---|
| F1 | Clean v1.10.0 linux-amd64 build did not reproduce the published sha256 (module cache path embedded via assembler includes) | fixed in #174 (pinned container recipe, scripts/repro-build.sh) |
| F2 | Stale release references in README / REPRODUCIBLE-BUILDS, outdated `lantern help` banner | fixed in #175 |
| F3 | `init` used `--filecoin-network` while other commands use `--network` | fixed in #175 (`--network` works everywhere) |
| F4 | Release `.sha256` files name the CI path, so `sha256sum -c` fails next to a downloaded file | fixed in #174 for releases from v1.10.1 |
| F5 | `init` "Next steps" printed a bare `lantern daemon` (mainnet) after a calibration init | fixed in #175 |
| F6 | Unauthenticated read methods return results (by design); write methods are rejected without a token | documented in RUN-PDP.md step 4 |
| F7 | Upstream Curio docs and `.env` suggest `FULLNODE_API_INFO=/ip4/host.docker.internal/...`, which crash-loops Curio | <upstream issue link> |
| F8 | Some FEVM `eth_call`s missed locally bridge-off (`unsupported opcode 0x5e`, MCOPY) | fixed in #177 and #178; remaining gaps tracked in #183 |
| F9 | Bridge-off `eth_getTransactionReceipt` missing / missing event logs, blocking data set creation (`DataSetCreated event not found in receipt`) | fixed in #173 and #177 |
| F10 / F11 | Mainnet init could not reach the old 5-source quorum; the daemon started without an init anchor | fixed in #176 (operator-aware quorum, daemon refuses without anchor) |
| F12 | After a Lantern restart, receipt lookups for older txs exceeded Curio's RPC timeout (`i/o timeout`) | fixed in #179 (persistent eth tx index) |
| Reorg | Curio's reorg check failed on bridge-off eth blocks without transactions | fixed in #180 |
| Client | Data set create reverted with `insufficient funds to cover lockup after function execution` (client owned earlier data sets) | documented in RUN-PDP.md; `EXTRA_DEPOSIT_USDFC` in upload-test.mjs |
| NV29 | Lantern v1.10.0 predated NV29 (Solstice): FEVM contracts served as "no code" | fixed in #172 |

## Operator statement

> <In the operator's words: what they did, whether the guides were sufficient, anything they would change.>

## Maintainer sign-off

| | |
|---|---|
| Verified by | <name> |
| Date | <date> |
| Evidence checked | <explorer links opened, binary hash compared, ...> |
