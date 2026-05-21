package delegated_test

import (
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-keccak"
	"github.com/filecoin-project/go-state-types/builtin"
	crypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/crypto/sigs"
	_ "github.com/Reiers/lantern/crypto/sigs/delegated"
)

func TestDelegatedRoundTrip(t *testing.T) {
	priv, err := sigs.Generate(crypto.SigTypeDelegated)
	require.NoError(t, err)

	pub, err := sigs.ToPublic(crypto.SigTypeDelegated, priv)
	require.NoError(t, err)

	// Compute the f4 delegated address from the keccak256 of the uncompressed
	// public key (skipping the 0x04 prefix), taking the lower 20 bytes.
	h := keccak.NewLegacyKeccak256()
	pubForHash := pub
	if len(pubForHash) > 0 && pubForHash[0] == 0x04 {
		pubForHash = pubForHash[1:]
	}
	h.Write(pubForHash)
	digest := h.Sum(nil)
	addr, err := address.NewDelegatedAddress(builtin.EthereumAddressManagerActorID, digest[12:])
	require.NoError(t, err)

	msg := []byte("delegated test")
	sig, err := sigs.Sign(crypto.SigTypeDelegated, priv, msg)
	require.NoError(t, err)

	require.NoError(t, sigs.Verify(sig, addr, msg))

	// Tamper.
	require.Error(t, sigs.Verify(sig, addr, []byte("different message")))
}
