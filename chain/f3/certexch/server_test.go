package certexch_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/filecoin-project/go-f3/certexchange"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/gpbft"
	"github.com/filecoin-project/go-f3/manifest"
	cid "github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/chain/bootstrap/sources"
	"github.com/Reiers/lantern/chain/f3/anchor"
	"github.com/Reiers/lantern/chain/f3/certexch"
)

const testNet gpbft.NetworkName = "lantern-certexch-test"

// stubCertSource is a subscriber.CertSource that serves a fixed
// (latest, certs[]) tuple. Used so the integration test doesn't need
// to dial an upstream JSON-RPC.
type stubCertSource struct {
	certs  []*certs.FinalityCertificate
	latest *certs.FinalityCertificate
}

func (s *stubCertSource) GetCert(_ context.Context, instance uint64) (*certs.FinalityCertificate, error) {
	for _, c := range s.certs {
		if c.GPBFTInstance == instance {
			return c, nil
		}
	}
	return nil, errors.New("not found")
}
func (s *stubCertSource) GetLatest(_ context.Context) (*certs.FinalityCertificate, error) {
	return s.latest, nil
}

func cidOf(t *testing.T, s string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.Raw, h)
}

// TestResponderRoundTrip is the B-11-01 integration gate: bring up a
// real go-f3 certexchange.Server on one mocknet peer (via our
// Responder, sharing the same libp2p host model a beacon uses) and ask
// it from a real LanternBeaconSource on another mocknet peer. Verify
// the cert that round-trips is byte-identical to the one we seeded.
//
// This proves the responder side of B-11-01 hooks the existing certstore
// + go-f3 server up correctly and the source side decodes it back into
// the same bootstrap.Finality shape the quorum compares on.
func TestResponderRoundTrip(t *testing.T) {
	mn := mocknet.New()
	t.Cleanup(func() { _ = mn.Close() })

	beaconHost, err := mn.GenPeer()
	require.NoError(t, err)
	clientHost, err := mn.GenPeer()
	require.NoError(t, err)
	require.NoError(t, mn.LinkAll())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Build a Responder around the embedded mainnet anchor. We disable
	// the live JSON-RPC poll by supplying a stub source that returns
	// "no progress to make" (latest = anchor.Instance - 1 would be
	// rejected; instead we hand it a cert at the anchor instance and
	// let the test seed the store directly).
	a, err := anchor.Embedded("mainnet")
	require.NoError(t, err)
	require.NotZero(t, a.Instance)

	stub := &stubCertSource{}

	r, err := certexch.New(certexch.Config{
		Host:           beaconHost,
		Anchor:         a,
		Manifest:       &manifest.Manifest{NetworkName: testNet},
		CertSource:     stub,
		PollInterval:   time.Hour, // effectively disabled for this test
		RequestTimeout: 2 * time.Second,
	})
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx))
	t.Cleanup(func() { _ = r.Stop(context.Background()) })

	// Seed the underlying certstore directly with a cert for the anchor
	// instance. We can't use the verify-then-Put path because we don't
	// have a real BLS-signed cert; the responder still serves whatever
	// is in its store, which is exactly what a real beacon does after
	// its ingest loop validates and inserts certs.
	store := r.Store()
	require.NotNil(t, store)

	pt, err := a.PowerTable()
	require.NoError(t, err)
	ptCid, err := certs.MakePowerTableCID(pt.Entries)
	require.NoError(t, err)

	tsk := gpbft.TipSetKey(cidOf(t, "round-trip-tsk").Bytes())
	cert := &certs.FinalityCertificate{
		GPBFTInstance:    a.Instance,
		SupplementalData: gpbft.SupplementalData{PowerTable: ptCid},
		ECChain: &gpbft.ECChain{TipSets: []*gpbft.TipSet{{
			Epoch: 9_000_000, Key: tsk, PowerTable: ptCid,
		}}},
	}
	require.NoError(t, store.Put(ctx, cert))

	require.NoError(t, mn.ConnectAllButSelf())

	// Smoke-test the raw protocol first so a failure here unambiguously
	// points at the responder rather than the bootstrap-source decoder.
	rawClient := certexchange.Client{
		Host:           clientHost,
		NetworkName:    testNet,
		RequestTimeout: 3 * time.Second,
	}
	head, _, err := rawClient.Request(ctx, beaconHost.ID(),
		&certexchange.Request{FirstInstance: 0, Limit: 0})
	require.NoError(t, err)
	require.Equal(t, a.Instance+1, head.PendingInstance,
		"responder should advertise PendingInstance = anchor+1 after one seed cert")

	// Then exercise the actual LanternBeaconSource end-to-end.
	src := sources.NewLanternBeaconSource(
		clientHost,
		peer.AddrInfo{ID: beaconHost.ID(), Addrs: beaconHost.Addrs()},
		testNet,
		3*time.Second,
	)
	fin, err := src.LatestFinality(ctx)
	require.NoError(t, err)
	require.Equal(t, a.Instance, fin.Instance)
	require.Equal(t, int64(9_000_000), fin.Epoch)
	require.Equal(t, ptCid, fin.StateRoot)
	require.Len(t, fin.TipSetKey, 1)
	require.Equal(t, cidOf(t, "round-trip-tsk"), fin.TipSetKey[0])
}
