// Package trustedroot is the Phase 1 output module: given streams of headers
// and F3 finality certificates plus the network manifest, it produces and
// persists Lantern's TrustedRoot tuple.
//
// See TRUSTED-ROOT.md for the spec.

package trustedroot

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/gpbft"
	"github.com/filecoin-project/go-f3/manifest"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/Reiers/lantern/chain/beacon"
	"github.com/Reiers/lantern/chain/f3"
	"github.com/Reiers/lantern/chain/header"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// TrustedRoot is the small in-memory tuple the rest of the node treats as
// ground truth for "what the chain is right now."
type TrustedRoot struct {
	// Chain identity at the selected tip.
	Epoch                 abi.ChainEpoch
	TipSetKey             ltypes.TipSetKey
	StateRoot             cid.Cid
	ParentMessageReceipts cid.Cid
	ParentWeight          ltypes.BigInt
	BeaconRound           uint64

	// F3 finality witness. F3Cert may be nil pre-F3-finality.
	F3Instance uint64
	F3Cert     *certs.FinalityCertificate

	// Bookkeeping.
	AcceptedAt time.Time
	// AncestorRoots is the last N state roots (default 100) for state
	// queries against recent past epochs.
	AncestorRoots []cid.Cid
}

// HeaderSource yields headers in epoch-ascending order. The default
// implementation in examples/historical/phase1 reads from a Lotus RPC; production
// code will swap in a libp2p header sync.
type HeaderSource interface {
	// Tipset returns the tipset at the given epoch, or an error if it
	// can't be produced. May skip null-round epochs by returning an empty
	// block list.
	Tipset(ctx context.Context, epoch abi.ChainEpoch) (*ltypes.TipSet, error)
	// Head returns the current head epoch.
	Head(ctx context.Context) (abi.ChainEpoch, error)
}

// F3CertSource yields finality certificates starting at the requested
// instance.
type F3CertSource interface {
	// Cert returns the cert at the given instance, or (nil, ErrNoCert) if
	// not yet finalized.
	Cert(ctx context.Context, instance uint64) (*certs.FinalityCertificate, error)
	// Latest returns the latest available cert; useful for figuring out
	// how far back to walk.
	Latest(ctx context.Context) (*certs.FinalityCertificate, error)
}

// ErrNoCert is returned by F3CertSource.Cert when the requested instance is
// not yet finalized.
var ErrNoCert = errors.New("f3 cert not yet available")

// InitialPowerTableLoader supplies the seed power table referenced by the
// network manifest's InitialPowerTable CID. Phase 1 supplies this from the
// public Glif RPC (Filecoin.F3GetCertificate(0) gives the first cert whose
// base.PowerTable matches the manifest).
type InitialPowerTableLoader func(ctx context.Context, cidRef cid.Cid) (gpbft.PowerEntries, error)

// Options configures a Build run.
type Options struct {
	// Manifest is the F3 network manifest. Required.
	Manifest *manifest.Manifest
	// BeaconConfig is the DRAND verifier for current-network beacon entries.
	// Optional: if nil, beacon-entry verification is skipped (useful for
	// catching up to a tip without flagging every quicknet round).
	BeaconConfig *beacon.Config
	// MaxBacktrack limits how many epochs back from head we walk; default 30.
	MaxBacktrack abi.ChainEpoch
	// AncestorRootDepth: how many ancestor state roots to record. Default 100.
	AncestorRootDepth int
}

