# Running stock Curio-PDP on Lantern (calibration)

> **Minimum Lantern version: v1.10.1-rc1 or later.** Earlier releases cannot serve stock Curio-PDP after the NV29 (Solstice) network upgrade: they serve FEVM contracts as "no code", return receipts without event logs, and time out on receipt lookups after a restart. Check with `lantern version` before you start.

This walkthrough takes a Linux machine to a working PDP storage provider on the Filecoin **calibration** network: the unmodified Curio-PDP Docker image (`filecoin/curio-pdp:calibnet`) using **Lantern as its only chain node**. At the end you have a provider registered with Filecoin Warm Storage Service (FWSS), a client data set stored on it, and proofs landing on chain every proving period.

Every step below is what worked, in this order, in a 24 hour calibration run by the maintainers before this guide was handed to external operators. Timings in the text come from that run.

## What Lantern does here

Curio-PDP needs a Lotus-compatible chain API (`FULLNODE_API_INFO`). The upstream Docker stack ships Forest for that, which needs a chain snapshot and tens of GB of disk. In this setup **Curio talks only to Lantern**, and there is no Lotus or Forest anywhere. Lantern anchors to the chain head with a multi-operator quorum, follows the head over libp2p gossip, and verifies every block, state read and FEVM `eth_call` locally against content-addressed chain data. Curio's own messages (FWSS registration, data set creation, proofs) are signed by Curio and published through Lantern's local message pool. Lantern needs a few GB of disk and holds no keys in this setup.

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
   |            | FULLNODE_API_INFO = token:/ip4/<bridge-ip>/tcp/1234/http
   |            v                                         |
   |   lantern daemon --network calibration               |
   |     --listen <bridge-ip>:1234 (docker0, not public)  |
   +------------------------------------------------------+
                              |
                    libp2p gossip + Bitswap (Filecoin network)
