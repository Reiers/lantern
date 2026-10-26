# Running stock Curio-PDP on Lantern (calibration)

This walkthrough takes a machine with Docker to a working PDP storage provider on the Filecoin **calibration** network: the unmodified Curio-PDP Docker image (`filecoin/curio-pdp:calibnet`) using **Lantern as its only chain node**. At the end you have a provider registered with Filecoin Warm Storage Service (FWSS), a client data set stored on it and proofs landing on chain.

Build or install Lantern first: [BUILD.md](BUILD.md).

## What Lantern does here

Curio-PDP needs a Lotus-compatible chain API (`FULLNODE_API_INFO`). The upstream Docker stack ships Forest for that, which needs a chain snapshot and tens of GB of disk. In this setup **Curio talks only to Lantern**, and there is no Lotus or Forest anywhere. Lantern anchors to the chain head with a multi-source quorum, follows the head over libp2p gossip, and verifies every block, state read and FEVM `eth_call` locally against content-addressed chain data. Curio's own messages (FWSS registration, data set creation, proofs) are signed by Curio and published through Lantern's local message pool. Lantern needs a few GB of disk and holds no keys in this setup.

```
            internet (HTTPS, port 80 behind a tunnel or 443)
                              |
   +--------------------------v---------------------------+
   | host                                                 |
   |   docker network skiff-net                           |
   |   +-----------------+      +----------------------+  |
   |   | skiff           |----->| yugabyte (HarmonyDB) |  |
   |   | (Curio-PDP)     |      +----------------------+  |
   |   +--------+--------+                                |
   |            | FULLNODE_API_INFO = token:/ip4/172.17.0.1/tcp/1234/http
   |            v                                         |
   |   lantern daemon --network calibration               |
   |     --listen 172.17.0.1:1234 (docker0, not public)   |
   +------------------------------------------------------+
                              |
                    libp2p gossip + Bitswap (Filecoin network)
```

## Contents

