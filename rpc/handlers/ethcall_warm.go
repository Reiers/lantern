package handlers

// Head warming for the eth_call burst caches.
//
// A cold PDPVerifier view call on a fresh head is a chain of ~20
// dependent block fetches: state tree path to the proxy, its EVM head,
// the KAMT path to the implementation slot, the state tree path to the
// implementation, then the KAMT paths to the mapping slots the view
// reads. Each link waits a network round trip, and the chain cannot be
// shortened from inside one call because the interpreter only learns the
// next address/slot from the previous read.
//
// Across heads, though, the SAME contracts and slots are read again and
// again (Curio walks the same data sets every epoch). So the warmer
// records which actors and storage slots live-head eth_calls touched, and
// when a new head lands it re-reads all of them at the new state root
// concurrently: every actor resolution in parallel, then every slot of
// every contract in parallel. The chain collapses to roughly
// state-tree depth + 1 + KAMT depth round trips, and it starts the moment
// the head is adopted, before the clients' bursts arrive. Results land in
// the content/state-keyed caches of ethcall_perf.go, so a warmed value is
// exactly what the interpreter would have read itself; nothing here can
// serve a stale value.

import (
	"context"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"github.com/Reiers/lantern/chain/trustedroot"
	ltypes "github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/state/accessor"
	"github.com/Reiers/lantern/state/actors"
	"github.com/Reiers/lantern/state/kamt"
	"github.com/Reiers/lantern/vm/evm"
)

const (
	warmMaxActors   = 128              // distinct actors kept in the warm set
	warmMaxSlots    = 8192             // distinct (contract, slot) pairs kept
	warmKeepEpochs  = 20               // drop entries not read for this many epochs
	warmParallelism = 32               // concurrent reads per warm pass
	warmTimeout     = 25 * time.Second // bound on one warm pass
)

type warmSlotKey struct {
	addr evm.Address
	slot [32]byte
}

// headWarmer records live-head reads and replays them on new heads.
type headWarmer struct {
	mu        sync.Mutex
	actors    map[evm.Address]int64 // contract -> last epoch read
	slots     map[warmSlotKey]int64 // (contract, slot) -> last epoch read
	cancel    context.CancelFunc    // cancels the in-flight pass
	lastEpoch int64
}

func newHeadWarmer() *headWarmer {
	return &headWarmer{actors: map[evm.Address]int64{}, slots: map[warmSlotKey]int64{}}
}

func (w *headWarmer) recordActor(a evm.Address, epoch int64) {
	w.mu.Lock()
	if _, ok := w.actors[a]; ok || len(w.actors) < warmMaxActors {
		w.actors[a] = epoch
	}
	w.mu.Unlock()
}

func (w *headWarmer) recordSlot(a evm.Address, slot [32]byte, epoch int64) {
	k := warmSlotKey{addr: a, slot: slot}
	w.mu.Lock()
	if _, ok := w.slots[k]; ok || len(w.slots) < warmMaxSlots {
		w.slots[k] = epoch
	}
	w.mu.Unlock()
}

// snapshot prunes entries not read in the last warmKeepEpochs and returns
// the remaining slots grouped by contract (actors without slots map to an
// empty list).
func (w *headWarmer) snapshot(epoch int64) map[evm.Address][][32]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[evm.Address][][32]byte, len(w.actors))
	for a, last := range w.actors {
		if epoch-last > warmKeepEpochs {
			delete(w.actors, a)
			continue
		}
		out[a] = nil
	}
	for k, last := range w.slots {
		if epoch-last > warmKeepEpochs {
			delete(w.slots, k)
			continue
		}
		out[k.addr] = append(out[k.addr], k.slot)
	}
	return out
}

// onHead starts a warm pass for head, cancelling any pass still running
// for an older head. Called from the header store's head listener, so it
// must not block.
func (c *ChainAPI) warmOnHead(p *ethCallPerf, head *ltypes.TipSet) {
	if head == nil || !head.ParentState().Defined() {
		return
	}
	w := p.warm
	plan := w.snapshot(int64(head.Height()))
	if len(plan) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.cancel = cancel
	w.mu.Unlock()
	go func() {
		defer cancel()
		c.warmPass(ctx, p, head, plan)
	}()
}

// warmPass reads every planned actor and slot at head's execution state.
// Actors and slots use separate semaphores: an actor goroutine holds its
// slot while it queues slot reads, so sharing one would deadlock.
func (c *ChainAPI) warmPass(ctx context.Context, p *ethCallPerf, head *ltypes.TipSet, plan map[evm.Address][][32]byte) {
	root := head.ParentState()
	bg := newRetryingBlockGetter(p.blockGetter(), 2, 8*time.Second)
	acc := accessor.New(&trustedroot.TrustedRoot{Epoch: head.Height(), StateRoot: root}, bg)
	reg := acc.Registry()
	actorSem := make(chan struct{}, warmParallelism/2)
	slotSem := make(chan struct{}, warmParallelism)
	var wg sync.WaitGroup
	run := func(sem chan struct{}, f func()) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			f()
		}()
	}
	for a, slots := range plan {
		a, slots := a, slots
		run(actorSem, func() {
			filAddr, err := ethAddrToFilecoin(a[:])
			if err != nil {
				return
			}
			actor, err := p.getActorCached(ctx, acc, root, filAddr)
			if err != nil || actor == nil {
				return
			}
			sk := evmStateKey{code: actor.Code, head: actor.Head}
			st, ok := p.evmStates.Get(sk)
			if !ok {
				st, err = actors.LoadEVM(ctx, actor.Code, actor.Head, bg, reg)
				if err != nil {
					return
				}
				p.evmStates.Add(sk, st)
			}
			if bc := st.BytecodeCID(); bc.Defined() {
				if _, ok := p.code.Get(bc); !ok {
					if code, err := actors.FetchBytecode(ctx, st, bg); err == nil {
						p.code.Add(bc, code)
					}
				}
			}
			storageRoot := st.StorageRoot()
			if !storageRoot.Defined() {
				return
			}
			for _, slot := range slots {
				slot := slot
				key := slotCacheKey{root: storageRoot, slot: slot}
				if _, ok := p.slots.Get(key); ok {
					continue
				}
				run(slotSem, func() {
					var k uint256.Int
					k.SetBytes32(slot[:])
					v, _, err := kamt.GetU256(ctx, storageRoot, k.ToBig(), bg)
					if err != nil {
						return
					}
					var out uint256.Int
					out.SetFromBig(v)
					p.slots.Add(key, out)
					p.warmedSlots.Add(1)
				})
			}
		})
	}
	// Slot reads are queued by the actor goroutines; wait for both waves.
	wg.Wait()
	p.warmPasses.Add(1)
}