```

`<bridge-ip>` is the host's `docker0` address, typically `172.17.0.1`. The commands below use `172.17.0.1`; replace it if yours differs.

## Contents

- [Already running Curio-PDP with Lotus? Switch it to Lantern](#already-running-curio-pdp-with-lotus-switch-it-to-lantern)

1. [Hardware](#1-hardware)
2. [Prerequisites: Docker and Lantern](#2-prerequisites-docker-and-lantern)
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
13. [What success looks like](#13-what-success-looks-like)
14. [Operating notes](#14-operating-notes)
15. [Troubleshooting](#15-troubleshooting)

Throughout, replace placeholders in angle brackets (`<like-this>`). `pdp.example.com` stands for your provider's public hostname.

## Already running Curio-PDP with Lotus? Switch it to Lantern

If you already operate a Curio-PDP provider (native install or Docker) backed by Lotus or Forest, you do not need the fresh Docker stack in steps 5 to 10. Keep your Curio, your registration, your PDP key and your data sets, and only replace the chain node:

1. Install Lantern v1.10.1-rc1 or later ([BUILD.md](BUILD.md) or a release binary) on a host Curio can reach.
2. `lantern init --network calibration --no-wallet` and `lantern node-type pdp --network calibration` (step 3). If an older Lantern data directory exists and the daemon reports "no trust anchor", run `lantern repair --network calibration`.
3. Start the daemon. If Curio runs on the same host or container, loopback is enough and no `--allow-remote-rpc` is needed:
   ```
   lantern daemon --network calibration --listen 127.0.0.1:1234
   ```
   If Curio runs elsewhere (another container, the Docker bridge), bind to an address Curio can reach and add `--allow-remote-rpc` (step 4 explains the security model).
4. `lantern info --network calibration` prints `FULLNODE_API_INFO=<token>:/ip4/<ip>/tcp/<port>/http`.
5. Put **only** that value in Curio's `FULLNODE_API_INFO` (for example `/etc/curio.env` or your systemd unit) and restart Curio. Do not list Lotus as well: with several entries Curio fails over between them, and you are no longer testing Lantern. Lotus can keep running; Curio just should not know about it.
6. Check the Curio GUI **Chain** page: **RPC nodes 1/1**, the Lantern version, in sync. Then wait for your next proving window and confirm `provePossession` txs land (step 12).

Notes:
- **Lantern holds no keys.** Curio-PDP keeps the PDP key in its own database and sends Lantern only signed transactions, so nothing needs importing into Lantern. Check the key is listed on Curio's PDP page.
- For about the first 24 hours a fresh Lantern only has block headers from its own start, so Curio's `StorageGCMark` task may log `get finality tipset: header/store: not found`. It is storage garbage collection, not proving, and clears once the node is older than a day (and is fixed in the next release).


## 1. Hardware

| | Production (Curio-PDP docs) | Test run on calibration |
|---|---|---|
| CPU | 16 threads | any modern x86_64 or arm64; we used 16 threads and they were mostly idle |
| RAM | 64 GiB | we ran on 60 GB and the whole stack (Lantern + Yugabyte + Curio-PDP) used a few GB |
| Fast disk (NVMe/SSD) | Yugabyte and the Curio repo | 20 GB free is plenty for a test; Lantern's PDP tier keeps a 3 GiB persistent cache plus about 21 MiB per day of transaction index |
| Piece storage | as much as you sell; may be HDD | FWSS registration requires declaring at least 100 GiB, and the PDP Guide checks that at least 100 GiB is free |
| Network | public HTTPS reachability for the PDP API | a home connection behind NAT works with a Cloudflare Tunnel (step 9) |
| GPU | not needed | not needed |

The production figures come from the Curio-PDP documentation (<https://docs.curiostorage.org/getting-started/curio-pdp>). Lantern replaces the Forest container and needs far less than a full chain node.

## 2. Prerequisites: Docker and Lantern

You need:

- **A Linux host** (the run used Ubuntu 26.04; the Lantern build was also checked in a clean Ubuntu 24.04 container). The walkthrough uses a systemd user service and the `docker0` bridge.
- **git** and **curl**.
- **A public hostname** for the PDP API (a domain you control; for the Cloudflare Tunnel option the domain must be on Cloudflare).
- **tFIL** for the PDP wallet (at least about 11 tFIL: 5 FIL registry fee plus a 5 FIL minimum balance Curio's health check requires, step 8) and, for the client test, a second wallet with tFIL and **USDFC** (step 11).
- **Node.js 20+** on any machine for the client test in step 11.

### 2.1 Docker Engine and Compose 2.24.4 or newer

The Lantern overlay uses the Compose `!override` merge tag, which needs **Docker Compose v2.24.4 or newer**. Distribution packages are often older; Docker's own packages are not. On Debian or Ubuntu the quickest route is Docker's convenience script (or follow <https://docs.docker.com/engine/install/>):

```sh
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"      # then log out and back in
docker version
docker compose version               # must print v2.24.4 or newer
ip -4 addr show docker0              # note the inet address, typically 172.17.0.1
```

Being in the `docker` group means nothing below needs sudo, except the one-time `loginctl enable-linger` in step 4.

### 2.2 Lantern

Get a Lantern binary of **v1.10.1-rc1 or later** onto your `PATH`, either way:

- **Build from source** (about 85 s on a clean machine): follow [BUILD.md](BUILD.md).
- **Download the release binary** (Linux x86_64 shown; `lantern-linux-arm64` also exists):

```sh
TAG=v1.10.1-rc1                              # or a newer release
curl -fsSLO "https://github.com/Reiers/lantern/releases/download/$TAG/lantern-linux-amd64"
curl -fsSLO "https://github.com/Reiers/lantern/releases/download/$TAG/lantern-linux-amd64.sha256"
sha256sum -c lantern-linux-amd64.sha256       # lantern-linux-amd64: OK
mkdir -p "$HOME/.local/bin"
install -m 0755 lantern-linux-amd64 "$HOME/.local/bin/lantern"
export PATH="$HOME/.local/bin:$PATH"           # add to ~/.bashrc if needed
```

This checks the download against the published manifest. To check that the published bytes really come from the tagged source, rebuild them with `scripts/repro-build.sh` ([docs/REPRODUCIBLE-BUILDS.md](docs/REPRODUCIBLE-BUILDS.md)).

Then check:

```sh
lantern version          # lantern v1.10.1 (<commit>) Lantern+mainnet, or newer
```

`Lantern+mainnet` is only the compiled-in default; every command takes `--network calibration` at run time.

## 3. Initialise Lantern for calibration

Pick a data directory (default `~/.lantern`; set `LANTERN_HOME` to use another one, and keep it identical for every command and for the service).

```sh
export LANTERN_HOME="$HOME/.lantern"
export LANTERN_PASS=''          # explicit empty: no keystore passphrase (Lantern holds no keys here)

lantern init --network calibration --no-wallet
lantern node-type pdp --network calibration
```

`init` took about 15 s in our run. It asks independent sources (libp2p peers and public RPC providers) for the current finalized head and writes a trust anchor only if enough **distinct operators** agree. The end of a successful run looks like this (illustrative; your sources, counts and epochs differ):

```
✓ Quorum reached: 3 distinct operators agree (need 3): instance=<n> epoch=<epoch> tsk=[<cid>,...] state=<cid>
  Operators: chain.love, chainsafe-fil.io, filfox.info
  Elapsed: 15.2s, sources answered: 6/8, distinct operators reachable: 4/4
  Per-source:
    ✓ [libp2p] <peer> {chain.love}: instance=<n> epoch=<epoch> ...
    ✓ [forest] <rpc url> {filfox.info}: instance=<n> epoch=<epoch> ...
    · [forest] <rpc url> {ankr.com}: not needed (quorum already reached)
    ...
    ✓ Trust anchor written: <LANTERN_HOME>/calibration/bootstrap-anchor.json
