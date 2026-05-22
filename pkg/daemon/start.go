// start.go: the wiring that brings a Daemon online.
//
// Status: bootstrap implementation. The real implementation will live
// here once cmd/lantern's cmdDaemon body is extracted into pkg/daemon.
// For now, this file provides the minimum API surface external callers
// (Curio Core, integration tests) need today so they can begin building
// against pkg/daemon while the extraction lands incrementally.
//
// The current implementation:
//
//   - validates Config
//   - fetches a TrustedRoot (the load-bearing first step every Lantern
//     daemon does)
//   - exposes that TrustedRoot via Daemon.TrustedRoot()
//   - holds on lifecycle (Start blocks until ctx is cancelled)
//
// Not yet wired (will land in subsequent commits as cmd/lantern's body
// migrates here):
//
//   - JSON-RPC server (rpc/server.New + chainAPI binding)
//   - libp2p host + DHT + gossipsub block ingestor
//   - Bitswap client
//   - VM bridge for AllowBlockSubmit=true
//   - Header store + sync agent
//   - Metrics + dashboard endpoints
//   - Hello + ChainExchange responders
//
// External callers can start building against `daemon.New(cfg).Start(ctx)`
// today; the runtime that Start exposes will grow over subsequent commits
// without changing the public API.

package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/ipfs/go-cid"

	"github.com/Reiers/lantern/chain/f3/subscriber"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/chain/types"
	"github.com/Reiers/lantern/net/glif"
	"github.com/Reiers/lantern/net/hsync"
)

// startInternal is the (currently minimal) bring-up sequence. It will
// grow as cmd/lantern's daemon body migrates into pkg/daemon. Today it
// captures the TrustedRoot, which is the load-bearing first step every
// Lantern daemon does.
func (d *Daemon) startInternal(ctx context.Context) error {
	tr, err := fetchTrustedHead(ctx, d.cfg.Gateway)
	if err != nil {
		return fmt.Errorf("fetch trusted head: %w", err)
	}
	if !d.cfg.EmbeddedMode {
		fmt.Printf("daemon: anchored at epoch %d (state root %s)\n", tr.Epoch, tr.StateRoot)
	}
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()

	// TODO(daemon-extraction): mount RPC server, libp2p, gossipsub,
	// metrics, dashboard, header store, etc. See cmd/lantern/main.go
	// cmdDaemon for the existing wiring.
	return nil
}

// stopInternal performs the (currently minimal) shutdown sequence.
func (d *Daemon) stopInternal(ctx context.Context) error {
	defer close(d.stopped)
	d.mu.Lock()
	d.started = false
	d.mu.Unlock()
	// TODO(daemon-extraction): shutdown the RPC server + close header
	// store + close libp2p host. See cmd/lantern/main.go cmdDaemon's
	// deferred cleanup chain.
	_ = ctx
	return nil
}

// fetchTrustedHead probes the primary gateway's /state/root endpoint,
// falling back to Glif's Filecoin.ChainHead when the gateway is down.
// Both responses are CID-verified before becoming a TrustedRoot.
//
// AcceptedAt is stamped on every TrustedRoot so the dashboard's anchor
// age stat populates. Also attempts a best-effort F3 latest-cert probe
// so F3Instance is populated.
//
// (Mirrored from cmd/lantern/main.go's fetchTrustedHead. Once the full
// extraction lands, cmd/lantern will call this version and the
// duplicate will be removed from cmd/lantern.)
func fetchTrustedHead(ctx context.Context, gw string) (*trustedroot.TrustedRoot, error) {
	now := time.Now().UTC()

	if gw != "" {
		hc := hsync.NewClient([]string{gw}, 5*time.Second)
		if head, err := hc.GetStateHead(ctx); err == nil {
			if stateRoot, e := cid.Parse(head.StateRoot); e == nil {
				tskCids := make([]cid.Cid, 0, len(head.TipsetKey))
				for _, s := range head.TipsetKey {
					if c, e := cid.Parse(s); e == nil {
						tskCids = append(tskCids, c)
					}
				}
				pw, _ := big.FromString(head.ParentWeight)
				tr := &trustedroot.TrustedRoot{
					Epoch:        abi.ChainEpoch(head.Epoch),
					StateRoot:    stateRoot,
					TipSetKey:    types.NewTipSetKey(tskCids...),
					ParentWeight: pw,
					AcceptedAt:   now,
				}
				attachF3Latest(ctx, tr)
				return tr, nil
			}
		}
	}

	// Fallback to Glif when the gateway is unreachable or returned an
	// unparseable head.
	gc := glif.New("", 10*time.Second)
	gh, err := gc.FetchHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("both gateway and Glif failed: %w", err)
	}
	tr := &trustedroot.TrustedRoot{
		Epoch:                 gh.Epoch,
		StateRoot:             gh.StateRoot,
		TipSetKey:             gh.TipSetKey,
		ParentWeight:          gh.ParentWeight,
		ParentMessageReceipts: gh.ParentMessageReceipts,
		AcceptedAt:            now,
	}
	attachF3Latest(ctx, tr)
	return tr, nil
}

// attachF3Latest does a best-effort Filecoin.F3GetLatestCertificate
// probe against Glif so observability surfaces (dashboard, logs) can
// render F3 instance. Failures are silent.
func attachF3Latest(ctx context.Context, tr *trustedroot.TrustedRoot) {
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	src := subscriber.NewJSONRPCSource("https://api.node.glif.io/rpc/v1")
	cert, err := src.GetLatest(probeCtx)
	if err != nil || cert == nil {
		return
	}
	tr.F3Instance = cert.GPBFTInstance
	tr.F3Cert = cert
}