1. [Hardware](#1-hardware)
2. [Prerequisites](#2-prerequisites)
3. [Initialise Lantern for calibration](#3-initialise-lantern-for-calibration)
4. [Run the Lantern daemon as a service on the Docker bridge](#4-run-the-lantern-daemon-as-a-service-on-the-docker-bridge)
5. [Start Curio-PDP against Lantern](#5-start-curio-pdp-against-lantern)
6. [Open the admin GUI](#6-open-the-admin-gui)
7. [Attach storage](#7-attach-storage)
8. [Create and fund the PDP wallet](#8-create-and-fund-the-pdp-wallet)
9. [Public HTTPS endpoint](#9-public-https-endpoint)
10. [Register with FWSS](#10-register-with-fwss)
11. [Create a data set as a client](#11-create-a-data-set-as-a-client)
12. [Confirm that proofs land](#12-confirm-that-proofs-land)
13. [Operating notes](#13-operating-notes)
14. [Troubleshooting](#14-troubleshooting)

Throughout, replace placeholders in angle brackets (`<like-this>`). `pdp.example.com` stands for your provider's public hostname.

## 1. Hardware

| | Production (Curio-PDP docs) | Test run on calibration |
|---|---|---|
| CPU | 16 threads | any modern x86_64 or arm64; we used 16 threads and they were mostly idle |
| RAM | 64 GiB | we ran on 60 GB and the whole stack (Lantern + Yugabyte + Curio-PDP) used a few GB |
| Fast disk (NVMe/SSD) | Yugabyte and the Curio repo | 20 GB free is plenty for a test; Lantern's PDP tier keeps a 3 GiB persistent cache |
| Piece storage | as much as you sell; may be HDD | FWSS registration requires declaring at least 100 GiB |
| Network | public HTTPS reachability for the PDP API | a home connection behind NAT works with a Cloudflare Tunnel (step 9) |
| GPU | not needed | not needed |

The production figures come from the Curio-PDP documentation (<https://docs.curiostorage.org/getting-started/curio-pdp>). Lantern itself replaces the Forest container and needs far less than a full chain node.

## 2. Prerequisites

- **Linux host** (the dry run used Ubuntu 26.04; the build was also checked in a clean Ubuntu 24.04 container). The walkthrough uses a systemd user service and the `docker0` bridge.
- **Lantern** with network version 29 support, built or installed per [BUILD.md](BUILD.md), on your `PATH`.
- **Docker Engine** and the **Docker Compose plugin v2.24.4 or newer** (the Lantern overlay uses the `!override` merge tag). Check with `docker compose version`. Your user should be in the `docker` group (no sudo needed for anything below except the optional `loginctl enable-linger`).
- **git** and **curl**.
- A **public hostname** for the PDP API (a domain you control; with Cloudflare Tunnel the domain must be on Cloudflare).
- **tFIL** for the PDP wallet, and for the client test also **USDFC** (both from calibration faucets, step 8).
- **Node.js 20+** on any machine for the client test in step 11.

```sh
lantern version          # must be a release with NV29 support (see BUILD.md)
docker compose version   # v2.24.4 or newer
ip -4 addr show docker0  # note the inet address, usually 172.17.0.1
```

<!-- TODO-VERIFY: name the exact minimum Lantern release here once tagged (needs PR #172 NV29 support and the bridge-off receipt-logs fix). -->

## 3. Initialise Lantern for calibration

Pick a data directory (default `~/.lantern`; set `LANTERN_HOME` to use another one, and keep it identical for every command and for the service).

```sh
export LANTERN_HOME="$HOME/.lantern"
export LANTERN_PASS=''          # explicit empty: no keystore passphrase (Lantern holds no keys here)

lantern init --network calibration --no-wallet
lantern node-type pdp --network calibration
```

- `init` asks independent sources (libp2p peers and public RPC operators) for the current finalized head and refuses to write a trust anchor unless at least 3 agree. It takes about 15 s and ends with `Lantern initialised` and a list of next steps that all carry `--network calibration`.
- `--no-wallet`: Curio-PDP keeps its own PDP signing key in Yugabyte, so Lantern needs none.
- `node-type pdp` selects the PDP tier: a persistent 3 GiB block cache, so warm PDP / FWSS / registry contract state survives restarts.
- `init` mints API tokens under `$LANTERN_HOME/calibration/secrets/` (`token` is the admin token Curio uses).

`--network` is the one network flag for every Lantern command (`init`, `node-type`, `daemon`, `info`, `doctor`, `wallet`, `service install`). The older `lantern init --filecoin-network calibration` spelling still works. A command without `--network` means **mainnet**.

## 4. Run the Lantern daemon as a service on the Docker bridge

### Why the Docker bridge address

Curio-PDP runs inside a Compose network. Inside the container, `127.0.0.1` is the container itself, so a Lantern bound to the host's loopback is unreachable. The host's `docker0` address (usually `172.17.0.1`) is reachable from containers but is not routed from your LAN or the internet. Binding Lantern there exposes it to local containers only.

### Security

- Lantern refuses a non-loopback `--listen` unless you pass `--allow-remote-rpc`. The flag is an acknowledgement, not an open door: methods that write, sign or administer still require the Bearer token (`missing permission to invoke 'Filecoin.WalletNew' (need 'write')` without it). Read methods (chain, state, `eth_` reads) answer without a token.
- **Never use `--listen 0.0.0.0:1234` on a host with a public interface.** Bind the `docker0` IP only.
- If you run a host firewall, keep port 1234 closed on public interfaces. With `ufw`, allow the Docker subnets to reach it (see Troubleshooting if Curio cannot connect).
- Keep the admin token (`secrets/token`) private: it is the credential in `FULLNODE_API_INFO`.
- The metrics and dashboard listener (`--metrics`) stays on `127.0.0.1`.

### Option A: `lantern service install`

```sh
lantern service install --network calibration --listen 172.17.0.1:1234 -- --allow-remote-rpc --metrics 127.0.0.1:9092
```

This writes `~/.config/systemd/user/lantern.service` with `LANTERN_HOME` (from your environment) and `LANTERN_PASS` set, then enables and starts it. Flags after `--` are passed to `lantern daemon` unchanged. Logs go to `$LANTERN_HOME/lantern.log` and `$LANTERN_HOME/lantern.err`. For an encrypted keystore add `--passphrase-file <file>` (the file holds the passphrase, `chmod 600`).

`--network` and the extra daemon flags for `service install` are new in the release after v1.10.0. On older binaries, `service install` always starts a mainnet daemon on `127.0.0.1:1234`: use option B.

### Option B: hand-written unit (any version)

The repo ships [deploy/curio-pdp/lantern.service](deploy/curio-pdp/lantern.service). It reads `LANTERN_HOME` and `LANTERN_PASS` from an `EnvironmentFile` and logs to the journal.

```sh
mkdir -p ~/.config/systemd/user ~/.config/lantern
cp deploy/curio-pdp/lantern.service ~/.config/systemd/user/lantern.service
cat > ~/.config/lantern/lantern.env <<EOF
LANTERN_HOME=$HOME/.lantern
LANTERN_PASS=
EOF
chmod 600 ~/.config/lantern/lantern.env
# edit ExecStart in the unit if your binary is not ~/.local/bin/lantern or docker0 is not 172.17.0.1
systemctl --user daemon-reload
systemctl --user enable --now lantern.service
journalctl --user -u lantern -f
```

The unit runs:

```
lantern daemon --network calibration --listen 172.17.0.1:1234 --allow-remote-rpc --metrics 127.0.0.1:9092
```

### Either option: keep it running after logout

User services stop when you log out unless lingering is enabled (one-time, needs sudo or an administrator):

```sh
sudo loginctl enable-linger "$USER"
```

`docker0` exists only while Docker runs. If Lantern starts first at boot, the bind fails and systemd restarts it until Docker is up.

### Check the daemon

```sh
curl -s http://172.17.0.1:1234/healthz            # ok
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"Filecoin.StateNetworkVersion","params":[[]]}' \
  http://172.17.0.1:1234/rpc/v1                     # {"id":1,"jsonrpc":"2.0","result":29}
lantern info --network calibration
```

`lantern info` reads the address the daemon actually bound and prints the value Curio needs:

```
FULLNODE_API_INFO (daemon on 172.17.0.1:1234):
  <token>:/ip4/172.17.0.1/tcp/1234/http
Daemon healthz: ok
```

`lantern info --network calibration --token-only` prints only the token (same as `cat $LANTERN_HOME/calibration/secrets/token`). The node follows the head within about 30 s of starting. `StateNetworkVersion` must be **29**: anything lower means a Lantern build that predates the current network upgrade.

## 5. Start Curio-PDP against Lantern

Use the upstream stack from `filecoin-project/curio` unchanged, plus one overlay file from this repo that removes Forest.

```sh
git clone --depth 1 https://github.com/filecoin-project/curio.git
cd curio/docker/skiff
cp /path/to/lantern/deploy/curio-pdp/docker-compose.lantern.yaml .
cp .env .env.upstream
cp /path/to/lantern/deploy/curio-pdp/env.lantern.example .env
```

Our dry run used curio commit `5910e6d`; newer commits should work the same as long as `docker/skiff` keeps the same service names.

Edit `.env` ([deploy/curio-pdp/env.lantern.example](deploy/curio-pdp/env.lantern.example) documents every line):

```sh
SKIFF_IMAGE=filecoin/curio-pdp:calibnet
SKIFF_HTTP_DOMAIN=pdp.example.com
FULLNODE_API_INFO=<token from lantern info>:/ip4/172.17.0.1/tcp/1234/http
```

- **Set `SKIFF_HTTP_DOMAIN` before the first start.** It is seeded into the base config layer (`HTTP.DomainName`) once. If you start with the default `localhost`, you have to change `HTTP.DomainName` later (step 9).
- **`FULLNODE_API_INFO` must use an IP literal** after `/ip4/`. The upstream `.env` and docs suggest `/ip4/host.docker.internal/tcp/1234/http`; with that, Curio exits with `unknown url scheme ''` and restarts in a loop.
- Optionally move `YUGABYTE_DATA` / `SKIFF_REPO_DATA` to fast disk and `SKIFF_STORAGE` (mounted as `/data`) to bulk disk.

Start Yugabyte and Curio-PDP only. Name both services every time, otherwise Compose also starts the Forest service from the base file:

```sh
C="docker compose -f docker-compose.yaml -f docker-compose.calibnet.yaml -f docker-compose.lantern.yaml"
$C pull yugabyte skiff
$C up -d --no-build yugabyte skiff
$C ps
$C logs -f skiff
```

- The first pull fetches `filecoin/curio-pdp:calibnet` and `yugabytedb/yugabyte:2024.1.0.0-b129` (a few minutes).
- Yugabyte turns healthy after about 60 s; Curio runs its database migrations on first start (about 2 minutes), then starts its tasks.
- `$C ps` lists `skiff-yugabyte` and `skiff`, and no `skiff-forest`.
- The skiff log should show no repeated chain connection errors. A startup warning about a missing PDP key is expected until step 8.

<!-- TODO-VERIFY: paste a representative skiff log line that confirms the chain connection to Lantern (for example the chain head / ChainNotify line) from the live run. -->

To stop the stack: `$C down` (data stays under the paths in `.env`). To update the Curio image: `$C up -d --no-deps --no-build --pull always skiff`.

## 6. Open the admin GUI

The admin GUI listens on `127.0.0.1:4701` on the host. It has **no login**: anyone who reaches it can manage keys and config, so never publish port 4701. From your workstation:

```sh
ssh -fN -L 4701:127.0.0.1:4701 <user>@<host>
```

Then open <http://127.0.0.1:4701>. The **PDP Guide** page walks through the remaining provider steps (storage, wallet, domain, registration) and shows a status for each.

## 7. Attach storage

Pieces are stored under `/data` inside the container, which is `SKIFF_STORAGE` on the host (default `curio/docker/skiff/data`). Create a directory for them:

```sh
docker exec skiff mkdir -p /data/pdp-pieces
```

In the GUI open **Storage** (or PDP Guide, "Select storage folders"), enter `/data/pdp-pieces` and attach it. Curio writes a `sectorstore.json` there and the PDP Guide shows the storage step as OK with the free capacity.

## 8. Create and fund the PDP wallet

In the GUI open **PDP**, wallet section, and click **Create**. Curio generates a secp256k1 key, stores it in Yugabyte and shows the private key **once**: save it somewhere safe before closing the dialog. Only one PDP key is allowed per Curio cluster. Note the `0x` address.

Fund that address with tFIL from a calibration faucet:

- <https://faucet.reiers.io> (calibration tFIL and USDFC, run by the Lantern maintainer)
- the faucets listed at <https://docs.filecoin.io/smart-contracts/developing-contracts/get-test-tokens> (for example the ChainSafe calibration faucet)

<!-- TODO-VERIFY: minimum tFIL needed for FWSS registration plus a few weeks of proving on calibration (registry registration fee + gas). We funded far more than needed. -->

Check the balance through your own Lantern (this read is verified locally):

```sh
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["<pdp 0x address>","latest"]}' \
  http://172.17.0.1:1234/rpc/v1
```

The result is the balance in attoFIL as hex. The PDP Guide shows the wallet step as OK once the key exists.

## 9. Public HTTPS endpoint

FWSS registration and clients need to reach the PDP API at `https://pdp.example.com`. Curio-PDP serves it on port **80** with `HTTP.DelegateTLS = true` (TLS terminated in front of Curio, the default) or on **443** with Curio's built-in Let's Encrypt TLS (`HTTP.DelegateTLS = false`). Open only 80 and 443 to the internet, never 4701 or 1234.

### Option A: Cloudflare Tunnel (works behind NAT; used in our dry run)

The domain must be on Cloudflare. The GUI **PDP Guide** has a "Configure Cloudflare tunnel" step that walks through it. To do it by hand:

1. In the Cloudflare Zero Trust dashboard, create a tunnel and add a public hostname `pdp.example.com` with service `http://localhost:80`.
2. Run the connector on the host with the tunnel token Cloudflare shows:

```sh
docker run -d --name cloudflared --restart unless-stopped --network host \
  cloudflare/cloudflared:latest tunnel --no-autoupdate run --token <tunnel-token>
```

Keep `HTTP.DelegateTLS = true` (the default); Cloudflare terminates TLS.

### Option B: public IP, your own DNS and Curio's built-in TLS

Point an `A`/`AAAA` record for `pdp.example.com` at the host, open TCP 80 and 443, and set `HTTP.DelegateTLS = false` in the base layer (GUI, Configuration) so Curio obtains a Let's Encrypt certificate on 443. Restart skiff after config changes: `$C restart skiff`.

<!-- TODO-VERIFY: option B (Curio built-in TLS) was not exercised in the dry run; confirm the exact GUI path for editing the base layer and that no other HTTP.* setting is required. -->

### If you started with SKIFF_HTTP_DOMAIN=localhost

Edit `HTTP.DomainName` in the **base** layer in the GUI configuration editor (Curio-PDP reads only the base layer), save, then `$C restart skiff`.

### Check

```sh
curl -i https://pdp.example.com/pdp/ping
```

Before the wallet exists this returns `503` with `PDP wallet not configured`, which already proves the path from the internet to Curio works. After step 8 it returns `200`.

<!-- TODO-VERIFY: exact /pdp/ping response body with the wallet configured. -->

## 10. Register with FWSS

In the GUI open the **Register** tab and fill in name, description, location (free text, for example `EU-NO`) and capacity (minimum 100 GiB). Submit.

Curio signs the registration transaction with the PDP key and sends it to Lantern (`eth_sendRawTransaction`); Lantern publishes it from its local message pool to the calibration network. After it lands (usually within a minute or two) the GUI shows your **provider ID**. Note it for step 11. You can look the transaction up on a calibration explorer such as <https://calibration.filfox.info> or <https://filecoin-testnet.blockscout.com>.

Verify the public endpoint the way FWSS clients see it:

```sh
pdptool ping --service-url https://pdp.example.com --service-name public
# or without pdptool:
curl -i https://pdp.example.com/pdp/ping
```

`pdptool` is built from the Curio repository (`make pdptool`).

<!-- TODO-VERIFY: whether pdptool is available inside the filecoin/curio-pdp image (docker exec skiff pdptool ...) and the exact make target. -->

## 11. Create a data set as a client

A provider proves only data it stores, so a client has to create a data set and add a piece. [deploy/curio-pdp/upload-test.mjs](deploy/curio-pdp/upload-test.mjs) is a minimal client built on `@filoz/synapse-sdk`. Run it on any machine with Node.js 20+.

1. Create a **separate client wallet** (any secp256k1 key; never the PDP key) and fund it with tFIL (gas) and USDFC (storage payment) from <https://faucet.reiers.io> or another calibration faucet.
2. Run:

```sh
cd deploy/curio-pdp
npm install
CLIENT_PRIVATE_KEY=0x<client key> PROVIDER_ID=<your provider id> node upload-test.mjs
```

The script:

1. creates a storage context for your provider (`createContext({ providerId })`);
2. prepares payment: deposits USDFC and approves FWSS if needed (`deposit/approval tx`);
3. uploads a 4 MiB random piece to `https://pdp.example.com` (`uploaded piece`);
4. waits while **your provider** sends the CreateDataSet / AddPieces transactions through Lantern, and prints the data set ID when they are confirmed (`confirmed: data set <id>`).

The client uses the public Glif calibration RPC by default (`RPC_URL` overrides it). That is deliberate: the client is not part of the provider stack. On the provider side, every chain read and write goes through Lantern.

<!-- TODO-VERIFY: typical wall-clock for steps 2-4 and the exact log lines from the live run once the receipt-logs fix is deployed. -->

## 12. Confirm that proofs land

**On chain, with the script** (read-only, no key):

```sh
node upload-test.mjs status <dataSetId>
# read through your own Lantern instead of Glif:
RPC_URL=http://172.17.0.1:1234/rpc/v1 node upload-test.mjs status <dataSetId>
```

It reads the PDPVerifier contract and prints `live`, the active piece count, the next challenge epoch and the last proven epoch. `last proven epoch` greater than 0 means a proof landed; run it again after the next challenge epoch and it should advance once per proving period. (Through Lantern, the first call can take tens of seconds while cold contract state is fetched and verified.)

**On an explorer.** The script prints the PDPVerifier address. Open it on <https://filecoin-testnet.blockscout.com> (Logs / Events tab) or <https://calibration.filfox.info> and filter for your data set ID. Expect, in order: `DataSetCreated`, `PiecesAdded`, `NextProvingPeriod`, then `PossessionProven` every proving period. The transactions are sent from your PDP wallet address, so the address page of that wallet lists them too.

**In the Curio GUI.** The PDP pages list the data set, its pieces and the proving tasks.

<!-- TODO-VERIFY: exact Curio GUI page names for data sets and proof history, and the time to the first PossessionProven on calibration (one proving period after the data set's first challenge epoch). -->

Record the provider ID, data set ID and these transaction hashes in [docs/VERIFICATION-M2.md](docs/VERIFICATION-M2.md) if you are doing a verification run.

## 13. Operating notes

- **Upgrading Lantern:** replace the binary and restart the service (`systemctl --user restart lantern`). Curio reconnects on its own.
- **Network upgrades** change actor code. A Lantern release that predates an upgrade cannot serve FEVM state after it (see Troubleshooting). Watch the Lantern releases before each Filecoin network upgrade.
- **Bridge-off:** this guide runs Lantern without `--vm-bridge-rpc`, so no chain query is delegated to a third party. `--vm-bridge-rpc <url>` exists as an optional fallback to your own Forest/Lotus; do not add it unless Troubleshooting tells you to, and note that it makes the run not bridge-off.
- **Backups:** the PDP key lives in Yugabyte; back up `YUGABYTE_DATA` (see the Curio docs). Lantern's `secrets/` directory is backed up automatically on each start (last 7 kept) and holds only API tokens in this setup.
- **Metrics and dashboard:** <http://127.0.0.1:9092/dashboard/> on the host (SSH-forward port 9092 like the GUI).

## 14. Troubleshooting

Every row in the first table is an error we hit in the internal dry run that produced this guide.

| Symptom | Cause | Fix |
|---|---|---|
| Curio exits with `unknown url scheme ''` and restarts in a loop | `FULLNODE_API_INFO=/ip4/host.docker.internal/...` (as in the upstream `.env`). `/ip4/` needs an IP literal. | Use `<token>:/ip4/172.17.0.1/tcp/1234/http` (your `docker0` IP), then `$C up -d --no-build yugabyte skiff`. |
| Curio logs `GetPDPConfig: no contract code at given address` | Lantern predates network version 29 (v1.10.0 or older). It does not know the actors v19 EVM code and serves contracts as "no code". | Upgrade Lantern to a release with NV29 support. `Filecoin.StateNetworkVersion` must return 29. |
| `refusing to bind RPC to non-loopback address "172.17.0.1:1234" without --allow-remote-rpc` | Intentional guard for non-loopback binds. | Add `--allow-remote-rpc` (and keep the bind on `docker0`, never `0.0.0.0`). |
| `https://pdp.example.com/pdp/ping` returns `503 PDP wallet not configured` | Expected before the PDP key exists. | Step 8. |
| PDP Guide domain check fails, `HTTP.DomainName = "localhost"` | `SKIFF_HTTP_DOMAIN` was not set before the first start. | Edit `HTTP.DomainName` in the base layer, `$C restart skiff`. |
| A transaction sent by Curio is not found on a public RPC or explorer for several minutes | Public RPC/explorer lag; the transaction had landed. | Check another explorer, or ask Lantern: `eth_getTransactionByHash` / `eth_getTransactionReceipt` on `http://172.17.0.1:1234/rpc/v1`. |
| Lantern log: `FEVM method requires --vm-bridge-rpc` (a few per minute) | Some FEVM reads (certain `eth_call`s, transaction receipts) were not yet served locally bridge-off in early builds. Curio retries. | Upgrade to a release that serves them locally. Interim only: add `--vm-bridge-rpc https://api.calibration.node.glif.io/rpc/v1` to the daemon flags (this delegates those calls to Glif, so the run is no longer bridge-off). |
| Curio: `DataSetCreated event not found in receipt` or `failed to extract pieces from receipt`; the data set never appears in the GUI | Lantern returned the transaction receipt without its event logs (early bridge-off receipt path). | Upgrade Lantern to a release with the bridge-off receipt-logs fix. |

<!-- TODO-VERIFY: replace "a release with ..." in the last two rows with the concrete version once the bridge-off receipt fix (branch fix/bridgeoff-eth-receipts) is merged and tagged, and link its issue/PR. -->

Other problems you may run into:

| Symptom | Cause | Fix |
|---|---|---|
| `lantern daemon` syncs mainnet or `lantern info` says "not initialised" | A command was run without `--network calibration`. | Add `--network calibration` to every command and to the service. |
| Compose rejects `docker-compose.lantern.yaml` (unknown tag / invalid `depends_on`) | Compose older than v2.24.4 does not understand `!override`. | Update the Docker Compose plugin. |
| A `skiff-forest` container is running or downloading a snapshot | `up -d` was run without naming services. | `$C rm -sf forest`, then always `$C up -d --no-build yugabyte skiff`. |
| Curio logs connection refused or timeouts to `172.17.0.1:1234` | Lantern not running, bound to another address, or a host firewall drops traffic from the Compose network (for example `ufw` default deny). | `systemctl --user status lantern`, `curl http://172.17.0.1:1234/healthz` on the host, then allow the Docker subnets, for example `sudo ufw allow from 172.16.0.0/12 to any port 1234 proto tcp`. |
| Lantern service fails at boot with "cannot assign requested address" | `docker0` does not exist yet. | Nothing to do: systemd restarts Lantern until Docker is up. Check `systemctl --user status lantern` after boot. |
| User service stops when you log out | Lingering not enabled. | `sudo loginctl enable-linger $USER`. |
| Unauthenticated `Filecoin.WalletList` returns `null` | Read methods need no token, and the node has no wallets. | Expected. Write, sign and admin methods return `missing permission` without the token. |

Still stuck? File an [operator report](https://github.com/Reiers/lantern/issues/new?template=operator-report.yml) with the step number, the exact error and your versions (redact tokens and keys).