...
✓ Lantern initialised.

Next steps:
  ... (every suggested command carries --network calibration)
```

How to read it:

- **The quorum counts operators, not endpoints.** Each source is tagged with the operator that runs it in `{braces}`. Several endpoints of one operator count once (Glif and chain.love are the same infrastructure, so they count as one). The default is **3 distinct operators** that must agree on the same finality, and **any conflicting answer fails init** (a conflict is never outvoted).
- `·` lines are sources that were not needed or not counted; `✗` lines failed (timeouts from individual public endpoints are normal as long as the quorum is reached).
- `sources answered` and `distinct operators reachable` tell you how much headroom you had.
- **`lantern daemon` refuses to start until `init` (or `lantern repair --network calibration`) has written this trust anchor** (`$LANTERN_HOME/calibration/bootstrap-anchor.json`). This is deliberate: without it the daemon would have to trust a weaker head agreement. `--allow-rpc-anchor` overrides the check with a warning; do not use it for this setup.
- If `init` fails, it prints which sources failed (and which operator runs each), how many operators answered against how many are required, and the flags that help: `lantern doctor --network calibration` re-runs the probe read-only, `--peer https://<your-node>/rpc/v1` adds a source you trust (counts as its own operator), `--no-libp2p` helps if libp2p is blocked, `--bootstrap-quorum N` accepts fewer operators. A conflict is reported separately and is never fixed by lowering the quorum.

The rest of this step:

- `--no-wallet`: Curio-PDP keeps its own PDP signing key in Yugabyte, so Lantern needs none.
- `node-type pdp` selects the PDP tier: a persistent 3 GiB block cache, so warm PDP / FWSS / registry contract state survives restarts, and a one-day persistent transaction index so Curio's receipt lookups stay fast across restarts.
- `init` mints API tokens under `$LANTERN_HOME/calibration/secrets/` (`token` is the admin token Curio uses).

`--network` is the one network flag for every Lantern command (`init`, `node-type`, `daemon`, `info`, `doctor`, `wallet`, `service install`). A command without `--network` means **mainnet**.

## 4. Run the Lantern daemon as a service on the Docker bridge

### Why the Docker bridge address

Curio-PDP runs inside a Compose network. Inside the container, `127.0.0.1` is the container itself, so a Lantern bound to the host's loopback is unreachable. The host's `docker0` address (typically `172.17.0.1`) is reachable from containers but is not routed from your LAN or the internet. Binding Lantern there exposes it to local containers only.

### Security

- Lantern refuses a non-loopback `--listen` unless you pass `--allow-remote-rpc`. The flag is an acknowledgement, not an open door: methods that write, sign or administer still require the Bearer token (without it: `missing permission to invoke 'Filecoin.WalletNew' (need 'write')`). Read methods (chain, state, `eth_` reads) answer without a token.
- Curio authenticates with the admin token you put in `FULLNODE_API_INFO` (step 5). Treat `$LANTERN_HOME/calibration/secrets/token` like a password.
- **Never use `--listen 0.0.0.0:1234` on a host with a public interface.** Bind the `docker0` IP only.
- If you run a host firewall, keep port 1234 closed on public interfaces. With `ufw`, allow the Docker subnets to reach it (see Troubleshooting if Curio cannot connect).
- The metrics and dashboard listener (`--metrics`) stays on `127.0.0.1`.

The daemon command, whichever way you run it:

```
lantern daemon --network calibration --listen 172.17.0.1:1234 --allow-remote-rpc --metrics 127.0.0.1:9092
```

### Option A: `lantern service install`

```sh
lantern service install --network calibration --listen 172.17.0.1:1234 -- --allow-remote-rpc --metrics 127.0.0.1:9092
```

This writes `~/.config/systemd/user/lantern.service` with `LANTERN_HOME` (from your environment) and `LANTERN_PASS` set, then enables and starts it. Flags after `--` are passed to `lantern daemon` unchanged. Logs go to `$LANTERN_HOME/lantern.log` and `$LANTERN_HOME/lantern.err`. For an encrypted keystore add `--passphrase-file <file>` (the file holds the passphrase, `chmod 600`).

### Option B: hand-written unit

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

`lantern info --network calibration --token-only` prints only the token (same as `cat $LANTERN_HOME/calibration/secrets/token`). In our run the node followed the head within about 30 s of starting. `StateNetworkVersion` must be **29** (or higher after future upgrades): anything lower means a Lantern build that predates the current network upgrade.

## 5. Start Curio-PDP against Lantern

