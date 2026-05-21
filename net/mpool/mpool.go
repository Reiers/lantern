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
}

// Pool is a libp2p-gossipsub-backed message pool.
type Pool struct {
	ps    *pubsub.PubSub
	topic *pubsub.Topic
	sub   *pubsub.Subscription
	cfg   Config

	mu       sync.Mutex
	pending  map[cid.Cid]*ltypes.SignedMessage // our published CIDs
	received uint64                              // total messages observed
	rejected uint64                              // failed superficial validation
	publishd uint64                              // total published
}

// New starts a Pool: joins the topic, subscribes, and (if OnMessage is set)
// dispatches incoming messages to the handler in a background goroutine.
func New(ctx context.Context, ps *pubsub.PubSub, cfg Config) (*Pool, error) {
	if cfg.Topic == "" {
		cfg.Topic = build.MainnetGossipTopicMessages
	}
	t, err := ps.Join(cfg.Topic)
	if err != nil {
		return nil, fmt.Errorf("join topic %s: %w", cfg.Topic, err)
	}
	sub, err := t.Subscribe()
	if err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("subscribe topic %s: %w", cfg.Topic, err)
	}
	p := &Pool{
		ps:      ps,
		topic:   t,
		sub:     sub,
		cfg:     cfg,
		pending: make(map[cid.Cid]*ltypes.SignedMessage),
	}
	go p.readLoop(ctx)
	return p, nil
}

// Close stops the subscription and topic.
func (p *Pool) Close() error {
	p.sub.Cancel()
	return p.topic.Close()
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
		p.pending[mcid] = sm
		p.mu.Unlock()
		log.Infof("mpool DRY-RUN: would publish %s (%d bytes) to %s", mcid, len(raw), p.cfg.Topic)
		return mcid, ErrDryRun
	}

	if err := p.topic.Publish(ctx, raw); err != nil {
		return cid.Undef, fmt.Errorf("publish: %w", err)
	}
	p.mu.Lock()
	p.pending[mcid] = sm
	p.publishd++
	p.mu.Unlock()
	return mcid, nil
}

// Pending returns a snapshot of locally pushed message CIDs (and their
// signed-message bodies).
func (p *Pool) Pending() []*ltypes.SignedMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*ltypes.SignedMessage, 0, len(p.pending))
	for _, sm := range p.pending {
		out = append(out, sm)
	}
	return out
}

// Forget drops a CID from the pending set (call after the message is
// confirmed on-chain).
func (p *Pool) Forget(c cid.Cid) {
	p.mu.Lock()
	delete(p.pending, c)
	p.mu.Unlock()
}

// Stats reports observable counters.
type Stats struct {
	Received   uint64
	Rejected   uint64
	Published  uint64
	PendingCnt int
	Topic      string
}

// Stats returns activity counters.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Received:   p.received,
		Rejected:   p.rejected,
		Published:  p.publishd,
		PendingCnt: len(p.pending),
		Topic:      p.cfg.Topic,
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
