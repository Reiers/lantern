#!/usr/bin/env python3
import json
import urllib.request

T = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJBbGxvdyI6WyJyZWFkIiwid3JpdGUiLCJzaWduIiwiYWRtaW4iXX0.S-Q1wlavC6MwswepDL0OTCF5TPLR3SKRTOlVcrddmcM"

def call(method, params=None):
    body = json.dumps({"jsonrpc":"2.0","method":f"Filecoin.{method}","params":params,"id":1}).encode()
    req = urllib.request.Request("http://127.0.0.1:11234/rpc/v1", data=body,
                                 headers={"Authorization":"Bearer "+T, "Content-Type":"application/json"})
    return json.loads(urllib.request.urlopen(req, timeout=10).read())

r = call("NetPeers")
peers = r.get("result", []) or []
print("NetPeers:", len(peers), "peers")
for p in peers[:8]:
    print(f"   id={p['ID'][:20]}...  addrs={len(p.get('Addrs') or [])}")

print()
print("NetBandwidthStats:", call("NetBandwidthStats")["result"])
print("NetAutoNatStatus: ", call("NetAutoNatStatus")["result"])
print("NetListening:     ", call("NetListening")["result"])

ch = call("ChainHead")["result"]
print("ChainHead epoch:  ", ch.get("Height"))
print("EthBlockNumber:   ", call("EthBlockNumber")["result"])
