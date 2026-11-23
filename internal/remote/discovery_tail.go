package remote

import (
	"context"
	"fmt"
	"time"

	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
)

// discoveryTailSpan is how long a round may take and still replay its
// additions to the head: 1,024 blocks were 34 minutes on OP but 105 seconds
// on Robinhood.
const discoveryTailSpan = 10 * time.Minute

// discoveryTailBytes bounds the retained replay inputs.
const discoveryTailBytes = 128 << 20

// Keep only replay inputs, not readers, fee caches or producer acknowledgements.
func (d *poolDiscovery) appendTail(update canonicalUpdate) {
	update = canonicalUpdate{block: update.block, transactions: update.transactions}
	d.tail = append(d.tail, update)
	d.bytes += discoveryTailSize(update)
	limit := max(d.tailBlocks, discovery.ReplayBlockLimit)
	for len(d.tail) > limit || d.bytes > discoveryTailBytes {
		d.bytes -= discoveryTailSize(d.tail[0])
		d.tail[0] = canonicalUpdate{}
		d.tail = d.tail[1:]
	}
}

func discoveryTailSize(update canonicalUpdate) int {
	size := 256 + 32*len(update.block.Transactions)
	for _, tx := range update.transactions {
		size += 96
		for _, log := range tx.Logs {
			size += 96 + len(log.Topics)*32 + len(log.Data)
		}
	}
	return size
}

// reader is the round's view of the anchor block: the canonical reader, or
// one on the state endpoint pinned to the same block hash.
func (d *poolDiscovery) reader(r *runtime) *snapshot {
	if d.state == nil {
		return r.reader()
	}
	return &snapshot{client: d.state, block: r.block, chain: domain.ChainIdentity{Family: r.cfg.Network.Family, ChainID: r.cfg.ChainID()}, deployments: r.cfg.Network.DEX}
}

// awaitStateAnchor waits until the state endpoint serves the anchor block. It
// can trail the primary by a few blocks, and every round read names the
// anchor's hash.
func (d *poolDiscovery) awaitStateAnchor(ctx context.Context, anchor domain.BlockRef, budget, poll time.Duration) error {
	if d.state == nil {
		return nil
	}
	block, err := waitExecutionBlock(ctx, d.state, anchor.Hash, budget, poll)
	if err != nil {
		return fmt.Errorf("discovery state endpoint lacks anchor %d: %w", anchor.Number, err)
	}
	if block.Ref.Hash != anchor.Hash {
		return fmt.Errorf("discovery state endpoint returned another block for anchor %d", anchor.Number)
	}
	return nil
}
