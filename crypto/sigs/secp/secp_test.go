package secp_test

import (
	"testing"

	"github.com/filecoin-project/go-address"
	crypto "github.com/filecoin-project/go-state-types/crypto"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/crypto/sigs"
	_ "github.com/Reiers/lantern/crypto/sigs/secp"
)

func TestSecpRoundTrip(t *testing.T) {
	priv, err := sigs.Generate(crypto.SigTypeSecp256k1)
	require.NoError(t, err)

	pub, err := sigs.ToPublic(crypto.SigTypeSecp256k1, priv)
	require.NoError(t, err)

	addr, err := address.NewSecp256k1Address(pub)
	require.NoError(t, err)

	msg := []byte("hello lantern")
	sig, err := sigs.Sign(crypto.SigTypeSecp256k1, priv, msg)
	require.NoError(t, err)

	require.NoError(t, sigs.Verify(sig, addr, msg))

	// tamper
	require.Error(t, sigs.Verify(sig, addr, []byte("hello not-lantern")))
}
