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

## Build (BUILD.md)

| | |
|---|---|
| Lantern tag | <vX.Y.Z or rc> |
| Go | <go version output> |
| Build command | as in BUILD.md step 4 / <deviations> |
| Time: prerequisites | <s> |
| Time: Go install | <s> |
| Time: clone + checkout | <s> |
| Time: go build | <s> |
| **Total build time** | **<s>** |
| `lantern version` | <output> |
| Binary sha256 | <hash> |
| Matches published release hash | <yes / no / not checked>; method: <docs/REPRODUCIBLE-BUILDS.md, scripts/repro-build.sh> |

## Run (RUN-PDP.md)

| Step | Result | Time taken | Notes |
|---|---|---|---|
| 3. lantern init --network calibration --no-wallet | <ok / problem> | <min> | <anchor epoch, quorum> |
| 4. Daemon as a service on docker0 | <option A / B> | | <listen address> |
| 5. Curio-PDP up against Lantern | | | <no Forest container confirmed> |
| 6. Admin GUI over SSH | | | |
| 7. Storage attached | | | <capacity> |
| 8. PDP wallet created and funded | | | <faucet used> |
| 9. Public HTTPS | | | <method> |
| 10. FWSS registration | | | |
| 11. Client data set created | | | |
| 12. Proofs observed | | | <number of proving periods> |

Lantern daemon command line exactly as run (must not contain `--vm-bridge-rpc` for a bridge-off result):

```
<lantern daemon --network calibration --listen 172.17.0.1:1234 --allow-remote-rpc ...>
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
| F1 | Clean v1.10.0 linux-amd64 build did not reproduce the published sha256 | <link> |
| F2 | Stale release references in README / REPRODUCIBLE-BUILDS, outdated `lantern help` banner | <this PR> |
| F3 | `init` used `--filecoin-network` while other commands use `--network` | <this PR: --network works everywhere> |
| F4 | Release `.sha256` files name the CI path, so `sha256sum -c` fails next to a downloaded file | <link> |
| F5 | `init` "Next steps" printed a bare `lantern daemon` (mainnet) after a calibration init | <this PR> |
| F6 | Unauthenticated read methods return results (by design); write methods are rejected without a token | documented in RUN-PDP.md |
| F7 | Upstream Curio docs and `.env` suggest `FULLNODE_API_INFO=/ip4/host.docker.internal/...`, which crash-loops Curio | <upstream issue link> |
| F8 | Some FEVM `eth_call`s missed locally bridge-off (`FEVM method requires --vm-bridge-rpc`) | <link> |
| F9 | Bridge-off `eth_getTransactionReceipt` missing / missing event logs, blocking data set creation | <link> |
| NV29 | Lantern v1.10.0 predated NV29 (Solstice): FEVM contracts served as "no code" | fixed in #172 |

## Operator statement

> <In the operator's words: what they did, whether the guides were sufficient, anything they would change.>

## Maintainer sign-off

| | |
|---|---|
| Verified by | <name> |
| Date | <date> |
| Evidence checked | <explorer links opened, binary hash compared, ...> |
