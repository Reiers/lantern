// Package mpool implements Lantern's gossipsub-based message pool.
//
// Subscribing: we listen on the network's message topic (mainnet:
// /fil/msgs/testnetnet) and deliver received signed messages to a
// caller-provided handler. We do superficial validation (CBOR shape,
// signature shape) before delivery; deep signature verification is
// deferred to the consumer (e.g. the rpc/handlers MpoolPush path).
//
// Publishing: take a SignedMessage, marshal to CBOR, publish to the topic.
// The CID of the published message is returned; we also track our locally
// pushed CIDs so StateSearchMsg can search forward for them.

package mpool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/Reiers/lantern/build"
	ltypes "github.com/Reiers/lantern/chain/types"
)

var log = logging.Logger("lantern/mpool")

// ErrDryRun is returned by Pool.Publish when DryRun is true.
var ErrDryRun = errors.New("mpool: dry-run mode, not publishing")

// Config configures a Pool.
type Config struct {
	// Topic name. Defaults to build.MainnetGossipTopicMessages.
	Topic string
	// DryRun: when true, Publish validates locally and records the
	// would-be publish but doesn't actually push to gossipsub. Useful
	// for production safety while we shake out the libp2p mesh.
	DryRun bool
	// OnMessage is fired for every signed message received from the
	// network. Nil means "ignore incoming traffic".
	OnMessage func(*ltypes.SignedMessage, peer.ID)

	// --- #47: pending confirm + rebroadcast loop ---
	//
	// ConfirmAfterEpochs is the confidence window: a published message not
	// seen on chain for at least this many epochs is rebroadcast (identical
	// bytes, same nonce/CID). Default 3 (~90s mainnet). Long enough that a
	// slow-but-valid inclusion isn't mistaken for failure.
	ConfirmAfterEpochs int64
	// MaxRetries caps rebroadcasts before a message is marked failed.
	// Default 5. 0 uses the default; use a negative value for "unlimited".
	MaxRetries int
	// OnFailed, if set, is invoked once when a pending message gives up
	// (max retries exhausted). The message is also moved to a failed set
	// observable via Stats.Failed. Never silently stuck.
	OnFailed func(*ltypes.SignedMessage, string)

	// --- #119: durable pending-message store ---
	//
	// PersistPath is the on-disk JSONL journal Lantern uses to keep the
	// pending set alive across daemon restart. When empty (the historical
	// default), the pool is memory-only and behaves like pre-#119.
	//
	// When set:
	//   - New() opens the file, replays the journal, and re-registers every
	//     live entry into the in-memory pending set. Nonce derivation
	//     (MpoolGetNonce) reads Pending() so a restart transparently keeps
	//     each account's next nonce correct.
	//   - Publish() fsyncs an "add" line before returning success.
	//   - Reconcile confirm/fail branches fsync a "tombstone" line.
	//   - Rebroadcast fsyncs a "retry" line with the bumped counter.
	//
	// The file lives at <home>/<network>/mpool/pending.jsonl. It is chain-
	// side of the secrets boundary (never in keystore/, secrets/, or
	// backups/), but `lantern reset --chain-state` deliberately does NOT
	// wipe it: user-signed pending messages are user state, not rebuildable
	// chain state.
	PersistPath string

	// --- #123: alternate wire sink for devnet ---
	//
	// Sink, when non-nil, replaces the gossipsub topic.Publish call in
	// Publish(). This is the devnet lotus-RPC path: on a single-node
	// docker devnet the gossipsub mesh can't form, so signed messages
	// are POST'd directly to the devnet lotus via Filecoin.MpoolPush
	// instead. All other Pool semantics (persist journal, pending set,
	// nonce derivation, reconcile/retry loop) work identically.
	//
	// When Sink is set, New() may be called with a nil pubsub instance;
	// no topic is joined and no subscription is created. The reconcile
	// loop still runs, rebroadcasting via Sink until inclusion.
	Sink func(ctx context.Context, sm *ltypes.SignedMessage, raw []byte) (cid.Cid, error)
}

// pendingMsg tracks a locally published message for the confirm/retry loop.
type pendingMsg struct {
	sm           *ltypes.SignedMessage
	raw          []byte // serialized bytes captured at first publish (rebroadcast is byte-identical)
	publishedAt  int64  // epoch observed at publish time (0 = unknown until first reconcile)
	retries      int
	lastActivity int64 // epoch of last (re)broadcast
}

// Pool is a libp2p-gossipsub-backed message pool.
type Pool struct {
	ps    *pubsub.PubSub
	topic *pubsub.Topic
	sub   *pubsub.Subscription
	cfg   Config

	mu       sync.Mutex
	pending  map[cid.Cid]*pendingMsg // our published CIDs awaiting inclusion
	received uint64                  // total messages observed
	rejected uint64                  // failed superficial validation
	publishd uint64                  // total published
	rebroad  uint64                  // total rebroadcasts (#47)
	confirmd uint64                  // total confirmed-on-chain (#47)
	failed   uint64                  // total given up (#47)

	// #119: durable pending-message journal. nil when PersistPath is empty.
	persist *persistStore
	// restored counts how many entries were re-registered from disk at
	// startup (observable via Stats for smoke tests + dashboard).
	restored uint64
}

