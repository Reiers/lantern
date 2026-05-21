// Package anchor handles persistent F3 trust anchors.
//
// An F3 trust anchor is a (PowerTable, Instance) tuple that Lantern accepts
// as the starting point for forward F3 cert-chain verification. Because the
// F3 manifest's InitialPowerTable predates Forest's local F3 finality base by
// ~900k epochs (it's bootstrapped over libp2p from peers, not from chain),
// Lantern instead anchors to a *recent* power table that we cross-verify at
// build time against:
//
//  1. A live Forest/Lotus node we control (this binary fetches over RPC).
//  2. The BLS aggregate signature on the cert that finalized the instance
//     prior to the anchor instance, which must verify >=2/3 of the prior
//     power table. (Loop-back: we trust the prior table because it was the
//     anchor on the previous build, or because the chain is so deep that the
//     economic cost of forging it is prohibitive.)
//
// The anchor is embedded as a build asset (`anchor_mainnet.json`,
// `anchor_calibnet.json`) and re-pinned whenever a new release is cut.
package anchor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"

	"github.com/filecoin-project/go-f3/gpbft"
)

// Anchor is the on-disk representation of a trusted F3 power table at a
// known instance. It is what Lantern starts forward-verification from.
type Anchor struct {
	// Network is "mainnet" or "calibnet" (or another testnet name).
	Network string `json:"network"`

	// Instance is the GPBFT instance whose committee is described by Entries.
	// Forward verification starts at Instance and walks each subsequent
	// certificate (which carries a power diff applied to the table).
	Instance uint64 `json:"instance"`

	// Entries is the committee at Instance, sorted by Power desc, then ID asc
	// (canonical order required by gpbft.PowerTable serialization).
	Entries []gpbft.PowerEntry `json:"entries"`

	// SourceBlock is human-readable provenance: the chain head height at the
	// moment this anchor was captured, and the Forest/Lotus version. Useful
	// for audit trails when re-pinning.
	SourceBlock string `json:"sourceBlock,omitempty"`
	CapturedAt  string `json:"capturedAt,omitempty"`
}

// PowerTable materialises the Anchor's Entries into a gpbft.PowerTable
// ready for forward verification. Returns an error if the entries are not in
// canonical order or contain duplicates.
func (a *Anchor) PowerTable() (*gpbft.PowerTable, error) {
	pt := gpbft.NewPowerTable()
	if err := pt.Add(a.Entries...); err != nil {
		return nil, fmt.Errorf("build power table: %w", err)
	}
	return pt, nil
}

// MarshalJSON emits the canonical anchor encoding. Pubkeys are base64-encoded
// so the file is line-diffable and ~80 KB instead of ~120 KB hex-encoded.
func (a *Anchor) MarshalJSON() ([]byte, error) {
	type wireEntry struct {
		ID     uint64 `json:"id"`
		Power  string `json:"power"`
		PubKey string `json:"pubkey"` // base64
	}
	type wire struct {
		Network     string      `json:"network"`
		Instance    uint64      `json:"instance"`
		Entries     []wireEntry `json:"entries"`
		SourceBlock string      `json:"sourceBlock,omitempty"`
		CapturedAt  string      `json:"capturedAt,omitempty"`
	}
	w := wire{
		Network:     a.Network,
		Instance:    a.Instance,
		Entries:     make([]wireEntry, len(a.Entries)),
		SourceBlock: a.SourceBlock,
		CapturedAt:  a.CapturedAt,
	}
	for i, e := range a.Entries {
		w.Entries[i] = wireEntry{
			ID:     uint64(e.ID),
			Power:  e.Power.String(),
			PubKey: base64.StdEncoding.EncodeToString(e.PubKey),
		}
	}
	return json.Marshal(w)
}

