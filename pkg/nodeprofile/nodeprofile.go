// Package nodeprofile persists the INSTALL-TIME node tier so a Lantern
// install runs as the class the operator chose - Light, PDP, or Full -
// without the operator re-passing runtime flags on every start, and
// crucially WITHOUT forcing the light node to carry the PDP footprint.
//
// The tier is chosen once by the installer (get.golantern.io) and written
// to <home>/<network>/node-profile.json. The daemon reads it at startup to
// pick tier-appropriate defaults (memory vs persistent cache, block-submit
// availability, cache budget). Runtime flags still override, so this is a
// default source, not a lock.
//
// Why a file and not a flag: a flag would mean one universal binary where
// every user - including a wallet-only light node - carries the machinery
// (and the operator has to remember the flags). Making tier an install-time
// property keeps the light node genuinely light and lets the installer
// provision only what the chosen tier needs.
package nodeprofile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tier is the node class chosen at install time.
type Tier string

const (
	// TierLight is the default ~1 GB wallet/deal-client/read node:
	// in-memory cache, no write/production surface beyond sending txs.
	TierLight Tier = "light"

	// TierPDP is the mid node: a persistent (2-5 GB) block cache with the
	// warm contract set pinned, plus the full write surface INCLUDING
	// block production, so it can prove/settle PDP and double as a backup
	// block producer (block production still needs a VM bridge - the
	// no-CGo wall).
	TierPDP Tier = "pdp"

	// TierFull is reserved for the full-node feasibility track. Not yet
	// buildable bridge-off (native FVM execution needs CGo/filecoin-ffi);
	// accepted here so the profile format is forward-compatible and the
	// installer can record the intent.
	TierFull Tier = "full"
)

// DefaultFullCacheBytes is the persistent-cache budget written for a Full
// tier install when the operator doesn't override --cache-gb. A Full node
// serves the whole chain surface (not just PDP contract subtrees), so it
// needs a bigger warm working set than PDP. 12 GiB is single-to-low-double-
// digit GB territory (see #92) with plenty of headroom for a Mac-mini
// deployment while staying well below Lotus's 76 GB baseline.
const DefaultFullCacheBytes int64 = 12 << 30

// DefaultPDPCacheBytes is the persistent-cache budget written for a PDP
// install: 3 GiB, the middle of the 2-5 GB tier target.
const DefaultPDPCacheBytes int64 = 3 << 30

// fileName is the per-network profile file name.
const fileName = "node-profile.json"

// Profile is the persisted install-time node configuration.
type Profile struct {
	// Tier is the node class. Empty/unknown is treated as Light.
	Tier Tier `json:"tier"`

	// PersistentCacheBytes is the block-cache soft budget for tiers that
	// use a persistent cache (PDP/Full). 0 => tier default.
	PersistentCacheBytes int64 `json:"persistent_cache_bytes,omitempty"`

	// AllowBlockSubmit records that the operator opted the node in as a
	// block producer / backup at install time. Still requires a VM bridge
	// at run time; recorded here so the daemon can surface the requirement.
	AllowBlockSubmit bool `json:"allow_block_submit,omitempty"`
}

// Path returns the profile path for a given home + network.
func Path(home, network string) string {
	return filepath.Join(home, network, fileName)
}

// UsesPersistentCache reports whether the tier wants an on-disk block cache.
func (p Profile) UsesPersistentCache() bool {
	return p.Tier == TierPDP || p.Tier == TierFull
}

// FullValidation reports whether this tier runs the pure-Go full-node
// per-block consensus pipeline (chain/fullvalidate, #90). Only the Full tier
// does; Light/PDP rely on the F3-anchored trusted root without re-verifying
// every block's VRF/signature/win-count.
func (p Profile) FullValidation() bool {
	return p.Tier == TierFull
}

// CacheBytes returns the effective persistent-cache budget for the tier
// (falling back to the tier default when unset).
func (p Profile) CacheBytes() int64 {
	if p.PersistentCacheBytes > 0 {
		return p.PersistentCacheBytes
	}
	switch p.Tier {
	case TierFull:
		return DefaultFullCacheBytes
	case TierPDP:
		return DefaultPDPCacheBytes
	}
	return 0
}

// Normalize coerces an unknown/empty tier to Light so a malformed or
// absent profile degrades safely to the smallest footprint.
func (p Profile) Normalize() Profile {
	switch strings.ToLower(string(p.Tier)) {
	case string(TierPDP):
		p.Tier = TierPDP
	case string(TierFull):
		p.Tier = TierFull
	default:
		p.Tier = TierLight
	}
	return p
}

// Load reads the profile for home+network. A missing file is NOT an error:
// it returns a Light profile (the safe default for any pre-existing install
// that predates node tiers). A malformed file returns an error so the
// operator notices, rather than silently downgrading a PDP node.
func Load(home, network string) (Profile, error) {
	path := Path(home, network)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Profile{Tier: TierLight}, nil
		}
		return Profile{}, fmt.Errorf("read node profile %s: %w", path, err)
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return Profile{}, fmt.Errorf("parse node profile %s: %w", path, err)
	}
	return p.Normalize(), nil
}

// Save writes the profile for home+network atomically (write temp + rename)
// so a crash mid-write can't leave a half-file that fails to parse.
func Save(home, network string, p Profile) error {
	p = p.Normalize()
	dir := filepath.Join(home, network)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create profile dir %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode node profile: %w", err)
	}
	raw = append(raw, '\n')
	path := Path(home, network)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write node profile temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit node profile: %w", err)
	}
	return nil
}