Use the upstream stack from `filecoin-project/curio` unchanged, plus one overlay file from this repo that removes Forest.

```sh
git clone --depth 1 https://github.com/filecoin-project/curio.git
cd curio/docker/skiff
cp /path/to/lantern/deploy/curio-pdp/docker-compose.lantern.yaml .
cp .env .env.upstream
cp /path/to/lantern/deploy/curio-pdp/env.lantern.example .env
```

Newer Curio commits should work the same as long as `docker/skiff` keeps the service names `yugabyte`, `forest` and `skiff`.

Edit `.env` ([deploy/curio-pdp/env.lantern.example](deploy/curio-pdp/env.lantern.example) documents every line). The three lines that matter:

```sh
SKIFF_IMAGE=filecoin/curio-pdp:calibnet
SKIFF_HTTP_DOMAIN=pdp.example.com
FULLNODE_API_INFO=<token from lantern info>:/ip4/172.17.0.1/tcp/1234/http
```

- **Set `SKIFF_HTTP_DOMAIN` before the first start.** It is seeded into the base config layer (`HTTP.DomainName`) once, when the database is first created. If you start with the default `localhost`, you have to change `HTTP.DomainName` in the database config layer later (step 9).
- **`FULLNODE_API_INFO` must use an IP literal after `/ip4/`**: the `docker0` IP. The upstream `.env` and docs suggest `/ip4/host.docker.internal/tcp/1234/http`; `/ip4/` cannot carry a host name, so Curio exits with `unknown url scheme ''` and restarts in a loop.
- Optionally move `YUGABYTE_DATA` / `SKIFF_REPO_DATA` to fast disk and `SKIFF_STORAGE` (mounted as `/data`) to bulk disk.

Start Yugabyte and Curio-PDP only, with all three compose files. Name both services every time, otherwise Compose also starts the Forest service from the base file:

```sh
C="docker compose -f docker-compose.yaml -f docker-compose.calibnet.yaml -f docker-compose.lantern.yaml"
$C pull yugabyte skiff
$C up -d --no-build yugabyte skiff
$C ps
$C logs -f skiff
```

- The first pull fetches `filecoin/curio-pdp:calibnet` and `yugabytedb/yugabyte:2024.1.0.0-b129` (a few minutes).
- Yugabyte turns healthy after about 60 s. **On first start Curio runs its database migrations, which take a few minutes** (about 2 minutes in our run; up to about 8 minutes when Yugabyte is limited to 4 CPUs). The GUI is not usable until they finish. Later starts are fast.
- `$C ps` lists `skiff-yugabyte` and `skiff`, and no `skiff-forest`.
- The skiff log should show no repeated chain errors (`unknown url scheme`, `connection refused`, `no contract code`). A warning about a missing PDP key is expected until step 8.

Confirm the chain connection in the GUI once it is up (step 6): the **Chain** page has a table with one row for your Lantern: **RPC Address** `172.17.0.1:1234`, **Reachability** `ok`, **Sync Status** `ok`, and **Version** showing the Lantern version string. If Reachability says `FAIL`, see Troubleshooting.

To stop the stack: `$C down` (data stays under the paths in `.env`). To update the Curio image: `$C up -d --no-deps --no-build --pull always skiff`.

## 6. Open the admin GUI

The admin GUI listens on `127.0.0.1:4701` on the host (the upstream compose file publishes it on loopback only). It has **no login**: anyone who reaches it can manage keys and config, so never publish port 4701. From your workstation:

```sh
ssh -fN -L 4701:127.0.0.1:4701 <user>@<host>
```

Then open <http://127.0.0.1:4701>. The sidebar has **PDP Overview**, **PDP Proving**, **DataSets**, **Chain**, **Tasks** and **Storage**, then **PDP Guide**, **Wallets** and **Configuration**, and under tools **PDP** and **SQL Console**. The **PDP Guide** page walks through the remaining provider steps (wallet, storage, domain, registration) and shows a status for each; use it as your checklist for steps 7 to 10.

## 7. Attach storage

Pieces are stored under `/data` inside the container, which is `SKIFF_STORAGE` on the host (default `curio/docker/skiff/data`). Create a directory for them:

```sh
docker exec skiff mkdir -p /data/pdp-pieces
```

In the GUI open **Storage** (or the storage step of the **PDP Guide**), enter `/data/pdp-pieces` and attach it. Curio writes a `sectorstore.json` there. The PDP Guide shows "Store-capable paths" and "At least 100 GiB available" as OK, with the free capacity.

## 8. Create and fund the PDP wallet

In the GUI open **Wallets** (PDP wallet section) or the wallet step of the **PDP Guide** and click **Create**. Curio generates a secp256k1 key, stores it in Yugabyte and shows the private key **once**: save it somewhere safe before closing the dialog. Only one PDP key is allowed per Curio cluster. Note the `0x` address.

