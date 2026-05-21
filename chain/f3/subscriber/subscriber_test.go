package subscriber_test

import (
	"context"
	"errors"
	"testing"

	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/manifest"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/chain/f3/anchor"
	"github.com/Reiers/lantern/chain/f3/subscriber"
)

type stubSource struct{}

func (stubSource) GetCert(_ context.Context, _ uint64) (*certs.FinalityCertificate, error) {
	return nil, errors.New("stub")
}
func (stubSource) GetLatest(_ context.Context) (*certs.FinalityCertificate, error) {
	return nil, errors.New("stub")
}

func TestSubscriberRequiresAnchor(t *testing.T) {
	_, err := subscriber.New(subscriber.Options{
		Manifest: &manifest.Manifest{NetworkName: "test"},
		Source:   stubSource{},
	})
	require.Error(t, err)
}

func TestSubscriberRequiresSource(t *testing.T) {
	a, err := anchor.Embedded("mainnet")
	require.NoError(t, err)
	_, err = subscriber.New(subscriber.Options{
		Anchor:   a,
		Manifest: &manifest.Manifest{NetworkName: "test"},
	})
	require.Error(t, err)
}

// TestSubscriberInitialState verifies that with a fresh anchor + manifest +
// source, the subscriber's initial state.Instance matches the anchor and
// the power table is non-empty.
func TestSubscriberInitialState(t *testing.T) {
	a, err := anchor.Embedded("mainnet")
	require.NoError(t, err)
	s, err := subscriber.New(subscriber.Options{
		Anchor:   a,
		Manifest: &manifest.Manifest{NetworkName: "filecoin"},
		Source:   stubSource{},
	})
	require.NoError(t, err)
	st := s.State()
	require.Equal(t, a.Instance, st.Instance)
	require.NotEmpty(t, st.PowerTable)
}