// UnmarshalJSON parses the canonical anchor encoding.
func (a *Anchor) UnmarshalJSON(b []byte) error {
	type wireEntry struct {
		ID     uint64 `json:"id"`
		Power  string `json:"power"`
		PubKey string `json:"pubkey"`
	}
	type wire struct {
		Network     string      `json:"network"`
		Instance    uint64      `json:"instance"`
		Entries     []wireEntry `json:"entries"`
		SourceBlock string      `json:"sourceBlock,omitempty"`
		CapturedAt  string      `json:"capturedAt,omitempty"`
	}
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	a.Network = w.Network
	a.Instance = w.Instance
	a.SourceBlock = w.SourceBlock
	a.CapturedAt = w.CapturedAt
	a.Entries = make([]gpbft.PowerEntry, len(w.Entries))
	for i, we := range w.Entries {
		pk, err := base64.StdEncoding.DecodeString(we.PubKey)
		if err != nil {
			return fmt.Errorf("entry %d pubkey base64: %w", i, err)
		}
		p := new(big.Int)
		if _, ok := p.SetString(we.Power, 10); !ok {
			return fmt.Errorf("entry %d power not decimal: %q", i, we.Power)
		}
		a.Entries[i] = gpbft.PowerEntry{
			ID:     gpbft.ActorID(we.ID),
			Power:  gpbft.StoragePower{Int: p},
			PubKey: gpbft.PubKey(pk),
		}
	}
	return nil
}

// FromForestPowerEntries builds an Anchor from the array returned by Forest's
// Filecoin.F3GetF3PowerTable RPC method. Entries are re-sorted to canonical
// order and validated for duplicates.
func FromForestPowerEntries(network string, instance uint64, raw []ForestPowerEntry, sourceBlock, capturedAt string) (*Anchor, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("no power entries")
	}
	entries := make([]gpbft.PowerEntry, len(raw))
	seen := make(map[uint64]bool, len(raw))
	for i, fe := range raw {
		if seen[fe.ID] {
			return nil, fmt.Errorf("duplicate actor ID %d", fe.ID)
		}
		seen[fe.ID] = true
		pk, err := base64.StdEncoding.DecodeString(fe.PubKey)
		if err != nil {
			return nil, fmt.Errorf("entry %d pubkey base64: %w", i, err)
		}
		p := new(big.Int)
		if _, ok := p.SetString(fe.Power, 10); !ok {
			return nil, fmt.Errorf("entry %d power not decimal: %q", i, fe.Power)
		}
		entries[i] = gpbft.PowerEntry{
			ID:     gpbft.ActorID(fe.ID),
			Power:  gpbft.StoragePower{Int: p},
			PubKey: gpbft.PubKey(pk),
		}
	}
	// Canonical order: power desc, then ID asc.
	sort.Slice(entries, func(i, j int) bool {
		pi, pj := entries[i].Power.Int, entries[j].Power.Int
		ci := pi.Cmp(pj)
		if ci != 0 {
			return ci > 0
		}
		return entries[i].ID < entries[j].ID
	})
	return &Anchor{
		Network:     network,
		Instance:    instance,
		Entries:     entries,
		SourceBlock: sourceBlock,
		CapturedAt:  capturedAt,
	}, nil
}

// ForestPowerEntry mirrors the wire format Forest emits over JSON-RPC.
type ForestPowerEntry struct {
	ID     uint64 `json:"ID"`
	Power  string `json:"Power"`
	PubKey string `json:"PubKey"` // base64
}

// Equal returns true if two anchors carry identical committee state. Useful
// for reproducibility tests across builds.
func (a *Anchor) Equal(b *Anchor) bool {
	if a.Network != b.Network || a.Instance != b.Instance || len(a.Entries) != len(b.Entries) {
		return false
	}
	for i := range a.Entries {
		ai, bi := a.Entries[i], b.Entries[i]
		if ai.ID != bi.ID || ai.Power.Int.Cmp(bi.Power.Int) != 0 || !bytes.Equal(ai.PubKey, bi.PubKey) {
			return false
		}
	}
	return true
}
