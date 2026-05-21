// Package combined wires multiple BlockGetters into a single fall-through
// chain: cache → bitswap → HTTP gateway. Successful fetches from upstream
// sources are written back into the local cache.
//
// This is the BlockGetter the state accessor takes in production.
package combined
