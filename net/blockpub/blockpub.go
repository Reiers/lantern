// Package blockpub publishes and subscribes block messages on the
// Filecoin /fil/blocks/<network> gossipsub topic.
//
// Phase 8 Part E: pre-requisite for the day we lift the
// SyncSubmitBlock gate. Today the gate is hard-locked
// (AllowBlockSubmit=false by default) and any operator who flips it on
// without a bridge for the post-execution state root (PHASE7-BLOCKERS.md
// B2) would publish blocks the network rejects.
//
// This package exists so the publish path is wired and tested *before*
// the gate is liftable, not after. Subscribe-side validation is
// deliberately superficial — we only validate CBOR shape + BLS
// signature presence here; full header validation is the consumer's
// responsibility (chain/header.ValidateBlock).

package blockpub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ipfs/go-cid"
	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/Reiers/lantern/build"
	ltypes "github.com/Reiers/lantern/chain/types"
)

// Config configures a Publisher.
type Config struct {
	// Topic name. Defaults to build.MainnetGossipTopicBlocks
	// (/fil/blocks/testnetnet on mainnet).
	Topic string
	// DryRun: when true, Publish runs all validation but does not push
	// the bytes to gossipsub. Useful for SP rehearsal flows.
	DryRun bool
	// OnBlock fires for every block received from the network. Nil
	// means "ignore inbound traffic." Validation is the consumer's
	// job — we deliver any block that decodes cleanly.
	OnBlock func(*ltypes.BlockMsg)
}

// Publisher publishes + subscribes on the block gossipsub topic.
type Publisher struct {
	topic *pubsub.Topic
	sub   *pubsub.Subscription
	cfg   Config

	mu        sync.Mutex
	published uint64
	received  uint64
	rejected  uint64
}

// New joins the block topic and (if OnBlock is set) starts the read
// loop. Subscribe is unconditional so we can count inbound traffic
// even without a handler.
func New(ctx context.Context, ps *pubsub.PubSub, cfg Config) (*Publisher, error) {
	if cfg.Topic == "" {
		cfg.Topic = build.MainnetGossipTopicBlocks
	}
	t, err := ps.Join(cfg.Topic)
	if err != nil {
		return nil, fmt.Errorf("blockpub: join topic %s: %w", cfg.Topic, err)
	}
	sub, err := t.Subscribe()
	if err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("blockpub: subscribe %s: %w", cfg.Topic, err)
	}
	p := &Publisher{topic: t, sub: sub, cfg: cfg}
	go p.readLoop(ctx)
	return p, nil
}

// Close stops the subscription.
func (p *Publisher) Close() error {
	p.sub.Cancel()
	return p.topic.Close()
}

// Publish marshals + publishes a BlockMsg on the topic.
//
// IMPORTANT: callers should only invoke Publish from a context where
// AllowBlockSubmit is true AND a VM bridge has produced the correct
// post-execution state root for the block's ParentStateRoot. Publishing
// a block with an incorrect stateRoot is a soft-fault: the network
// rejects it and the publishing peer eats a little reputation cost.
// See TRUST-MODEL.md.
func (p *Publisher) Publish(ctx context.Context, blk *ltypes.BlockMsg) (cid.Cid, error) {
	if blk == nil || blk.Header == nil {
		return cid.Undef, errors.New("blockpub.Publish: nil block")
	}
	var buf bytes.Buffer
	if err := blk.MarshalCBOR(&buf); err != nil {
		return cid.Undef, fmt.Errorf("blockpub: marshal block: %w", err)
	}
	headerCID := blk.Header.Cid()
	if p.cfg.DryRun {
		// Still count it.
		p.mu.Lock()
		p.published++
		p.mu.Unlock()
		return headerCID, nil
	}
	if err := p.topic.Publish(ctx, buf.Bytes()); err != nil {
		return cid.Undef, fmt.Errorf("blockpub: publish: %w", err)
	}
	p.mu.Lock()
	p.published++
	p.mu.Unlock()
	return headerCID, nil
}

// PublishBlock matches the rpc/handlers.BlockPublisher interface
// signature. It wraps Publish and discards the returned CID, which the
// caller can recompute via blk.Header.Cid() if needed.
func (p *Publisher) PublishBlock(ctx context.Context, blk *ltypes.BlockMsg) error {
	_, err := p.Publish(ctx, blk)
	return err
}

// Stats returns lifetime counters: published, received, rejected.
func (p *Publisher) Stats() (published, received, rejected uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.published, p.received, p.rejected
}

func (p *Publisher) readLoop(ctx context.Context) {
	for {
		msg, err := p.sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			return
		}
		p.mu.Lock()
		p.received++
		p.mu.Unlock()
		blk := new(ltypes.BlockMsg)
		if err := blk.UnmarshalCBOR(bytes.NewReader(msg.Data)); err != nil {
			p.mu.Lock()
			p.rejected++
			p.mu.Unlock()
			continue
		}
		if !superficiallyValid(blk) {
			p.mu.Lock()
			p.rejected++
			p.mu.Unlock()
			continue
		}
		if p.cfg.OnBlock != nil {
			p.cfg.OnBlock(blk)
		}
	}
}

// superficiallyValid returns true if the block has the shape we expect:
// header present, Miner address defined, BlockSig + BLSAggregate present.
// Deep validation (BLS verify, parent linkage) is the consumer's job.
func superficiallyValid(b *ltypes.BlockMsg) bool {
	if b == nil || b.Header == nil {
		return false
	}
	h := b.Header
	if h.Miner.Empty() {
		return false
	}
	if h.BlockSig == nil || len(h.BlockSig.Data) == 0 {
		return false
	}
	if h.BLSAggregate == nil {
		return false
	}
	return true
}
