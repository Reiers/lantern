#!/usr/bin/env node
// Minimal Filecoin Onchain Cloud client for testing a Lantern-backed
// Curio-PDP provider on calibration. It is the client side only: it talks to
// your provider over HTTPS and to the chain through RPC_URL. Your provider
// itself (Curio-PDP) talks only to Lantern.
//
//   npm install
//
//   # 1. Create a data set on your provider and upload one random piece:
//   CLIENT_PRIVATE_KEY=0x<64 hex> PROVIDER_ID=<your provider id> node upload-test.mjs
//
//   # 2. Watch proving for that data set (read-only, no key needed):
//   node upload-test.mjs status <dataSetId>
//
// Environment:
//   CLIENT_PRIVATE_KEY  0x-prefixed secp256k1 key of a CLIENT wallet funded with
//                       tFIL (gas) and USDFC (storage payment). Never use the
//                       provider's PDP key here.
//   PROVIDER_ID         Your provider ID from the FWSS registration.
//   RPC_URL             Calibration JSON-RPC for the client (default: public Glif).
//                       For "status" you can point it at your own Lantern, e.g.
//                       http://172.17.0.1:1234/rpc/v1, to read through it.
//   SIZE_BYTES          Size of the random test piece (default 4194304 = 4 MiB).
//   EXTRA_DEPOSIT_USDFC Optional. Deposit this many USDFC into Filecoin Pay
//                       before preparing (for example 5). Each data set locks
//                       funds and "prepare" only sizes one new data set, so a
//                       client that already owns data sets may need more; the
//                       symptom is a create tx reverting with "insufficient
//                       funds to cover lockup after function execution".
//
// Requires Node.js 20 or newer.

import { Synapse, calibration, formatUnits, parseUnits } from '@filoz/synapse-sdk'
import { createPublicClient, http } from 'viem'
import { privateKeyToAccount } from 'viem/accounts'
import { randomBytes } from 'node:crypto'

const RPC_URL = process.env.RPC_URL || calibration.rpcUrls.default.http[0]
const log = (...a) => console.log(new Date().toISOString(), ...a)
const json = (v) => JSON.stringify(v, (_k, x) => (typeof x === 'bigint' ? String(x) : x))

function die(msg) {
  console.error('error: ' + msg)
  console.error('usage: CLIENT_PRIVATE_KEY=0x... PROVIDER_ID=<id> node upload-test.mjs')
  console.error('       node upload-test.mjs status <dataSetId>')
  process.exit(2)
}

async function status(arg) {
  if (!/^\d+$/.test(arg || '')) die('status needs a numeric data set id')
  const id = BigInt(arg)
  const client = createPublicClient({ chain: calibration, transport: http(RPC_URL) })
  const pdp = calibration.contracts.pdp
  const read = (functionName) =>
    client.readContract({ address: pdp.address, abi: pdp.abi, functionName, args: [id] })

  const head = await client.getBlockNumber()
  log('rpc', RPC_URL)
  log('PDPVerifier', pdp.address)
  log('chain head epoch', String(head))
  const live = await read('dataSetLive')
  log('data set', String(id), 'live', live)
  if (!live) return
  const [pieces, next, last] = await Promise.all([
    read('getActivePieceCount'),
    read('getNextChallengeEpoch'),
    read('getDataSetLastProvenEpoch'),
  ])
  log('active pieces', String(pieces))
  log('next challenge epoch', String(next), next > head ? '(in ' + String(next - head) + ' epochs)' : '(passed ' + String(head - next) + ' epochs ago)')
  log('last proven epoch', String(last), last > 0n ? '(' + String(head - last) + ' epochs ago)' : '')
  if (last > 0n) {
    log('OK: at least one proof has landed for this data set. Run again after the next challenge epoch: the last proven epoch should advance every proving period.')
    if (next + 240n < head) log('WARNING: the next challenge epoch is far in the past, so proving looks stalled or late. Check the Curio PDP pages and logs.')
  } else {
    log('No proof yet. The first PossessionProven lands after the first challenge epoch; run this again later.')
  }
}

async function upload() {
  const pk = process.env.CLIENT_PRIVATE_KEY || ''
  if (!/^0x[0-9a-fA-F]{64}$/.test(pk)) die('set CLIENT_PRIVATE_KEY to a 0x-prefixed 32-byte hex key')
  const providerArg = process.env.PROVIDER_ID || ''
  if (!/^\d+$/.test(providerArg)) die('set PROVIDER_ID to your numeric provider id')
  const size = Number(process.env.SIZE_BYTES || 4 * 1024 * 1024)
  if (!Number.isSafeInteger(size) || size <= 0) die('SIZE_BYTES must be a positive integer')

  const account = privateKeyToAccount(pk)
  const synapse = Synapse.create({
    account,
    chain: calibration,
    transport: http(RPC_URL),
    source: 'lantern-pdp-upload-test',
  })
  log('client', account.address, 'rpc', RPC_URL)

  const ctx = await synapse.storage.createContext({
    providerId: BigInt(providerArg),
    metadata: { purpose: 'lantern-pdp-upload-test' },
  })
  log('provider', String(ctx.provider?.id), ctx.provider?.name, 'existing dataSetId', String(ctx.dataSetId))

  const decimals = synapse.payments.decimals()
  const showPay = async (label) =>
    log(label, 'Filecoin Pay available', formatUnits(await synapse.payments.balance(), decimals), 'USDFC; wallet', formatUnits(await synapse.payments.walletBalance(), decimals), 'USDFC')
  await showPay('before:')

  const extra = process.env.EXTRA_DEPOSIT_USDFC || ''
  if (extra) {
    if (!/^\d+(\.\d+)?$/.test(extra)) die('EXTRA_DEPOSIT_USDFC must be a decimal amount, e.g. 5')
    const hash = await synapse.payments.deposit({
      amount: parseUnits(extra, decimals),
      onApprovalTransaction: (h) => log('USDFC approval tx', h),
    })
    log('extra deposit tx', hash)
    const pub = createPublicClient({ chain: calibration, transport: http(RPC_URL) })
    const rcpt = await pub.waitForTransactionReceipt({ hash })
    log('extra deposit status', rcpt.status)
    await showPay('after extra deposit:')
  }

  const data = randomBytes(size)
  const prep = await synapse.storage.prepare({ context: ctx, pieceSizes: [BigInt(size)] })
  log('prepare: ready', prep.costs?.ready, 'deposit needed', String(prep.transaction?.depositAmount ?? 0n))
  if (prep.transaction) {
    const r = await prep.transaction.execute({ onHash: (h) => log('deposit/approval tx', h) })
    log('funded', json(r).slice(0, 300))
  }

  let dataSetId = ctx.dataSetId
  const res = await ctx.upload(data, {
    onStored: (_providerId, piece) => log('stored piece', String(piece)),
    onPiecesAdded: (tx) => log('add-pieces tx (sent by your provider through Lantern)', tx),
    onPiecesConfirmed: (ds, _providerId, pieces) => {
      dataSetId = ds
      log('confirmed: data set', String(ds), json(pieces).slice(0, 300))
    },
  })
  log('result', json(res).slice(0, 1500))
  log('PDPVerifier', calibration.contracts.pdp.address)
  if (dataSetId !== undefined && dataSetId !== null) {
    log('next: node upload-test.mjs status ' + String(dataSetId))
  }
}

const [cmd, arg] = process.argv.slice(2)
try {
  if (cmd === 'status') await status(arg)
  else if (cmd === undefined) await upload()
  else die('unknown command ' + cmd)
} catch (err) {
  console.error(err)
  process.exit(1)
}