// New starts a Pool: joins the topic, subscribes, and (if OnMessage is set)
// dispatches incoming messages to the handler in a background goroutine.
//
// When ps is nil, no gossipsub topic is joined and no subscription is
// created. In that mode cfg.Sink must be set (used for the devnet
// lotus-RPC send-path per #123). Persist + reconcile loop + pending set
// still work identically; the difference is only the wire transport used
// by Publish and Rebroadcast.
func New(ctx context.Context, ps *pubsub.PubSub, cfg Config) (*Pool, error) {
	if cfg.Topic == "" {
		cfg.Topic = build.MainnetGossipTopicMessages
	}
	if ps == nil && cfg.Sink == nil {
		return nil, errors.New("mpool: New requires either a pubsub instance or Config.Sink (devnet)")
	}
	var t *pubsub.Topic
	var sub *pubsub.Subscription
	if ps != nil {
		var err error
		t, err = ps.Join(cfg.Topic)
		if err != nil {
			return nil, fmt.Errorf("join topic %s: %w", cfg.Topic, err)
		}
		sub, err = t.Subscribe()
		if err != nil {
			_ = t.Close()
			return nil, fmt.Errorf("subscribe topic %s: %w", cfg.Topic, err)
		}
	}
	if cfg.ConfirmAfterEpochs <= 0 {
		cfg.ConfirmAfterEpochs = 3
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 5
	}
	p := &Pool{
		ps:      ps,
		topic:   t,
		sub:     sub,
		cfg:     cfg,
		pending: make(map[cid.Cid]*pendingMsg),
	}

	// #119: if PersistPath is set, open the journal + re-register any
	// live entries. Failure to open the store is fatal (we'd otherwise
	// silently regress to memory-only behaviour and lose the peace-of-mind
	// contract on the very restart that motivated #119).
	if cfg.PersistPath != "" {
		store, perr := openPersistStore(cfg.PersistPath)
		if perr != nil {
			if sub != nil {
				sub.Cancel()
			}
			if t != nil {
				_ = t.Close()
			}
			return nil, fmt.Errorf("open pending journal: %w", perr)
		}
		p.persist = store
		// Re-register live entries. Counter resets on restart: the
		// resurrection itself is not a rebroadcast, only actual re-gossip
		// events on the next Reconcile bump the counter.
		for _, e := range store.All() {
			sm, derr := e.SignedMessage()
			if derr != nil {
				log.Warnw("mpool persist: dropping undecodable entry", "cid", e.CID, "err", derr)
				_ = store.Remove(e.CID)
				continue
			}
			p.pending[e.CID] = &pendingMsg{
				sm:           sm,
				raw:          e.Raw,
				publishedAt:  e.PublishedAt,
				retries:      e.Retries,
				lastActivity: e.PublishedAt,
			}
			p.restored++
		}
		if p.restored > 0 {
			log.Infow("mpool persist: restored pending entries across restart", "count", p.restored, "path", cfg.PersistPath)
		}
	}

	if sub != nil {
		go p.readLoop(ctx)
	}
	return p, nil
}

// Close stops the subscription and topic, and closes the persist journal
// (when open). Safe when the pool was constructed without a pubsub
// instance (devnet Sink mode); the nil topic/sub branches are skipped.
func (p *Pool) Close() error {
	if p.sub != nil {
		p.sub.Cancel()
	}
	var topicErr error
	if p.topic != nil {
		topicErr = p.topic.Close()
	}
	if p.persist != nil {
		if perr := p.persist.Close(); perr != nil && topicErr == nil {
			return perr
		}
	}
	return topicErr
}

func (p *Pool) readLoop(ctx context.Context) {
	for {
		msg, err := p.sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warnf("mpool sub.Next: %v", err)
			return
		}
		p.mu.Lock()
		p.received++
		p.mu.Unlock()
		sm, err := decodeAndValidate(msg.Data)
		if err != nil {
			p.mu.Lock()
			p.rejected++
			p.mu.Unlock()
			continue
		}
		if p.cfg.OnMessage != nil {
			p.cfg.OnMessage(sm, msg.ReceivedFrom)
		}
	}
}

