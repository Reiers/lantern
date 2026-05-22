// Package subscriber walks F3 finality certificates forward from the
// embedded trust anchor, verifying each cert against the running power
// table, applying its power-table-diff, and persisting the latest verified
// (instance, powerTable, finalizedTipSet) so subsequent boots don't re-walk
// from the anchor.
//
// Cert source: today this is a Forest/Lotus gateway over JSON-RPC
// (`Filecoin.F3GetCertificate`, `Filecoin.F3GetLatestCertificate`).
// Tomorrow we can switch to `go-f3/certexchange` over libp2p.

package subscriber

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathbig "math/big"
	"net/http"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-f3/gpbft"
	"github.com/filecoin-project/go-f3/manifest"
	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"

	"github.com/Reiers/lantern/chain/f3"
	"github.com/Reiers/lantern/chain/f3/anchor"
)

var log = logging.Logger("lantern/f3/subscriber")

// CertSource pulls F3 certs over the wire.
type CertSource interface {
	GetCert(ctx context.Context, instance uint64) (*certs.FinalityCertificate, error)
	GetLatest(ctx context.Context) (*certs.FinalityCertificate, error)
}

// JSONRPCSource implements CertSource via a Lotus-compatible JSON-RPC
// endpoint (Filecoin.F3GetCertificate / Filecoin.F3GetLatestCertificate).
type JSONRPCSource struct {
	URL    string
	Client *http.Client
}

// NewJSONRPCSource returns a JSONRPCSource with a 20s default timeout.
func NewJSONRPCSource(url string) *JSONRPCSource {
	return &JSONRPCSource{URL: url, Client: &http.Client{Timeout: 20 * time.Second}}
}