**How much tFIL:** registering with FWSS costs a **5 FIL registration fee** charged by the service provider registry, plus gas, and every proving period costs a little gas. **Curio also marks the provider unhealthy (`/pdp/ping` returns 503 "Balance for PDP wallet ... is below 5 FIL") whenever the PDP wallet holds less than 5 FIL**, and clients skip unhealthy providers. So fund **at least about 11 tFIL** (5 FIL fee, plus 5 FIL that must stay in the wallet, plus gas) and top up before the balance drops under 5 FIL. On mainnet the same applies in real FIL. The GUI's **Register Provider** button stays disabled until the wallet is funded.

Fund the `0x` address with tFIL from any calibration faucet, for example:

- the official calibration faucets listed at <https://docs.filecoin.io/smart-contracts/developing-contracts/get-test-tokens>
- <https://faucet.reiers.io> (calibration tFIL and USDFC, run by the Lantern maintainer)

Check the balance through your own Lantern (this read is verified locally):

```sh
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["<pdp 0x address>","latest"]}' \
  http://172.17.0.1:1234/rpc/v1
```

The result is the balance in attoFIL as hex (1 FIL = 10^18 attoFIL). The PDP Guide shows the key, its f4 address, the on-chain actor and the balance once the funds land.

## 9. Public HTTPS endpoint

FWSS registration and clients need to reach the PDP API at `https://pdp.example.com`. Curio-PDP serves it on port **80** with `HTTP.DelegateTLS = true` (TLS terminated in front of Curio, the default) or on **443** with Curio's built-in Let's Encrypt TLS (`HTTP.DelegateTLS = false`). Open only 80 and 443 to the internet, never 4701 or 1234.

`HTTP.DomainName` must be the public hostname. If you set `SKIFF_HTTP_DOMAIN` in step 5, it already is.

### Option A: Cloudflare Tunnel (works behind NAT; used in our run)

The domain must be on Cloudflare. In the Cloudflare Zero Trust dashboard, create a tunnel and add a public hostname `pdp.example.com` with service `http://localhost:80`. Then either:

- **From the GUI:** in the **PDP Guide**, the domain step has an optional "Cloudflare Tunnel" item. Paste the tunnel token and click **Download cloudflared & start tunnel**. Curio runs the connector for you.
- **By hand:** run the connector on the host with the tunnel token Cloudflare shows:

```sh
docker run -d --name cloudflared --restart unless-stopped --network host \
  cloudflare/cloudflared:latest tunnel --no-autoupdate run --token <tunnel-token>
```

Keep `HTTP.DelegateTLS = true` (the default); Cloudflare terminates TLS.

### Option B: your own domain and TLS

Point an `A`/`AAAA` record for `pdp.example.com` at the host and open TCP 80 and 443. Then either put your own TLS-terminating reverse proxy in front of port 80 (keep `HTTP.DelegateTLS = true`), or let Curio obtain a Let's Encrypt certificate itself: set `HTTP.DelegateTLS = false` in **Configuration** (on Curio-PDP this opens the base layer editor) and restart skiff with `$C restart skiff`. Option B was not exercised in our run: confirm it with the **PDP Guide** domain step and the `/pdp/ping` check below, and report anything that differs.

### If you started with SKIFF_HTTP_DOMAIN=localhost

The domain lives in the database config layer, not in `.env`: changing `.env` after the first start has no effect. Open **Configuration** in the GUI (base layer), set `HTTP.DomainName` to your hostname, save, then `$C restart skiff`.

### Check

```sh
curl -i https://pdp.example.com/pdp/ping
```

- Before the wallet exists: `503` with `PDP wallet not configured`. That is expected, and it already proves the path from the internet to Curio works.
- With the wallet configured: `200`. Recent Curio builds answer with the body `curio-pdp`.

The **PDP Guide** domain step shows "HTTP.DomainName configured" and "Reachable via /pdp/ping" as OK.

## 10. Register with FWSS

In the GUI open **PDP** (tools section). In the **Filecoin Service Registry** section click **Register Provider** and fill in name, description (**at most 128 characters**, longer is rejected by the registry contract), location (free text, for example `EU-NO`) and storage capacity in GiB (minimum **100**). Submit.

Curio signs the registration transaction with the PDP key and sends it to Lantern (`eth_sendRawTransaction`); Lantern publishes it from its local message pool to the calibration network. The registry charges the 5 FIL fee from step 8. After the transaction lands (a minute or two) the section shows your **provider ID** and the PDP Guide marks "Service provider registry" as OK. Note the provider ID for step 11. You can look the transaction up on a calibration explorer such as <https://calibration.filfox.info> or <https://filecoin-testnet.blockscout.com> (status `1` / Success).

