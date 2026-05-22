// Lantern's F3 follower. Thin wrapper over github.com/filecoin-project/go-f3.
// We delegate all signature/aggregation/power-table-diff logic to go-f3.
// Lantern only owns:
//
//   - Loading the network manifest (from build/F3ManifestMainnetJSON).
//   - Seeding the initial power table (from a caller-provided loader).
//   - Driving certs.ValidateFinalityCertificates over a stream of certs.
//   - Persisting verified certs to BadgerDB (lives in chain/trustedroot).

package f3

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/xerrors"

	f3blssig "github.com/filecoin-project/go-f3/blssig"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/gpbft"
	"github.com/filecoin-project/go-f3/manifest"
)

// Manifest loads the network manifest from a JSON byte slice. JSON is the
// same shape Lotus ships in build/buildconstants/f3manifest_*.json.
func ParseManifest(jsonBytes []byte) (*manifest.Manifest, error) {
	var m manifest.Manifest
	if err := json.NewDecoder(bytes.NewReader(jsonBytes)).Decode(&m); err != nil {
		return nil, xerrors.Errorf("decoding F3 manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, xerrors.Errorf("validating F3 manifest: %w", err)
	}
	return &m, nil
}

// Verifier returns a pure-Go BLS verifier suitable for go-f3's
// certs.ValidateFinalityCertificates.
//
// go-f3's blssig.VerifierWithKeyOnG1 is implemented against
// go-f3/internal/gnark (Consensys gnark-crypto) and kyber's BDN scheme; no
// CGo, no filecoin-ffi.
func Verifier() gpbft.Verifier {
	return f3blssig.VerifierWithKeyOnG1()
}

// VerifyCertChain validates a list of consecutive finality certificates,
// starting at `firstInstance`, against the initial power table. It returns:
//
//   - nextInstance: the GPBFT instance immediately after the last validated cert
//   - finalChain:   the EC chain finalized by the last cert (head-most tipset key
//     is the new "F3 finalized" tip)
//   - newPowerTable: the power table to use validating the next cert
//
// Lantern keeps (finalChain.Head(), newPowerTable, nextInstance) as the F3
// witness inside the TrustedRoot.
func VerifyCertChain(
	network gpbft.NetworkName,
	initialPower gpbft.PowerEntries,
	firstInstance uint64,
	certsList []*certs.FinalityCertificate,
) (uint64, *gpbft.ECChain, gpbft.PowerEntries, error) {
	if len(certsList) == 0 {
		return firstInstance, nil, initialPower, errors.New("f3: empty certs list")
	}
	nextInstance, chain, newPT, err := certs.ValidateFinalityCertificates(
		Verifier(),
		network,
		initialPower,
		firstInstance,
		nil,
		certsList...,
	)
	if err != nil {
		return nextInstance, chain, newPT, xerrors.Errorf("f3: validating cert chain: %w", err)
	}
	return nextInstance, chain, newPT, nil
}

// VerifySingleCert validates a single new cert against the current power
// table and current instance counter. Mirrors VerifyCertChain for the
// steady-state follower path.
func VerifySingleCert(
	network gpbft.NetworkName,
	prevPower gpbft.PowerEntries,
	expectedInstance uint64,
	cert *certs.FinalityCertificate,
) (gpbft.PowerEntries, error) {
	if cert == nil {
		return prevPower, errors.New("f3: nil cert")
	}
	if cert.GPBFTInstance != expectedInstance {
		return prevPower, fmt.Errorf("f3: expected instance %d, got %d", expectedInstance, cert.GPBFTInstance)
	}
	_, _, newPower, err := certs.ValidateFinalityCertificates(
		Verifier(), network, prevPower, expectedInstance, nil, cert,
	)
	if err != nil {
		return prevPower, xerrors.Errorf("f3: validating single cert: %w", err)
	}
	return newPower, nil
}