func (s *JSONRPCSource) rpc(ctx context.Context, method string, params []any, out any) error {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      1,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(all))
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(all, &env); err != nil {
		return fmt.Errorf("decode envelope: %w (body %s)", err, snippet(all))
	}
	if env.Error != nil {
		return fmt.Errorf("rpc: %s (code=%d)", env.Error.Message, env.Error.Code)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// GetCert calls Filecoin.F3GetCertificate(instance).
func (s *JSONRPCSource) GetCert(ctx context.Context, instance uint64) (*certs.FinalityCertificate, error) {
	var c certs.FinalityCertificate
	if err := s.rpc(ctx, "Filecoin.F3GetCertificate", []any{instance}, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// GetLatest calls Filecoin.F3GetLatestCertificate.
func (s *JSONRPCSource) GetLatest(ctx context.Context) (*certs.FinalityCertificate, error) {
	var c certs.FinalityCertificate
	if err := s.rpc(ctx, "Filecoin.F3GetLatestCertificate", nil, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func snippet(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// State is the in-memory follower state.
type State struct {
	NetworkName gpbft.NetworkName
	Instance    uint64                     // next instance to verify
	PowerTable  gpbft.PowerEntries         // committee at Instance
	Latest      *certs.FinalityCertificate // most recently verified cert
	LatestChain *gpbft.ECChain             // its EC chain (head = finalized)
}

// Subscriber walks F3 certs from anchor forward.
type Subscriber struct {
	mu    sync.Mutex
	state State
	src   CertSource
	store *badger.DB
	netNm gpbft.NetworkName
	stats Stats
}

// Stats tracks subscriber activity.
type Stats struct {
	CertsVerified uint64
	LastInstance  uint64
	LatestFinalEp int64
	LastError     string
}

// Options configures a Subscriber.
type Options struct {
	// Anchor is the embedded F3 trust anchor (Instance, committee).
	Anchor *anchor.Anchor
	// Manifest is the F3 network manifest (NetworkName).
	Manifest *manifest.Manifest
	// Source is the cert source (Forest/Lotus RPC).
	Source CertSource
	// DB is an optional BadgerDB for persistence of (instance, powerTable).
	DB *badger.DB
}

// New builds a Subscriber. Caller must call Bootstrap before Walk.
func New(opts Options) (*Subscriber, error) {
	if opts.Anchor == nil {
		return nil, errors.New("subscriber: nil anchor")
	}
	if opts.Manifest == nil {
		return nil, errors.New("subscriber: nil manifest")
	}
	if opts.Source == nil {
		return nil, errors.New("subscriber: nil source")
	}
	pt, err := opts.Anchor.PowerTable()
	if err != nil {
		return nil, fmt.Errorf("anchor.PowerTable: %w", err)
	}
	state := State{
		NetworkName: opts.Manifest.NetworkName,
		Instance:    opts.Anchor.Instance,
		PowerTable:  pt.Entries,
	}
	s := &Subscriber{
		state: state,
		src:   opts.Source,
		store: opts.DB,
		netNm: opts.Manifest.NetworkName,
	}
	// Try to load a persisted state newer than the anchor.
	if opts.DB != nil {
		if loaded, err := loadState(opts.DB); err == nil && loaded != nil && loaded.Instance > state.Instance {
			s.state = *loaded
		}
	}
	return s, nil
}

// State returns a snapshot of the current follower state.
func (s *Subscriber) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Stats returns activity counters.
func (s *Subscriber) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Walk pulls certs from the source starting at s.state.Instance, validates
// each, and advances state. It walks up to maxCerts or until the latest
// cert is reached, whichever is first. Returns the number of certs walked.
func (s *Subscriber) Walk(ctx context.Context, maxCerts int) (int, error) {
	latest, err := s.src.GetLatest(ctx)
	if err != nil {
		s.recordErr(err)
		return 0, fmt.Errorf("fetch latest cert: %w", err)
	}
	if latest == nil {
		return 0, errors.New("no latest cert")
	}
	if latest.GPBFTInstance < s.state.Instance {
		// We're ahead of the source — nothing to do.
		return 0, nil
	}
	walked := 0
	const batchSize = 50
	for s.state.Instance <= latest.GPBFTInstance {
		if maxCerts > 0 && walked >= maxCerts {
			break
		}
		batch := make([]*certs.FinalityCertificate, 0, batchSize)
		for i := 0; i < batchSize && s.state.Instance+uint64(i) <= latest.GPBFTInstance; i++ {
			c, cerr := s.src.GetCert(ctx, s.state.Instance+uint64(i))
			if cerr != nil {
				if i == 0 {
					return walked, fmt.Errorf("fetch cert %d: %w", s.state.Instance+uint64(i), cerr)
				}
				break
			}
			batch = append(batch, c)
		}
		if len(batch) == 0 {
			break
		}
		nextInstance, chain, newPT, err := f3.VerifyCertChain(s.netNm, s.state.PowerTable, s.state.Instance, batch)
		if err != nil {
			s.recordErr(err)
			return walked, xerrors.Errorf("verify cert chain (instance %d, batch=%d): %w", s.state.Instance, len(batch), err)
		}

		s.mu.Lock()
		s.state.Instance = nextInstance
		s.state.PowerTable = newPT
		s.state.Latest = batch[len(batch)-1]
		if chain != nil {
			s.state.LatestChain = chain
			s.stats.LatestFinalEp = chain.Head().Epoch
		}
		s.stats.CertsVerified += uint64(len(batch))
		s.stats.LastInstance = nextInstance - 1
		s.mu.Unlock()

		walked += len(batch)

		if s.store != nil {
			if err := saveState(s.store, &s.state); err != nil {
				log.Warnf("persist state: %v", err)
			}
		}
	}
	return walked, nil
}

func (s *Subscriber) recordErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.LastError = err.Error()
}

// ----- persistence -----

const stateKey = "f3:sub:state"

// persistedState is the on-disk schema. PowerTable is JSON-encoded power
// entries; we avoid CBOR here for diff-readability during debugging.
type persistedState struct {
	NetworkName string      `json:"networkName"`
	Instance    uint64      `json:"instance"`
	PowerTable  []entryJSON `json:"powerTable"`
	LatestEp    int64       `json:"latestEp"`
	WrittenAt   string      `json:"writtenAt"`
	// LatestCert is the serialized CBOR of the most recent verified cert.
	LatestCert []byte `json:"latestCert,omitempty"`
}

type entryJSON struct {
	ID     uint64 `json:"id"`
	Power  string `json:"power"`
	PubKey []byte `json:"pubkey"`
}

func saveState(db *badger.DB, st *State) error {
	pe := make([]entryJSON, len(st.PowerTable))
	for i, e := range st.PowerTable {
		pe[i] = entryJSON{
			ID:     uint64(e.ID),
			Power:  e.Power.Int.String(),
			PubKey: e.PubKey,
		}
	}
	ps := persistedState{
		NetworkName: string(st.NetworkName),
		Instance:    st.Instance,
		PowerTable:  pe,
		WrittenAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if st.LatestChain != nil {
		ps.LatestEp = st.LatestChain.Head().Epoch
	}
	if st.Latest != nil {
		buf := &bytes.Buffer{}
		if err := st.Latest.MarshalCBOR(buf); err == nil {
			ps.LatestCert = buf.Bytes()
		}
	}
	raw, err := json.Marshal(&ps)
	if err != nil {
		return err
	}
	return db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(stateKey), raw)
	})
}

func loadState(db *badger.DB) (*State, error) {
	var raw []byte
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(stateKey))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			raw = append([]byte(nil), val...)
			return nil
		})
	})
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var ps persistedState
	if err := json.Unmarshal(raw, &ps); err != nil {
		return nil, err
	}
	st := &State{
		NetworkName: gpbft.NetworkName(ps.NetworkName),
		Instance:    ps.Instance,
		PowerTable:  make(gpbft.PowerEntries, len(ps.PowerTable)),
	}
	for i, pe := range ps.PowerTable {
		p := new(mathbig.Int)
		if _, ok := p.SetString(pe.Power, 10); !ok {
			return nil, fmt.Errorf("entry %d power not decimal", i)
		}
		st.PowerTable[i] = gpbft.PowerEntry{
			ID:     gpbft.ActorID(pe.ID),
			Power:  gpbft.StoragePower{Int: p},
			PubKey: gpbft.PubKey(pe.PubKey),
		}
	}
	if len(ps.LatestCert) > 0 {
		var c certs.FinalityCertificate
		if err := c.UnmarshalCBOR(bytes.NewReader(ps.LatestCert)); err == nil {
			st.Latest = &c
			st.LatestChain = c.ECChain
		}
	}
	return st, nil
}

// Compile-time check: be8 helper compatibility kept for future schema
// versions where we may want fixed-width instance keys.
var _ = binary.BigEndian
