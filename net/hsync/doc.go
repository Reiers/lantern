// Package hsync is Lantern's HTTPS gateway client.
//
// A "Lantern gateway" is a thin proxy node that translates Lantern's
// `GET /block/{cid}` calls into upstream Bitswap fetches or Filecoin RPC
// reads. The client never trusts the gateway's bytes; every fetched block
// is re-hashed against the requested CID before insertion into the cache.
//
// See cmd/lantern-gateway for the server side.
package hsync
