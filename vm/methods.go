// Built-in actor method dispatch for Lantern's VM shell.
//
// We import the public `Methods` map from each
// go-state-types/builtin/v{17,18}/<actor> package, then dispatch a
// (codeCID, methodNum) tuple to the corresponding method metadata. We
// don't *execute* the method; we just look up its name, parameter type,
// and return type — that's enough to (a) decode the message params for
// logging/trace, (b) compute gas for an estimate, (c) produce a synthetic
// receipt with the correct return-value shape.

package vm

import (
	"context"
	"fmt"

	addr "github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/builtin"

	account17 "github.com/filecoin-project/go-state-types/builtin/v17/account"
	cron17 "github.com/filecoin-project/go-state-types/builtin/v17/cron"
	datacap17 "github.com/filecoin-project/go-state-types/builtin/v17/datacap"
	eam17 "github.com/filecoin-project/go-state-types/builtin/v17/eam"
	ethaccount17 "github.com/filecoin-project/go-state-types/builtin/v17/ethaccount"
	evm17 "github.com/filecoin-project/go-state-types/builtin/v17/evm"
	initact17 "github.com/filecoin-project/go-state-types/builtin/v17/init"
	market17 "github.com/filecoin-project/go-state-types/builtin/v17/market"
	miner17 "github.com/filecoin-project/go-state-types/builtin/v17/miner"
	multisig17 "github.com/filecoin-project/go-state-types/builtin/v17/multisig"
	paych17 "github.com/filecoin-project/go-state-types/builtin/v17/paych"
	power17 "github.com/filecoin-project/go-state-types/builtin/v17/power"
	reward17 "github.com/filecoin-project/go-state-types/builtin/v17/reward"
	system17 "github.com/filecoin-project/go-state-types/builtin/v17/system"
	verifreg17 "github.com/filecoin-project/go-state-types/builtin/v17/verifreg"

	account18 "github.com/filecoin-project/go-state-types/builtin/v18/account"
	cron18 "github.com/filecoin-project/go-state-types/builtin/v18/cron"
	datacap18 "github.com/filecoin-project/go-state-types/builtin/v18/datacap"
	eam18 "github.com/filecoin-project/go-state-types/builtin/v18/eam"
	ethaccount18 "github.com/filecoin-project/go-state-types/builtin/v18/ethaccount"
	evm18 "github.com/filecoin-project/go-state-types/builtin/v18/evm"
	initact18 "github.com/filecoin-project/go-state-types/builtin/v18/init"
	market18 "github.com/filecoin-project/go-state-types/builtin/v18/market"
	miner18 "github.com/filecoin-project/go-state-types/builtin/v18/miner"
	multisig18 "github.com/filecoin-project/go-state-types/builtin/v18/multisig"
	paych18 "github.com/filecoin-project/go-state-types/builtin/v18/paych"
	power18 "github.com/filecoin-project/go-state-types/builtin/v18/power"
	reward18 "github.com/filecoin-project/go-state-types/builtin/v18/reward"
	system18 "github.com/filecoin-project/go-state-types/builtin/v18/system"
	verifreg18 "github.com/filecoin-project/go-state-types/builtin/v18/verifreg"

	"github.com/Reiers/lantern/state/actors"
)

// MethodInfo is the resolved metadata for one built-in method.
type MethodInfo struct {
	Kind    actors.Kind
	Version int
	Method  abi.MethodNum
	Meta    builtin.MethodMeta
}

// ResolveMethod looks up the (kind, version, method) -> MethodMeta.
//
// Returns an error if either the actor version is unsupported, or the
// method number is not registered for that actor.
func ResolveMethod(_ context.Context, kind actors.Kind, version int, m abi.MethodNum) (*MethodInfo, error) {
	tbl, err := methodTable(kind, version)
	if err != nil {
		return nil, err
	}
	meta, ok := tbl[m]
	if !ok {
		return nil, fmt.Errorf("method %d not registered for %s v%d", m, kind, version)
	}
	return &MethodInfo{Kind: kind, Version: version, Method: m, Meta: meta}, nil
}

// methodTable returns the Methods map for a given (kind, version).
// Currently we support v17 (nv25) and v18 (nv26) — the active mainnet
// actor versions as of 2026-05.
func methodTable(kind actors.Kind, version int) (map[abi.MethodNum]builtin.MethodMeta, error) {
	switch version {
	case 17:
		switch kind {
		case actors.KindAccount:
			return account17.Methods, nil
		case actors.KindCron:
			return cron17.Methods, nil
		case actors.KindInit:
			return initact17.Methods, nil
		case actors.KindMarket:
			return market17.Methods, nil
		case actors.KindMiner:
			return miner17.Methods, nil
		case actors.KindMultisig:
			return multisig17.Methods, nil
		case actors.KindPaych:
			return paych17.Methods, nil
		case actors.KindPower:
			return power17.Methods, nil
		case actors.KindReward:
			return reward17.Methods, nil
		case actors.KindSystem:
			return system17.Methods, nil
		case actors.KindVerifreg:
			return verifreg17.Methods, nil
		case actors.KindDatacap:
			return datacap17.Methods, nil
		case actors.KindEvm:
			return evm17.Methods, nil
		case actors.KindEam:
			return eam17.Methods, nil
		case actors.KindEthAccount:
			return ethaccount17.Methods, nil
		case actors.KindPlaceholder:
			// Placeholder actors only support method 0 (Send) and that
			// path is handled outside the table.
			return map[abi.MethodNum]builtin.MethodMeta{}, nil
		}
	case 18:
		switch kind {
		case actors.KindAccount:
			return account18.Methods, nil
		case actors.KindCron:
			return cron18.Methods, nil
		case actors.KindInit:
			return initact18.Methods, nil
		case actors.KindMarket:
			return market18.Methods, nil
		case actors.KindMiner:
			return miner18.Methods, nil
		case actors.KindMultisig:
			return multisig18.Methods, nil
		case actors.KindPaych:
			return paych18.Methods, nil
		case actors.KindPower:
			return power18.Methods, nil
		case actors.KindReward:
			return reward18.Methods, nil
		case actors.KindSystem:
			return system18.Methods, nil
		case actors.KindVerifreg:
			return verifreg18.Methods, nil
		case actors.KindDatacap:
			return datacap18.Methods, nil
		case actors.KindEvm:
			return evm18.Methods, nil
		case actors.KindEam:
			return eam18.Methods, nil
		case actors.KindEthAccount:
			return ethaccount18.Methods, nil
		case actors.KindPlaceholder:
			return map[abi.MethodNum]builtin.MethodMeta{}, nil
		}
	}
	return nil, fmt.Errorf("unsupported actor: kind=%s version=%d", kind, version)
}

// String returns a human-readable description: "<kind>.<MethodName>".
func (mi *MethodInfo) String() string {
	return fmt.Sprintf("%s.%s", mi.Kind, mi.Meta.Name)
}

// ZeroSenderAddr is convenience for tests; not used internally.
var ZeroSenderAddr addr.Address