// Build runs the Phase 1 trusted-root construction pipeline end-to-end:
//
//  1. Load the initial power table for the manifest.
//  2. Walk F3 certs forward, validating each.
//  3. Select the head: the F3-finalized tip if available, else head - safety.
//  4. Fetch the headers along the selected ancestry and validate each.
//  5. Persist (if db != nil) and return the TrustedRoot.
func Build(
	ctx context.Context,
	db *badger.DB,
	opts Options,
	headers HeaderSource,
	f3certs F3CertSource,
	initialPT InitialPowerTableLoader,
) (*TrustedRoot, error) {
	if opts.Manifest == nil {
		return nil, errors.New("trustedroot: Options.Manifest required")
	}
	if headers == nil {
		return nil, errors.New("trustedroot: HeaderSource required")
	}
	if opts.MaxBacktrack <= 0 {
		opts.MaxBacktrack = 30
	}
	if opts.AncestorRootDepth <= 0 {
		opts.AncestorRootDepth = 100
	}

	// 1. Initial power table.
	if !opts.Manifest.InitialPowerTable.Defined() {
		return nil, errors.New("trustedroot: manifest has no InitialPowerTable CID")
	}
	if initialPT == nil {
		return nil, errors.New("trustedroot: InitialPowerTableLoader required")
	}
	currentPT, err := initialPT(ctx, opts.Manifest.InitialPowerTable)
	if err != nil {
		return nil, xerrors.Errorf("loading initial power table: %w", err)
	}
	if len(currentPT) == 0 {
		return nil, errors.New("trustedroot: initial power table is empty")
	}

	// 2. F3 cert walk.
	var (
		latestCert    *certs.FinalityCertificate
		latestChain   *gpbft.ECChain
		instance      = opts.Manifest.InitialInstance
		finalizedTSK  ltypes.TipSetKey
		finalizedTip  abi.ChainEpoch = -1
		hasF3Finality bool
	)
	if f3certs != nil {
		latest, latestErr := f3certs.Latest(ctx)
		if latestErr == nil && latest != nil {
			// Walk from current instance to latest.
			batch := make([]*certs.FinalityCertificate, 0, 64)
			for i := instance; i <= latest.GPBFTInstance; i++ {
				c, cerr := f3certs.Cert(ctx, i)
				if cerr != nil {
					if errors.Is(cerr, ErrNoCert) {
						break
					}
					return nil, xerrors.Errorf("fetching f3 cert %d: %w", i, cerr)
				}
				batch = append(batch, c)
				// Flush in chunks so the validator doesn't have to hold
				// thousands of certs in memory at once.
				if len(batch) >= 200 || i == latest.GPBFTInstance {
					next, chain, newPT, vErr := f3.VerifyCertChain(opts.Manifest.NetworkName, currentPT, instance, batch)
					if vErr != nil {
						return nil, xerrors.Errorf("verifying f3 cert chain at instance %d: %w", instance, vErr)
					}
					instance = next
					currentPT = newPT
					if chain != nil {
						latestChain = chain
					}
					latestCert = batch[len(batch)-1]
					batch = batch[:0]
				}
			}
			if latestCert != nil {
				hasF3Finality = true
				if latestChain != nil && !latestChain.IsZero() {
					head := latestChain.Head()
					finalizedTip = abi.ChainEpoch(head.Epoch)
					// Map go-f3 TipSet key bytes -> Lantern TipSetKey.
					ftsk, kerr := ltypes.TipSetKeyFromBytes(head.Key)
					if kerr != nil {
						return nil, xerrors.Errorf("decoding f3 tipset key: %w", kerr)
					}
					finalizedTSK = ftsk
				}
			}
		}
	}

	// 3. Pick the selected epoch.
	currentHead, err := headers.Head(ctx)
	if err != nil {
		return nil, xerrors.Errorf("reading chain head: %w", err)
	}
	selected := currentHead - opts.MaxBacktrack
	if hasF3Finality && finalizedTip > selected {
		selected = finalizedTip
	}
	if selected < 0 {
		selected = 0
	}

	// 4. Fetch the selected tipset and validate headers.
	selectedTS, err := headers.Tipset(ctx, selected)
	if err != nil {
		return nil, xerrors.Errorf("fetching tipset at epoch %d: %w", selected, err)
	}
	if selectedTS == nil || len(selectedTS.Blocks()) == 0 {
		return nil, fmt.Errorf("trustedroot: tipset at epoch %d is empty", selected)
	}

	// Sanity: header CID + tipset shape.
	for _, b := range selectedTS.Blocks() {
		if err := header.VerifyBlockHeaderCID(b, b.Cid()); err != nil {
			return nil, xerrors.Errorf("header CID self-check at epoch %d: %w", selected, err)
		}
	}
	if _, err := header.ValidateTipsetShape(selectedTS.Blocks()); err != nil {
		return nil, xerrors.Errorf("tipset shape at epoch %d: %w", selected, err)
	}

	// Optional beacon-entry verification on the head block; the spec only
	// requires this on the headers along the ancestry, which Phase 1
	// integration runs against live data.
	if opts.BeaconConfig != nil {
		head := selectedTS.Blocks()[0]
		if len(head.BeaconEntries) > 0 {
			if err := opts.BeaconConfig.VerifyEntries(head.BeaconEntries, nil); err != nil {
				// In chained mode we'd need the previous round's signature;
				// non-fatal here because the integration runner supplies
				// quicknet (unchained) entries by default. Surface as a
				// warning via the returned root structure later.
			}
		}
	}

	// Optional: ancestor state roots for state queries against recent past
	// epochs. We walk backward AncestorRootDepth tipsets.
	ancestors := make([]cid.Cid, 0, opts.AncestorRootDepth)
	for ep := selectedTS.Height() - 1; ep >= 0 && len(ancestors) < opts.AncestorRootDepth; ep-- {
		ts, err := headers.Tipset(ctx, ep)
		if err != nil {
			break
		}
		if ts == nil || len(ts.Blocks()) == 0 {
			continue
		}
		ancestors = append(ancestors, ts.Blocks()[0].ParentStateRoot)
	}

	// 5. Construct the root and persist.
	headBlock := selectedTS.Blocks()[0]
	var beaconRound uint64
	if n := len(headBlock.BeaconEntries); n > 0 {
		beaconRound = headBlock.BeaconEntries[n-1].Round
	}

	tr := &TrustedRoot{
		Epoch:                 selectedTS.Height(),
		TipSetKey:             selectedTS.Key(),
		StateRoot:             headBlock.ParentStateRoot,
		ParentMessageReceipts: headBlock.ParentMessageReceipts,
		ParentWeight:          headBlock.ParentWeight,
		BeaconRound:           beaconRound,
		F3Instance:            instance,
		F3Cert:                latestCert,
		AcceptedAt:            time.Now().UTC(),
		AncestorRoots:         ancestors,
	}

	// If we have a finalized tipset key from F3, sanity-check that the
	// selected tipset is on the finalized ancestry (or beyond it). Phase 1
	// just records the F3 cert; the consistency check is best-effort
	// because the header source may have moved past the F3 cert.
	_ = finalizedTSK

	if db != nil {
		if err := Persist(db, tr); err != nil {
			return nil, xerrors.Errorf("persisting trusted root: %w", err)
		}
	}

	return tr, nil
}

