// Lantern DRAND beacon verifier. Written for Lantern; not copied from Lotus.
//
// We deliberately keep this much narrower than lotus/chain/beacon/drand: this
// package only verifies BeaconEntries that have already been delivered (e.g.
// from a block header). All networking, scheduling and prefetching that the
// Lotus DrandBeacon does is out of Phase 1 scope.
//
// We avoid github.com/drand/drand/v2/common/chain on purpose: that subtree
// imports drand's protobuf types, which transitively drag in gRPC, OpenTelemetry
// gRPC exporters, and a conflicting copy of google.golang.org/genproto. We
// parse the chain-info JSON ourselves (it's a stable, documented format) and
// only use the leaf packages we actually need (drand/v2/common,
// drand/v2/crypto, kyber).

package beacon

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	dcommon "github.com/drand/drand/v2/common"
	dcrypto "github.com/drand/drand/v2/crypto"
	"github.com/drand/kyber"
	"golang.org/x/xerrors"

	ltypes "github.com/Reiers/lantern/chain/types"
)

// Config is the trust anchor for a single DRAND chain: the chain hash, the
// signature scheme ID (e.g. "pedersen-bls-chained" or "bls-unchained-g1-rfc9380"),
// the group public key, and timing parameters.
type Config struct {
	// PublicKey is the DRAND group public key.
	PublicKey kyber.Point
	// Scheme is the DRAND signature scheme.
	Scheme *dcrypto.Scheme
	// IsChained reports whether beacons in this network chain to their
	// predecessor (pre-quicknet) or stand alone (post-quicknet, FIP-0063).
	IsChained bool
	// Hash is the network chain hash, for diagnostics.
	Hash []byte
}

// LoadConfigFromChainInfoJSON parses a drand chain-info JSON blob and an
// "is chained" flag, producing a verifier configuration. The JSON shape is
// the one Lotus stores in build/buildconstants.DrandConfigs[<network>].ChainInfoJSON.
//
// If schemeID is absent (older Lotus mainnet chain info), we default to
// "pedersen-bls-chained", the original league-of-entropy mainnet scheme.
func LoadConfigFromChainInfoJSON(chainInfoJSON string, isChained bool) (*Config, error) {
	var raw struct {
		PublicKey   string `json:"public_key"`
		Period      int    `json:"period"`
		GenesisTime int64  `json:"genesis_time"`
		Hash        string `json:"hash"`
		GroupHash   string `json:"groupHash"`
		SchemeID    string `json:"schemeID"`
	}
	if err := json.Unmarshal([]byte(chainInfoJSON), &raw); err != nil {
		return nil, xerrors.Errorf("parsing drand chain info: %w", err)
	}
	if raw.PublicKey == "" {
		return nil, errors.New("drand chain info missing public_key")
	}

	schemeName := raw.SchemeID
	if schemeName == "" {
		schemeName = dcrypto.DefaultSchemeID
	}
	scheme, err := dcrypto.SchemeFromName(schemeName)
	if err != nil {
		return nil, xerrors.Errorf("unknown drand scheme %q: %w", schemeName, err)
	}

	pkBytes, err := hex.DecodeString(raw.PublicKey)
	if err != nil {
		return nil, xerrors.Errorf("decoding drand public_key hex: %w", err)
	}
	pubkey := scheme.KeyGroup.Point()
	if err := pubkey.UnmarshalBinary(pkBytes); err != nil {
		return nil, xerrors.Errorf("unmarshalling drand public key: %w", err)
	}

	var hashBytes []byte
	if raw.Hash != "" {
		hashBytes, err = hex.DecodeString(raw.Hash)
		if err != nil {
			return nil, xerrors.Errorf("decoding drand hash hex: %w", err)
		}
	}

	return &Config{
		PublicKey: pubkey,
		Scheme:    scheme,
		IsChained: isChained,
		Hash:      hashBytes,
	}, nil
}

// VerifyEntry verifies a single beacon entry against the configured chain.
// For chained schemes, prevSig must be the signature data of the round
// preceding entry.Round. For unchained schemes (e.g. quicknet), prevSig is
// ignored (and may be nil).
func (c *Config) VerifyEntry(entry ltypes.BeaconEntry, prevSig []byte) error {
	if c == nil || c.Scheme == nil || c.PublicKey == nil {
		return errors.New("beacon: unconfigured verifier")
	}
	b := &dcommon.Beacon{
		Round:     entry.Round,
		Signature: entry.Data,
	}
	if c.IsChained {
		b.PreviousSig = prevSig
	}
	if err := c.Scheme.VerifyBeacon(b, c.PublicKey); err != nil {
		return fmt.Errorf("beacon round %d signature invalid: %w", entry.Round, err)
	}
	return nil
}

// VerifyEntries verifies a sequence of beacon entries. prevSig is the
// signature data at the round preceding the first entry (typically the last
// beacon of the parent block); pass nil for unchained schemes.
//
// The function does NOT enforce contiguous rounds: that's a header-level
// concern. It only verifies signatures and (for chained schemes) the
// previous-signature linkage between successive entries.
func (c *Config) VerifyEntries(entries []ltypes.BeaconEntry, prevSig []byte) error {
	prev := prevSig
	for i, e := range entries {
		if err := c.VerifyEntry(e, prev); err != nil {
			return xerrors.Errorf("beacon entry %d: %w", i, err)
		}
		if c.IsChained {
			prev = e.Data
		}
	}
	return nil
}

// EqualHash returns true iff the provided chain hash matches this config.
// Useful as a sanity check when wiring multiple Configs into a schedule.
func (c *Config) EqualHash(h []byte) bool {
	return bytes.Equal(c.Hash, h)
}