Verify the public endpoint the way FWSS clients see it:

```sh
curl -i https://pdp.example.com/pdp/ping     # 200
```

(`pdptool` from the Curio repository can do the same check, but it is not included in the `filecoin/curio-pdp` image; `curl` is enough.)

## 11. Create a data set as a client

A provider proves only data it stores, so a client has to create a data set and add a piece. [deploy/curio-pdp/upload-test.mjs](deploy/curio-pdp/upload-test.mjs) is a minimal client built on `@filoz/synapse-sdk`. Run it on any machine with Node.js 20+.

1. Create a **separate client wallet** (any secp256k1 key; never the PDP key) and fund it with tFIL (gas) and USDFC (storage payment) from a calibration faucet that offers USDFC (for example <https://faucet.reiers.io>).
2. Run:

```sh
cd deploy/curio-pdp
npm install
CLIENT_PRIVATE_KEY=0x<client key> PROVIDER_ID=<your provider id> node upload-test.mjs
```

The script:

1. creates a storage context for your provider (`createContext({ providerId })`) and prints the client's Filecoin Pay balance;
2. prepares payment: deposits USDFC into Filecoin Pay and approves FWSS if needed (`deposit/approval tx`);
3. uploads a 4 MiB random piece to `https://pdp.example.com` (`stored piece`);
4. waits while **your provider** sends the data set creation transaction (CreateDataSet with the piece added in the same transaction) through Lantern (`add-pieces tx`), and prints the data set ID once it is confirmed (`confirmed: data set <id>`).

In our run the deposit confirmed in about a minute and the provider sent the create transaction about four minutes after the script started; expect the data set ID within roughly 5 to 10 minutes.

**Each data set locks funds in Filecoin Pay.** `prepare` sizes the deposit for one new data set only. If the client wallet already owns data sets (for example from an earlier run), the create transaction can revert with `invariant failure: insufficient funds to cover lockup after function execution`. Deposit more USDFC and run again: `EXTRA_DEPOSIT_USDFC=5 CLIENT_PRIVATE_KEY=... PROVIDER_ID=... node upload-test.mjs` deposits 5 USDFC extra before preparing.

The client uses the public Glif calibration RPC by default (`RPC_URL` overrides it). That is deliberate: the client is not part of the provider stack. On the provider side, every chain read and write goes through Lantern.

## 12. Confirm that proofs land

**Timing.** After the data set is created, Curio initialises proving (`PDPv0_InitPP`) and the first challenge window opens about **one proving period** later. On calibration a proving period is **240 epochs, about 2 hours** (30 s per epoch), with a 20 epoch challenge window. In our run the first proof landed about 2 hours after the data set was created, and then once every 240 epochs.

**On chain, with the script** (read-only, no key):

```sh
node upload-test.mjs status <dataSetId>
# read through your own Lantern instead of Glif:
RPC_URL=http://172.17.0.1:1234/rpc/v1 node upload-test.mjs status <dataSetId>
```

It reads the PDPVerifier contract and prints `live`, the active piece count, the next challenge epoch and the last proven epoch. `last proven epoch` greater than 0 means a proof landed; run it again after the next challenge epoch and it should advance once per proving period. (Through Lantern, the first call can take tens of seconds while cold contract state is fetched and verified.)

**On an explorer.** Open your PDP wallet's `0x` address on <https://calibration.filfox.info> or <https://filecoin-testnet.blockscout.com>: it lists every transaction Curio sent (registration, `provePossession`, `nextProvingPeriod`), each with status `1` / Success. On the PDPVerifier contract (the script prints its address; Logs / Events tab) filter for your data set ID; expect `DataSetCreated`, `PiecesAdded`, `NextProvingPeriod`, then `PossessionProven` every proving period.

**In the Curio GUI.** **DataSets** lists the data set and its pieces, **PDP Proving** shows the proving state per data set, and **Tasks** shows the task history (`PDPv0_InitPP`, `PDPv0_Prove`, `PDPv0_ProvPeriod`).

Record the provider ID, data set ID and these transaction hashes in [docs/VERIFICATION-M2.md](docs/VERIFICATION-M2.md) if you are doing a verification run.

## 13. What success looks like

In order, from registration onwards:

1. **Registration:** the PDP page shows your provider ID; the registration transaction has status `1`.
2. **Data set creation:** after the client upload, Curio sends the create transaction through Lantern, waits for confirmations, and logs `Successfully processed data set create` (`$C logs skiff 2>&1 | grep "processed data set create"`). The data set appears under **DataSets**.
3. **`PDPv0_InitPP`** runs once for the new data set and sends a `nextProvingPeriod` transaction (status `1`).
4. **`PDPv0_Prove`** runs in every challenge window (every 240 epochs on calibration) and sends a `provePossession` transaction, followed by **`PDPv0_ProvPeriod`** scheduling the next period. Each `provePossession` transaction shows status `1` on an explorer, and the `status` mode of the script shows the last proven epoch advancing.
5. **No manual intervention.** In our 24 hour run, two data sets went through 13 consecutive proving windows each; every `provePossession` and `nextProvingPeriod` transaction landed with status `1`, and the Lantern daemon ran the whole time without a restart.

Once registered, **third-party clients may pick your provider** on their own and create data sets on it; that is normal. When a client terminates its service, Curio handles termination and payment settlement automatically (you will see terminate and settle transactions from your PDP wallet). Nothing to do.

## 14. Operating notes

- **Upgrading Lantern:** replace the binary and restart the service (`systemctl --user restart lantern`). Curio reconnects on its own. Since v1.10.1 the persistent transaction index keeps Curio's receipt lookups fast across restarts.
- **Network upgrades** change actor code. A Lantern release that predates an upgrade cannot serve FEVM state after it (see Troubleshooting). Watch the Lantern releases before each Filecoin network upgrade.
- **Bridge-off:** this guide runs Lantern without `--vm-bridge-rpc`, so no chain query is delegated to a third party. `--vm-bridge-rpc <url>` exists as an optional fallback to your own Forest/Lotus; do not add it unless Troubleshooting tells you to, and note that it makes the run not bridge-off.
- **Backups:** the PDP key lives in Yugabyte; back up `YUGABYTE_DATA` (see the Curio docs) as well as the private key you saved in step 8. Lantern's `secrets/` directory is backed up automatically on each start (last 7 kept) and holds only API tokens in this setup.
- **Metrics and dashboard:** <http://127.0.0.1:9092/dashboard/> on the host (SSH-forward port 9092 like the GUI).
- **Gas:** keep the PDP wallet funded. Every proving period costs gas; the PDP Guide shows the balance.

## 15. Troubleshooting

Every row in the first table is an error we hit in the run that produced this guide. The Lantern-side ones are fixed in v1.10.1; if you see them on v1.10.1-rc1 or later, file an operator report.

| Symptom | Cause | Fix |
|---|---|---|
| Curio exits with `unknown url scheme ''` and restarts in a loop | `FULLNODE_API_INFO=/ip4/host.docker.internal/...` (as in the upstream `.env`). `/ip4/` needs an IP literal. | Use `<token>:/ip4/172.17.0.1/tcp/1234/http` (your `docker0` IP), then `$C up -d --no-build yugabyte skiff`. |
| Curio: `GetPDPConfig: no contract code at given address` | Lantern older than NV29 support (v1.10.0 or earlier) serves FEVM contracts as "no code". | Upgrade Lantern to v1.10.1-rc1 or later. `Filecoin.StateNetworkVersion` must return 29. |
| Curio: `DataSetCreated event not found in receipt` or `failed to extract pieces from receipt`; the data set never appears | Lantern before the receipt-logs fix returned receipts without event logs. | Upgrade Lantern to v1.10.1-rc1 or later, then create a new data set (run the client again). |
| Curio: `read tcp ...->172.17.0.1:1234: i/o timeout` on receipt lookups after a Lantern restart | Lantern before the persistent transaction index had to scan the chain for older transactions and was slower than Curio's RPC timeout. | Upgrade Lantern to v1.10.1-rc1 or later. A single timeout followed by a successful retry is harmless. |
| Lantern log: `unsupported opcode 0x5e` | Lantern's local FEVM lacked newer (Cancun) opcodes before #178. | Upgrade Lantern to v1.10.1-rc1 or later. |
| Client: create transaction reverts with `invariant failure: insufficient funds to cover lockup after function execution` | Each data set locks funds in Filecoin Pay; `prepare` only sizes one new data set, and the client already owns others. | Deposit more USDFC into Filecoin Pay (`EXTRA_DEPOSIT_USDFC=<amount>`, step 11) and run again. |
| `init` / `repair`: `quorum not reached` or `fewer countable operators than quorum` | Fewer than 3 distinct operators answered (public endpoints flake, or libp2p is blocked). | Re-run; follow the printed hints (`--peer`, `--no-libp2p`, `--bootstrap-quorum`). `lantern doctor --network calibration` shows which sources fail. |
| `lantern daemon` exits with `no trust anchor at .../bootstrap-anchor.json` | `init` never completed for this network and `LANTERN_HOME` (or `lantern reset --chain-state` removed the anchor). | `lantern init --network calibration --no-wallet` (or `lantern repair --network calibration`) with the same `LANTERN_HOME` as the service, then restart. |
| `https://pdp.example.com/pdp/ping` returns `503 PDP wallet not configured` | Expected before the PDP key exists. | Step 8. |
| PDP Guide domain check fails, `HTTP.DomainName = "localhost"` | `SKIFF_HTTP_DOMAIN` was not set before the first start. | Edit `HTTP.DomainName` in **Configuration** (base layer), `$C restart skiff`. |
| A transaction sent by Curio is not found on a public RPC or explorer for several minutes | Public RPC/explorer lag; the transaction had landed. | Check another explorer, or ask Lantern: `eth_getTransactionReceipt` on `http://172.17.0.1:1234/rpc/v1`. |

Other problems you may run into:

| Symptom | Cause | Fix |
|---|---|---|
| Curio task log: `StorageGCMark` fails with `get finality tipset: chain: header/store: not found` | The node is younger than about 24h. StorageGCMark asks for the tipset about one day (2891 epochs) below head, and Lantern v1.10.1-rc1 and earlier only hold headers from the moment the node was initialised. | Harmless for PDP: storage GC marking just retries on its next run, and the error stops by itself once the node has been up for about a day. Fixed in the next release, which backfills the missing headers on demand (and, on PDP/Full, prefetches the last day at startup). |
| Many on-chain reverts from the PDP wallet (PDPVerifier `Incorrect fee amount`, `DataSetPaymentBeyondEndEpoch`, FilecoinPay `settleRail` exit 7) with Lantern v1.10.1-rc1 or earlier | Those releases answer `eth_estimateGas` with a fixed gas limit without executing the call, so Curio cannot tell that a transaction would revert and broadcasts it anyway (each one burns gas at a limit of about 1.4B). | Upgrade Lantern to the first release after v1.10.1-rc1. Curio then gets the same code-3 revert error from gas estimation that Lotus returns and does not send the transaction. |
| `refusing to bind RPC to non-loopback address "172.17.0.1:1234" without --allow-remote-rpc` | Intentional guard for non-loopback binds. | Add `--allow-remote-rpc` (and keep the bind on `docker0`, never `0.0.0.0`). |
| `init` / `repair`: `conflicting finality answers` | Two sources returned different finalized tipsets for the same F3 instance: one is wrong, broken or on another network. | Do not lower the quorum. Compare against a node you run (`--peer`), re-run later, and file an operator report naming the sources. |
| `lantern daemon` syncs mainnet or `lantern info` says "not initialised" | A command was run without `--network calibration`. | Add `--network calibration` to every command and to the service. |
| Compose rejects `docker-compose.lantern.yaml` (unknown tag / invalid `depends_on`) | Compose older than v2.24.4 does not understand `!override`. | Update the Docker Compose plugin (step 2.1). |
| A `skiff-forest` container is running or downloading a snapshot | `up -d` was run without naming services. | `$C rm -sf forest`, then always `$C up -d --no-build yugabyte skiff`. |
| Curio logs connection refused or timeouts to `172.17.0.1:1234`; Chain page shows Reachability `FAIL` | Lantern not running, bound to another address, or a host firewall drops traffic from the Compose network (for example `ufw` default deny). | `systemctl --user status lantern`, `curl http://172.17.0.1:1234/healthz` on the host, then allow the Docker subnets, for example `sudo ufw allow from 172.16.0.0/12 to any port 1234 proto tcp`. |
| Lantern log: `FEVM method requires --vm-bridge-rpc` | A FEVM read Lantern cannot yet serve locally. | Upgrade to the newest release. If it persists, file an operator report with the log line. Interim only: add `--vm-bridge-rpc https://api.calibration.node.glif.io/rpc/v1` (no longer bridge-off). |
| Lantern service fails at boot with "cannot assign requested address" | `docker0` does not exist yet. | Nothing to do: systemd restarts Lantern until Docker is up. Check `systemctl --user status lantern` after boot. |
| User service stops when you log out | Lingering not enabled. | `sudo loginctl enable-linger $USER`. |
| **Register Provider** button is disabled | PDP key missing or not funded. | Step 8; wait until the balance shows in the PDP Guide. |
| `/pdp/ping` returns 503 `Balance for PDP wallet ... is below 5 FIL` | Curio's health check wants at least 5 FIL in the PDP wallet. Proving continues, but clients treat the provider as unhealthy. | Top up the PDP wallet above 5 FIL (the check refreshes within a few minutes). |
| Registration fails with `description is too long, max 128 characters allowed` | Registry limit on the description field. Nothing was sent, no fee charged. | Shorten the description to 128 characters or fewer and submit again. |
| Unauthenticated `Filecoin.WalletList` returns `null` | Read methods need no token, and the node has no wallets. | Expected. Write, sign and admin methods return `missing permission` without the token. |

Still stuck? File an [operator report](https://github.com/Reiers/lantern/issues/new?template=operator-report.yml) with the step number, the exact error and your versions (redact tokens and keys).
