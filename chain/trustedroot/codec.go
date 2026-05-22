// JSON codec for the persisted TrustedRoot. Phase 1 uses JSON for simplicity;
// Phase 2 may switch to CBOR-gen once we have stable on-disk schema needs.

package trustedroot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// trustedRootJSON is the on-disk representation. Field tags are kept short
// and explicit to allow inspection with `badger get tr:current | jq`.
type trustedRootJSON struct {
	Epoch                 int64            `json:"epoch"`
	TipSetKey             ltypes.TipSetKey `json:"tipset_key"`
	StateRoot             string           `json:"state_root"`
	ParentMessageReceipts string           `json:"parent_message_receipts"`
	ParentWeight          string           `json:"parent_weight"`
	BeaconRound           uint64           `json:"beacon_round"`
	F3Instance            uint64           `json:"f3_instance"`
	F3CertCBOR            []byte           `json:"f3_cert_cbor,omitempty"`
	AcceptedAt            time.Time        `json:"accepted_at"`
	AncestorRoots         []string         `json:"ancestor_roots,omitempty"`
}

func encodeTrustedRoot(tr *TrustedRoot) ([]byte, error) {
	if tr == nil {
		return nil, errors.New("trustedroot: encode nil")
	}
	out := trustedRootJSON{
		Epoch:                 int64(tr.Epoch),
		TipSetKey:             tr.TipSetKey,
		StateRoot:             tr.StateRoot.String(),
		ParentMessageReceipts: tr.ParentMessageReceipts.String(),
		ParentWeight:          tr.ParentWeight.String(),
		BeaconRound:           tr.BeaconRound,
		F3Instance:            tr.F3Instance,
		AcceptedAt:            tr.AcceptedAt,
	}
	if tr.F3Cert != nil {
		var buf bytes.Buffer
		if err := tr.F3Cert.MarshalCBOR(&buf); err != nil {
			return nil, fmt.Errorf("encoding f3 cert: %w", err)
		}
		out.F3CertCBOR = buf.Bytes()
	}
	if len(tr.AncestorRoots) > 0 {
		out.AncestorRoots = make([]string, len(tr.AncestorRoots))
		for i, c := range tr.AncestorRoots {
			out.AncestorRoots[i] = c.String()
		}
	}
	return json.Marshal(&out)
}

func decodeTrustedRoot(data []byte) (*TrustedRoot, error) {
	var in trustedRootJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("decoding trustedroot json: %w", err)
	}
	sr, err := cid.Parse(in.StateRoot)
	if err != nil {
		return nil, fmt.Errorf("decoding state root cid: %w", err)
	}
	pmr, err := cid.Parse(in.ParentMessageReceipts)
	if err != nil {
		return nil, fmt.Errorf("decoding parent message receipts cid: %w", err)
	}
	pw, err := ltypes.BigFromString(in.ParentWeight)
	if err != nil {
		return nil, fmt.Errorf("decoding parent weight: %w", err)
	}

	tr := &TrustedRoot{
		Epoch:                 abi.ChainEpoch(in.Epoch),
		TipSetKey:             in.TipSetKey,
		StateRoot:             sr,
		ParentMessageReceipts: pmr,
		ParentWeight:          pw,
		BeaconRound:           in.BeaconRound,
		F3Instance:            in.F3Instance,
		AcceptedAt:            in.AcceptedAt,
	}
	if len(in.F3CertCBOR) > 0 {
		c := new(certs.FinalityCertificate)
		if err := c.UnmarshalCBOR(bytes.NewReader(in.F3CertCBOR)); err != nil {
			return nil, fmt.Errorf("decoding f3 cert cbor: %w", err)
		}
		tr.F3Cert = c
	}
	if len(in.AncestorRoots) > 0 {
		tr.AncestorRoots = make([]cid.Cid, len(in.AncestorRoots))
		for i, s := range in.AncestorRoots {
			c, err := cid.Parse(s)
			if err != nil {
				return nil, fmt.Errorf("decoding ancestor root %d: %w", i, err)
			}
			tr.AncestorRoots[i] = c
		}
	}
	return tr, nil
}
