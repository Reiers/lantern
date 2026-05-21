// Higher-level actor loaders bound to an Accessor. These are convenience
// wrappers that resolve `target` → ID → Actor → versioned state handle.

package accessor

import (
	"context"
	"fmt"

	addr "github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/state/actors"
)

// Registry returns the (lazy) singleton actor-code registry.
var defaultRegistry = actors.DefaultRegistry()

// Registry returns the actor-code registry used by the loaders.
func (a *Accessor) Registry() *actors.Registry { return defaultRegistry }

// LoadMiner resolves `target` to a miner actor and returns the versioned
// MinerState handle plus the proof path used to fetch the actor.
func (a *Accessor) LoadMiner(ctx context.Context, target addr.Address) (actors.MinerState, []cid.Cid, error) {
	actor, proof, err := a.GetActor(ctx, target)
	if err != nil {
		return nil, proof, fmt.Errorf("loading actor %s: %w", target, err)
	}
	ms, err := actors.LoadMiner(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return ms, append(proof, actor.Head), err
}

// LoadPower returns the power actor's state handle. Always at f04.
func (a *Accessor) LoadPower(ctx context.Context) (actors.PowerState, []cid.Cid, error) {
	actor, proof, err := a.GetActorByID(ctx, actors.PowerActorAddress)
	if err != nil {
		return nil, proof, fmt.Errorf("loading power actor: %w", err)
	}
	ps, err := actors.LoadPower(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return ps, append(proof, actor.Head), err
}

// LoadMarket returns the market actor's state handle. Always at f05.
func (a *Accessor) LoadMarket(ctx context.Context) (actors.MarketState, []cid.Cid, error) {
	actor, proof, err := a.GetActorByID(ctx, actors.MarketActorAddress)
	if err != nil {
		return nil, proof, fmt.Errorf("loading market actor: %w", err)
	}
	ms, err := actors.LoadMarket(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return ms, append(proof, actor.Head), err
}

// LoadVerifreg returns the verifreg actor's state handle. Always at f06.
func (a *Accessor) LoadVerifreg(ctx context.Context) (actors.VerifregState, []cid.Cid, error) {
	actor, proof, err := a.GetActorByID(ctx, actors.VerifregActorAddress)
	if err != nil {
		return nil, proof, fmt.Errorf("loading verifreg actor: %w", err)
	}
	vs, err := actors.LoadVerifreg(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return vs, append(proof, actor.Head), err
}

// LoadDatacap returns the datacap actor's state handle. Always at f07.
func (a *Accessor) LoadDatacap(ctx context.Context) (actors.DatacapState, []cid.Cid, error) {
	actor, proof, err := a.GetActorByID(ctx, actors.DatacapActorAddress)
	if err != nil {
		return nil, proof, fmt.Errorf("loading datacap actor: %w", err)
	}
	ds, err := actors.LoadDatacap(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return ds, append(proof, actor.Head), err
}

// LoadReward returns the reward actor's state handle. Always at f02.
func (a *Accessor) LoadReward(ctx context.Context) (actors.RewardState, []cid.Cid, error) {
	actor, proof, err := a.GetActorByID(ctx, actors.RewardActorAddress)
	if err != nil {
		return nil, proof, fmt.Errorf("loading reward actor: %w", err)
	}
	rs, err := actors.LoadReward(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return rs, append(proof, actor.Head), err
}

// LoadInit returns the init actor's state handle. Always at f01.
func (a *Accessor) LoadInit(ctx context.Context) (actors.InitState, []cid.Cid, error) {
	actor, proof, err := a.GetActorByID(ctx, actors.InitActorAddress)
	if err != nil {
		return nil, proof, fmt.Errorf("loading init actor: %w", err)
	}
	is, err := actors.LoadInit(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return is, append(proof, actor.Head), err
}

// LoadSystem returns the system actor's state handle. Always at f00.
func (a *Accessor) LoadSystem(ctx context.Context) (actors.SystemState, []cid.Cid, error) {
	actor, proof, err := a.GetActorByID(ctx, actors.SystemActorAddress)
	if err != nil {
		return nil, proof, fmt.Errorf("loading system actor: %w", err)
	}
	ss, err := actors.LoadSystem(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return ss, append(proof, actor.Head), err
}

// LoadAccount loads any account actor by address.
func (a *Accessor) LoadAccount(ctx context.Context, target addr.Address) (actors.AccountState, []cid.Cid, error) {
	actor, proof, err := a.GetActor(ctx, target)
	if err != nil {
		return nil, proof, fmt.Errorf("loading actor %s: %w", target, err)
	}
	as, err := actors.LoadAccount(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return as, append(proof, actor.Head), err
}

// LoadMultisig loads any multisig actor by address.
func (a *Accessor) LoadMultisig(ctx context.Context, target addr.Address) (actors.MultisigState, []cid.Cid, error) {
	actor, proof, err := a.GetActor(ctx, target)
	if err != nil {
		return nil, proof, fmt.Errorf("loading actor %s: %w", target, err)
	}
	ms, err := actors.LoadMultisig(ctx, actor.Code, actor.Head, a.bg, defaultRegistry)
	return ms, append(proof, actor.Head), err
}
