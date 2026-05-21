// AMT-backed accessor queries: MessageReceipt + MarketDeal.
//
// These traverse the parent-message-receipts AMT (rooted at
// TrustedRoot.ParentMessageReceipts) or the StorageMarket actor's Proposals
// AMT.

package accessor

import (
	"bytes"
	"context"
	"fmt"

	addr "github.com/filecoin-project/go-address"
	gscrypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"

	"github.com/Reiers/lantern/state/amt"
)

// Receipt is the per-message execution receipt stored in the AMT rooted at
// TrustedRoot.ParentMessageReceipts.
//
// Layout (from go-state-types/abi.MessageReceipt with EventsRoot optional):
//
//	[exitCode int, return bytes, gasUsed int64, eventsRoot? cid|null]
//
// Network version 18+ adds the optional EventsRoot pointer.
type Receipt struct {
	ExitCode   int64
	Return     []byte
	GasUsed    int64
	EventsRoot *cid.Cid // nil if not present or null
}

// MessageReceiptByIndex fetches receipt N from the ParentMessageReceipts
// AMT bound to the TrustedRoot. N is the position within the tipset's
// merged message list.
func (a *Accessor) MessageReceiptByIndex(ctx context.Context, idx uint64) (*Receipt, []cid.Cid, error) {
	if !a.tr.ParentMessageReceipts.Defined() {
		return nil, nil, fmt.Errorf("trusted root has no ParentMessageReceipts CID")
	}
	raw, proof, err := amt.Lookup(ctx, a.tr.ParentMessageReceipts, idx, a.bg, nil)
	if err != nil {
		return nil, proof, fmt.Errorf("AMT lookup receipt %d: %w", idx, err)
	}
	rcpt, err := decodeReceipt(raw)
	if err != nil {
		return nil, proof, fmt.Errorf("decoding receipt %d: %w", idx, err)
	}
	return rcpt, proof, nil
}

func decodeReceipt(raw []byte) (*Receipt, error) {
	br := bytes.NewReader(raw)
	maj, extra, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, err
	}
	if maj != cbg.MajArray {
		return nil, fmt.Errorf("receipt not an array (major %d)", maj)
	}
	if extra < 3 || extra > 4 {
		return nil, fmt.Errorf("receipt array length %d, want 3 or 4", extra)
	}

	// ExitCode (int).
	exit, err := readCborInt(br)
	if err != nil {
		return nil, fmt.Errorf("ExitCode: %w", err)
	}

	// Return (byte string).
	maj, l, err := cbg.CborReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("Return: %w", err)
	}
	if maj != cbg.MajByteString {
		return nil, fmt.Errorf("Return not byte string (major %d)", maj)
	}
	retBytes := make([]byte, l)
	if _, err := br.Read(retBytes); err != nil {
		return nil, fmt.Errorf("Return bytes: %w", err)
	}

	// GasUsed (int).
	gas, err := readCborInt(br)
	if err != nil {
		return nil, fmt.Errorf("GasUsed: %w", err)
	}

	// Optional EventsRoot.
	var ev *cid.Cid
	if extra == 4 {
		b, _ := br.ReadByte()
		if b == 0xf6 {
			// null
		} else {
			_ = br.UnreadByte()
			c, err := readCidLink(br)
			if err != nil {
				return nil, fmt.Errorf("EventsRoot: %w", err)
			}
			ev = &c
		}
	}

	return &Receipt{
		ExitCode:   exit,
		Return:     retBytes,
		GasUsed:    gas,
		EventsRoot: ev,
	}, nil
}

// readCborInt reads a CBOR signed-or-unsigned int as a single signed int64.
func readCborInt(br *bytes.Reader) (int64, error) {
	maj, extra, err := cbg.CborReadHeader(br)
	if err != nil {
		return 0, err
	}
	switch maj {
	case cbg.MajUnsignedInt:
		return int64(extra), nil
	case cbg.MajNegativeInt:
		return -1 - int64(extra), nil
	}
	return 0, fmt.Errorf("not an int (major %d)", maj)
}

// MarketDeal is a structural reflection of `market.DealProposal`. Decoding
// the full proposal is delegated to go-state-types when the caller wants
// strict types; this struct surfaces the most commonly used fields.
type MarketDeal struct {
	Raw    []byte // full CBOR-encoded DealProposal; caller can re-decode
	DealID uint64
}

// MarketDealRaw looks up the deal proposal bytes for a given deal ID by
// walking the StorageMarket actor's Proposals AMT.
//
// Layout: Market actor (singleton f05) → MarketState → Proposals (AMT root)
// → DealProposal at index dealID.
func (a *Accessor) MarketDealRaw(ctx context.Context, dealID uint64) ([]byte, []cid.Cid, error) {
	marketAddr, _ := addr.NewIDAddress(5)
	actor, actorProof, err := a.GetActorByID(ctx, marketAddr)
	if err != nil {
		return nil, actorProof, fmt.Errorf("loading Market actor (%s): %w", marketAddr, err)
	}

	marketStateRaw, err := a.bg.Get(ctx, actor.Head)
	if err != nil {
		return nil, actorProof, fmt.Errorf("fetching market state %s: %w", actor.Head, err)
	}
	proof := append(actorProof, actor.Head)

	proposalsAMT, err := decodeMarketProposalsCID(marketStateRaw)
	if err != nil {
		return nil, proof, fmt.Errorf("decoding market state: %w", err)
	}

	raw, amtProof, err := amt.Lookup(ctx, proposalsAMT, dealID, a.bg, nil)
	proof = append(proof, amtProof...)
	if err != nil {
		return nil, proof, fmt.Errorf("AMT lookup deal %d: %w", dealID, err)
	}
	return raw, proof, nil
}

// decodeMarketProposalsCID extracts the Proposals AMT root from the Market
// actor's serialized state. The state layout is many fields; Proposals is
// field 0 in every version since v2.
func decodeMarketProposalsCID(raw []byte) (cid.Cid, error) {
	br := bytes.NewReader(raw)
	maj, extra, err := cbg.CborReadHeader(br)
	if err != nil {
		return cid.Undef, err
	}
	if maj != cbg.MajArray {
		return cid.Undef, fmt.Errorf("market state not array")
	}
	if extra < 1 {
		return cid.Undef, fmt.Errorf("market state array empty")
	}
	return readCidLink(br)
}

// silence unused imports
var _ = gscrypto.Signature{}