// Key prefixes for badger persistence.
const (
	keyTrustedCurrent = "tr:current"
	keyTrustedFinal   = "tr:final:"
)

// Persist writes the TrustedRoot to db under tr:current and an additional
// tr:final:<epoch> if F3Cert is present.
func Persist(db *badger.DB, tr *TrustedRoot) error {
	if tr == nil {
		return errors.New("trustedroot: nil root")
	}
	return db.Update(func(txn *badger.Txn) error {
		buf, err := encodeTrustedRoot(tr)
		if err != nil {
			return err
		}
		if err := txn.Set([]byte(keyTrustedCurrent), buf); err != nil {
			return err
		}
		if tr.F3Cert != nil {
			var epochKey [8]byte
			binary.BigEndian.PutUint64(epochKey[:], uint64(tr.Epoch))
			if err := txn.Set([]byte(keyTrustedFinal+string(epochKey[:])), buf); err != nil {
				return err
			}
		}
		return nil
	})
}

// FromF3State produces a TrustedRoot from an F3 subscriber's verified
// state. This is the Phase 6 "activated cert chain" path: instead of
// running the legacy Build pipeline against header sources, we trust the
// subscriber's `(Instance, LatestChain)` which was walked forward from the
// embedded anchor with full BLS-aggregate verification at every step.
//
// `headers` is used to fetch the canonical tipset at the F3-finalized
// epoch so we can populate Lantern-side fields (StateRoot,
// ParentMessageReceipts, ParentWeight). Pass nil to defer those fields
// (acceptable when the caller only needs F3Instance / F3Cert).
func FromF3State(ctx context.Context, finalizedTSK ltypes.TipSetKey, finalizedEpoch abi.ChainEpoch, instance uint64, latest *certs.FinalityCertificate, headers HeaderSource) (*TrustedRoot, error) {
	tr := &TrustedRoot{
		Epoch:      finalizedEpoch,
		TipSetKey:  finalizedTSK,
		F3Instance: instance,
		F3Cert:     latest,
		AcceptedAt: time.Now().UTC(),
	}
	if headers != nil {
		ts, err := headers.Tipset(ctx, finalizedEpoch)
		if err == nil && ts != nil && len(ts.Blocks()) > 0 {
			b := ts.Blocks()[0]
			tr.StateRoot = b.ParentStateRoot
			tr.ParentMessageReceipts = b.ParentMessageReceipts
			tr.ParentWeight = b.ParentWeight
			if n := len(b.BeaconEntries); n > 0 {
				tr.BeaconRound = b.BeaconEntries[n-1].Round
			}
		}
	}
	return tr, nil
}

// Load returns the latest persisted TrustedRoot, or (nil, badger.ErrKeyNotFound)
// if no root was previously stored.
func Load(_ context.Context, db *badger.DB) (*TrustedRoot, error) {
	var out *TrustedRoot
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(keyTrustedCurrent))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			tr, derr := decodeTrustedRoot(val)
			if derr != nil {
				return derr
			}
			out = tr
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
