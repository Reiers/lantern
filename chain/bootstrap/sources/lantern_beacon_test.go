package sources_test

import (
	"context"
	"testing"
	"time"

	"github.com/filecoin-project/go-f3/certexchange"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/certstore"
	"github.com/filecoin-project/go-f3/gpbft"
	cid "github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dsync "github.com/ipfs/go-datastore/sync"
	"github.com/libp2p/go-libp2p/core/peer"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/Reiers/lantern/chain/bootstrap"
	"github.com/Reiers/lantern/chain/bootstrap/sources"
)

const testNet gpbft.NetworkName = "lantern-beacon-test"

func makePowerTable(t *testing.T) (gpbft.PowerEntries, cid.Cid) {
	t.Helper()
	pt := gpbft.PowerEntries{{
		ID:     1,
		Power:  gpbft.NewStoragePower(100),
		PubKey: []byte("k1"),
	}, {
		ID:     2,
		Power:  gpbft.NewStoragePower(100),
		PubKey: []byte("k2"),
	}}
	pcid, err := certs.MakePowerTableCID(pt)
	require.NoError(t, err)
	return pt, pcid
}

func cidFromString(t *testing.T, s string) cid.Cid {
	t.Helper()
	h, err := mh.Sum([]byte(s), mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.Raw, h)
}

// TestLanternBeaconSource_RoundTrip wires a mock libp2p network together
// so an in-process Lantern beacon (certexchange.Server) answers a query
// from a LanternBeaconSource. Asserts the source returns a Finality whose
// (instance, tipsetkey, stateroot) match the cert the responder served.
//
// This is the unit-level proof that B-11-01 actually closes the loop:
// quorum equality (sources.finalityFromCert) lines up with what the
// responder publishes.
func TestLanternBeaconSource_RoundTrip(t *testing.T) {
	mn := mocknet.New()
	t.Cleanup(func() { _ = mn.Close() })

	beaconHost, err := mn.GenPeer()
	require.NoError(t, err)
	clientHost, err := mn.GenPeer()
	require.NoError(t, err)
	require.NoError(t, mn.LinkAll())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Beacon-side certstore with two finalized instances.
	pt, pcid := makePowerTable(t)
	ds := dsync.MutexWrap(datastore.NewMapDatastore())
	cs, err := certstore.CreateStore(ctx, ds, 0, pt)
	require.NoError(t, err)

	tsk0 := gpbft.TipSetKey(cidFromString(t, "tsk0").Bytes())
	tsk1 := gpbft.TipSetKey(cidFromString(t, "tsk1").Bytes())

	cert0 := &certs.FinalityCertificate{
		GPBFTInstance:    0,
		SupplementalData: gpbft.SupplementalData{PowerTable: pcid},
		ECChain: &gpbft.ECChain{TipSets: []*gpbft.TipSet{{
			Epoch: 100, Key: tsk0, PowerTable: pcid,
		}}},
	}
	require.NoError(t, cs.Put(ctx, cert0))

	cert1 := &certs.FinalityCertificate{
		GPBFTInstance:    1,
		SupplementalData: gpbft.SupplementalData{PowerTable: pcid},
		ECChain: &gpbft.ECChain{TipSets: []*gpbft.TipSet{{
			Epoch: 200, Key: tsk1, PowerTable: pcid,
		}}},
	}
	require.NoError(t, cs.Put(ctx, cert1))

	server := certexchange.Server{
		NetworkName: testNet,
		Host:        beaconHost,
		Store:       cs,
	}
	require.NoError(t, server.Start(ctx))
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	require.NoError(t, mn.ConnectAllButSelf())

	// LanternBeaconSource pointed at the beacon's mocknet peer.
	src := sources.NewLanternBeaconSource(
		clientHost,
		peer.AddrInfo{ID: beaconHost.ID(), Addrs: beaconHost.Addrs()},
		testNet,
		5*time.Second,
	)
	require.Equal(t, bootstrap.KindLanternBeacon, src.Kind())
	require.Contains(t, src.Name(), "lantern-beacon:")

	fin, err := src.LatestFinality(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(1), fin.Instance, "expected to learn instance 1 (latest)")
	require.Equal(t, int64(200), fin.Epoch)
	require.Equal(t, pcid, fin.StateRoot,
		"finality state root should equal the head tipset's power table CID")
	require.Len(t, fin.TipSetKey, 1)
	require.Equal(t, cidFromString(t, "tsk1"), fin.TipSetKey[0])
}

// TestLanternBeaconSource_NilHostReturnsErrNoBeaconBackend verifies the
// V1.2.0 backwards-compat path: a source built without a host still
// fails cleanly instead of panicking. (Not used in the default source
// set; kept for callers that hand-roll source lists.)
func TestLanternBeaconSource_NilHostReturnsErrNoBeaconBackend(t *testing.T) {
	src := sources.NewLanternBeaconSource(nil, peer.AddrInfo{}, testNet, time.Second)
	_, err := src.LatestFinality(context.Background())
	require.ErrorIs(t, err, sources.ErrNoBeaconBackend)
}