// Publish validates `sm` superficially, marshals it, and publishes to the
// gossipsub topic. Returns the message CID and any error. In DryRun mode it
// returns the CID + ErrDryRun without publishing.
func (p *Pool) Publish(ctx context.Context, sm *ltypes.SignedMessage) (cid.Cid, error) {
	if sm == nil {
		return cid.Undef, errors.New("mpool: nil signed message")
	}
	if err := validateLocal(sm); err != nil {
		return cid.Undef, err
	}
	raw, err := sm.Serialize()
	if err != nil {
		return cid.Undef, fmt.Errorf("serialize signed message: %w", err)
	}
	mcid := sm.Cid()

	if p.cfg.DryRun {
		p.mu.Lock()
		p.pending[mcid] = &pendingMsg{sm: sm, raw: raw}
		p.mu.Unlock()
		// #119: persist even dry-run entries so tests exercise the same
		// journal path production uses. A production caller with DryRun=true
		// AND PersistPath set is unusual but consistent.
		if p.persist != nil {
			if perr := p.persist.Add(&PersistEntry{
				CID:           mcid,
				Raw:           raw,
				FirstSeenWall: time.Now().UTC(),
			}); perr != nil {
				log.Warnw("mpool persist: add failed on dry-run publish", "cid", mcid, "err", perr)
			}
		}
		log.Infof("mpool DRY-RUN: would publish %s (%d bytes) to %s", mcid, len(raw), p.cfg.Topic)
		return mcid, ErrDryRun
	}

	if p.cfg.Sink != nil {
		// Devnet lotus-RPC send path (#123). Sink returns the message
		// CID lotus computed; we compare against our local CID as a
		// consistency check but keep our local value (bytes-identical
		// under an honest sink).
		remoteCid, err := p.cfg.Sink(ctx, sm, raw)
		if err != nil {
			return cid.Undef, fmt.Errorf("sink publish: %w", err)
		}
		if remoteCid.Defined() && remoteCid != mcid {
			log.Warnw("mpool sink: remote CID differs from local CID (raw bytes may have been re-serialized upstream)",
				"local", mcid, "remote", remoteCid)
		}
	} else {
		if err := p.topic.Publish(ctx, raw); err != nil {
			return cid.Undef, fmt.Errorf("publish: %w", err)
		}
	}
	p.mu.Lock()
	p.pending[mcid] = &pendingMsg{sm: sm, raw: raw}
	p.publishd++
	p.mu.Unlock()
	// #119: fsync the entry to the journal AFTER the gossipsub push. If
	// the fsync fails we surface as an error even though the message is
	// already on the wire; the caller can decide whether to retry (which
	// is idempotent: identical bytes, same nonce, same CID).
	if p.persist != nil {
		if perr := p.persist.Add(&PersistEntry{
			CID:           mcid,
			Raw:           raw,
			FirstSeenWall: time.Now().UTC(),
		}); perr != nil {
			return mcid, fmt.Errorf("persist pending message (gossipsub push already sent, safe to retry): %w", perr)
		}
	}
	return mcid, nil
}

// Pending returns a snapshot of locally pushed message CIDs (and their
// signed-message bodies) still awaiting inclusion.
func (p *Pool) Pending() []*ltypes.SignedMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*ltypes.SignedMessage, 0, len(p.pending))
	for _, pm := range p.pending {
		out = append(out, pm.sm)
	}
	return out
}

// Forget drops a CID from the pending set (call after the message is
// confirmed on-chain).
func (p *Pool) Forget(c cid.Cid) {
	p.mu.Lock()
	delete(p.pending, c)
	p.mu.Unlock()
	if p.persist != nil {
		if err := p.persist.Remove(c); err != nil {
			log.Warnw("mpool persist: remove on Forget failed", "cid", c, "err", err)
		}
	}
}

// Stats reports observable counters.
type Stats struct {
	Received     uint64
	Rejected     uint64
	Published    uint64
	Rebroadcasts uint64 // #47
	Confirmed    uint64 // #47
	Failed       uint64 // #47
	PendingCnt   int
	Topic        string
	Restored     uint64 // #119 — entries re-registered from persist journal on startup
	PersistPath  string // #119 — empty when persistence is disabled
}

// Stats returns activity counters.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Received:     p.received,
		Rejected:     p.rejected,
		Published:    p.publishd,
		Rebroadcasts: p.rebroad,
		Confirmed:    p.confirmd,
		Failed:       p.failed,
		PendingCnt:   len(p.pending),
		Topic:        p.cfg.Topic,
		Restored:     p.restored,
		PersistPath:  p.cfg.PersistPath,
	}
}

// decodeAndValidate parses a CBOR SignedMessage and runs validateLocal.
func decodeAndValidate(b []byte) (*ltypes.SignedMessage, error) {
	if len(b) == 0 {
		return nil, errors.New("empty")
	}
	var sm ltypes.SignedMessage
	if err := sm.UnmarshalCBOR(bytes.NewReader(b)); err != nil {
		return nil, fmt.Errorf("decode signed message: %w", err)
	}
	if err := validateLocal(&sm); err != nil {
		return nil, err
	}
	return &sm, nil
}

// validateLocal does superficial sanity checks on a SignedMessage. It does
// NOT verify the signature cryptographically — that's the consumer's job.
func validateLocal(sm *ltypes.SignedMessage) error {
	if sm == nil {
		return errors.New("nil signed message")
	}
	if sm.Message.From.Empty() {
		return errors.New("empty From")
	}
	if sm.Message.To.Empty() {
		return errors.New("empty To")
	}
	if sm.Signature.Type == 0 && len(sm.Signature.Data) == 0 {
		return errors.New("missing signature")
	}
	if len(sm.Signature.Data) == 0 {
		return errors.New("empty signature payload")
	}
	return nil
}
