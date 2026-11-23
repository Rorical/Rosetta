package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

type canonicalUpdate struct {
	fees []pool.State
	// metric holds the MetricOmm pools the follower read at this block.
	metric []metricRead
	// feesScheduled: the follower ran its fee schedule for this block, so
	// fees holds every read that was due and the rest are known unchanged.
	feesScheduled bool
	reads         *rpc.ReadCache
	nextPools     chan []pool.State
	block         rpc.ExecutionBlock
	// feeParams fingerprints the block's OP fee parameters: rpc.BlockFeeParams
	// of RPC receipts, or l1InfoFeeParams of a stream block.
	feeParams    opFeeParams
	transactions []ingest.Transaction
	reset        *poolstate.Store
	accepted     chan struct{}
	tip          uint64
	catchingUp   bool
	nearTip      bool
}

// follower is the only producer of canonical blocks and performs all of their
// network reads. The pool writer never waits for an HTTP round trip while
// processing preconfirmation messages.
//
// With a flashblock stream (flash non-nil) the stream supplies each block and
// RPC verifies a sample. Gaps, untrusted stream blocks, mismatches and a stale
// stream take the RPC path, which stays the canonical authority.
type follower struct {
	ctx      context.Context
	cfg      config.Config
	client   *rpc.Client
	metrics  *telemetry.Registry
	specs    []bootstrap.Spec
	previous []pool.State
	head     domain.BlockRef
	radius   int16
	wake     <-chan domain.Hash
	flash    *flashCanonical
	seals    <-chan flashSeal
	out      chan<- canonicalUpdate
	poll     *time.Timer
	hint     domain.Hash
	feeReads feeSchedule
	metric   metricReads
	// woke is the last wake not yet followed by a published block.
	woke time.Time
}

// catchUpLimit is the gap past which the follower reseeds at the tip: about
// 7,200 Arbitrum One blocks, which cost as many calls to catch up as a reseed.
const catchUpLimit = 30 * time.Minute

func followRPC(ctx context.Context, cfg config.Config, client *rpc.Client, specs []bootstrap.Spec, head domain.BlockRef, radius int16, wake <-chan domain.Hash, flash *flashCanonical, out chan<- canonicalUpdate, initialPools ...[]pool.State) error {
	f := &follower{ctx: ctx, cfg: cfg, client: client, metrics: telemetry.FromContext(ctx), specs: specs, head: head, radius: radius, wake: wake, flash: flash, out: out, feeReads: feeSchedule{every: cfg.Network.Blocks(canonicalFeeInterval)}, metric: metricReads{client: client}}
	if len(initialPools) > 0 {
		f.previous = initialPools[0]
	}
	if flash != nil {
		f.seals = flash.seals
		flash.accepted(head)
	}
	f.poll = time.NewTimer(cfg.RPCPoll)
	defer f.poll.Stop()
	return f.run()
}

func (f *follower) run() error {
	ctx, cfg, client := f.ctx, f.cfg, f.client
	var tip rpc.ExecutionBlock
	fromHint := false
	for ctx.Err() == nil {
		if f.flash != nil {
			now := time.Now()
			f.flash.drain(f.head, now)
			if next := f.flash.next(f.head); next != nil {
				fork, ok, err := f.streamBlock(next)
				if !ok {
					return err
				}
				// A forked stream block leaves the RPC header to the path below.
				tip, fromHint = fork, false
				continue
			}
			if f.flash.idle(f.head, now) {
				if err := f.wait(f.flash.wake(now)); err != nil {
					return nil
				}
				continue
			}
			if tip.Ref.Hash == (domain.Hash{}) && f.hint == (domain.Hash{}) {
				f.hint = f.flash.hint(f.head)
			}
		}
		if tip.Ref.Hash == (domain.Hash{}) {
			var err error
			fromHint = false
			if f.hint != (domain.Hash{}) {
				fromHint = true
				tip, err = waitExecutionBlock(ctx, client, f.hint, min(cfg.RPCWait, 500*time.Millisecond), min(cfg.RPCPoll, 100*time.Millisecond))
				f.hint = domain.Hash{}
				if errors.Is(err, rpc.ErrBlockUnavailable) {
					fromHint = false
					tip, err = waitExecutionBlock(ctx, client, "latest", cfg.RPCWait, cfg.RPCPoll)
				}
			} else {
				tip, err = waitExecutionBlock(ctx, client, "latest", cfg.RPCWait, cfg.RPCPoll)
			}
			if err != nil {
				return err
			}
			if f.flash != nil {
				f.flash.checked(tip.Ref.Number, time.Now())
			}
		}
		// A raw-stream hint names a block, not the canonical tip. Stale or orphan
		// hints cannot select a rewind target; recheck the moving head in that case.
		if fromHint && tip.Ref.Number <= f.head.Number {
			var err error
			tip, err = waitExecutionBlock(ctx, client, "latest", cfg.RPCWait, cfg.RPCPoll)
			if err != nil {
				return err
			}
			fromHint = false
		}
		// A matching latest hash already checks the accepted height. During catch-up,
		// the next child's parent proves ancestry without rereading that height.
		// Behind a stream, the RPC can also serve an older block of the accepted chain.
		if tip.Ref.Number == f.head.Number && tip.Ref.Hash == f.head.Hash || f.flash != nil && f.flash.behind(tip.Ref, f.head) {
			if err := f.waitNext(); err != nil {
				return nil
			}
			tip = rpc.ExecutionBlock{}
			continue
		}
		// Catch-up costs two reads a block; reading every pool at the tip costs
		// about 15,000 on Arbitrum One. Far behind, reseed instead.
		if cfg.Blocks == 0 && tip.Ref.Number > f.head.Number+cfg.Network.Blocks(catchUpLimit) {
			slog.Warn("far behind the tip; reading pools at the tip instead of catching up", "head", f.head.Number, "tip", tip.Ref.Number)
			if ok, err := f.reseed(tip); !ok {
				return err
			}
			tip = rpc.ExecutionBlock{}
			continue
		}
		block := tip
		if tip.Ref.Number > f.head.Number+1 {
			var err error
			block, err = waitExecutionBlock(ctx, client, fmt.Sprintf("0x%x", f.head.Number+1), cfg.RPCWait, cfg.RPCPoll)
			if err != nil {
				return err
			}
		}
		if tip.Ref.Number <= f.head.Number || block.Ref.ParentHash != f.head.Hash {
			if block.Ref.ParentHash != f.head.Hash && tip.Ref.Number > f.head.Number {
				// The remembered tip can itself belong to the detached branch. Recover
				// against a fresh canonical head, never the old catch-up target.
				var err error
				tip, err = waitExecutionBlock(ctx, client, "latest", cfg.RPCWait, cfg.RPCPoll)
				if err != nil {
					return err
				}
				fromHint = false
				if tip.Ref.Hash == f.head.Hash {
					if err := f.waitNext(); err != nil {
						return nil
					}
					tip = rpc.ExecutionBlock{}
					continue
				}
			}
			if ok, err := f.reseed(tip); !ok {
				return err
			}
			tip = rpc.ExecutionBlock{}
			continue
		}
		receipts, err := client.ExecutionReceipts(ctx, block, cfg.Network.Family)
		if err != nil {
			return err
		}
		// A reorg can happen during the receipts read. Below the tip the next
		// block's parent hash proves this one before any search runs on it
		// (catch-up defers search), so only the tip block is read again; a
		// second header per block made up a third of catch-up calls.
		if block.Ref.Number == tip.Ref.Number {
			stale, err := f.recheckTip(&tip, block)
			if err != nil {
				return err
			}
			if stale {
				tip = rpc.ExecutionBlock{}
				continue
			}
		}
		known := tip.Ref.Number
		if f.flash != nil {
			known = max(known, f.flash.tip())
		}
		if ok, err := f.publish(block, receiptTransactions(receipts), opFeeParams{key: rpc.BlockFeeParams(receipts)}, known, telemetry.CanonicalRPC); !ok {
			return err
		}
		if f.head.Number == tip.Ref.Number {
			if err := f.waitNext(); err != nil {
				return nil
			}
			tip = rpc.ExecutionBlock{}
		}
	}
	return nil
}

// streamBlock publishes the stream block that extends the head, after an RPC
// comparison when one is due. When the RPC chain has another block at that
// height it publishes nothing and returns the RPC header to continue from.
func (f *follower) streamBlock(next *flashBlock) (rpc.ExecutionBlock, bool, error) {
	block, transactions, feeParams := next.block, next.transactions, next.feeParams
	source := telemetry.CanonicalFlash
	if !f.flash.verifyDue() {
		f.flash.apply(*next)
	} else {
		header, err := waitExecutionBlock(f.ctx, f.client, fmt.Sprintf("0x%x", block.Ref.Number), f.cfg.RPCWait, min(f.cfg.RPCPoll, 100*time.Millisecond))
		if err != nil {
			return rpc.ExecutionBlock{}, false, err
		}
		var receipts []rpc.ExecutionReceipt
		if header.Ref.Hash == block.Ref.Hash {
			if receipts, err = f.client.ExecutionReceipts(f.ctx, header, f.cfg.Network.Family); err != nil {
				return rpc.ExecutionBlock{}, false, err
			}
		}
		if header.Ref.Hash == block.Ref.Hash && sameStreamBlock(block, header) && sameEffects(transactions, receipts) {
			f.metrics.Add(telemetry.Standard, telemetry.CanonicalVerified, 1)
			f.flash.verified()
			// Identical but for the fees only receipts carry. The stream's
			// signed transactions stay: MetricOmm quotes are priced by calldata.
			verified := receiptTransactions(receipts)
			for i := range verified {
				verified[i].Raw = transactions[i].Raw
			}
			block, transactions = header, verified
		} else {
			f.metrics.Add(telemetry.Standard, telemetry.CanonicalMismatch, 1)
			applied, complete := f.flash.takeApplied()
			f.flash.distrust()
			if header.Ref.Hash != block.Ref.Hash {
				slog.Warn("stream block is not the RPC block at its height", "block", block.Ref.Number, "stream", block.Ref.Hash.Hex(), "rpc", header.Ref.Hash.Hex())
				return header, true, nil
			}
			slog.Warn("stream block content differs from its RPC receipts", "block", block.Ref.Number, "unverified", len(applied), "difference", streamDifference(block, header, transactions, receipts))
			// The unverified stream blocks before it may differ as well.
			same := complete
			for i := 0; same && i < len(applied); i++ {
				if same, err = f.sameAsRPC(applied[i]); err != nil {
					return rpc.ExecutionBlock{}, false, err
				}
			}
			if !same {
				ok, err := f.reseed(header)
				return rpc.ExecutionBlock{}, ok, err
			}
			block, transactions, feeParams, source = header, receiptTransactions(receipts), opFeeParams{key: rpc.BlockFeeParams(receipts)}, telemetry.CanonicalRPC
		}
	}
	ok, err := f.publish(block, transactions, feeParams, f.flash.tip(), source)
	if errors.Is(err, errStreamAhead) {
		slog.Warn("stream block not served by the RPC; reading it by RPC", "block", block.Ref.Number)
		return rpc.ExecutionBlock{}, true, nil
	}
	return rpc.ExecutionBlock{}, ok, err
}

// sameAsRPC reads an applied stream block by hash and compares it with what
// was applied: two reads, against about 43,000 for a reseed.
func (f *follower) sameAsRPC(applied flashBlock) (bool, error) {
	header, err := f.client.ExecutionBlock(f.ctx, applied.block.Ref.Hash)
	if err != nil {
		return false, err
	}
	receipts, err := f.client.ExecutionReceipts(f.ctx, header, f.cfg.Network.Family)
	if err != nil {
		return false, err
	}
	if difference := streamDifference(applied.block, header, applied.transactions, receipts); difference != "" {
		slog.Warn("an unverified stream block differs from its RPC receipts", "block", applied.block.Ref.Number, "difference", difference)
		return false, nil
	}
	return true, nil
}

// A lagging backend gets about two seconds to reach a block the RPC served.
const (
	laggingBackendRetries = 80
	laggingBackendWait    = 25 * time.Millisecond
)

// errStreamAhead reports a stream block the RPC does not serve yet. Nothing
// was published; the RPC path takes the height instead.
var errStreamAhead = errors.New("stream block unavailable by RPC")

// publish hands one canonical block to the pool writer and advances the head.
// source counts its content: CanonicalFlash or CanonicalRPC. It reports false
// when the context ended.
func (f *follower) publish(block rpc.ExecutionBlock, transactions []ingest.Transaction, feeParams opFeeParams, tip uint64, source telemetry.Counter) (bool, error) {
	catchingUp := block.Ref.Number < tip
	nearTip := !catchingUp || tip-block.Ref.Number == 1
	var fees []pool.State
	var metric []metricRead
	var reads *rpc.ReadCache
	scheduled := f.cfg.SearchConfig != "" && (nearTip || f.cfg.Blocks > 0)
	if scheduled {
		due := f.feeReads.due(f.specs, block.Ref.Number, transactions)
		var err error
		fees, reads, err = canonicalFees(f.ctx, f.cfg, f.client, block, due, f.previous)
		if err != nil && source == telemetry.CanonicalFlash && f.ctx.Err() == nil {
			// The stream can seal a block before the RPC serves it, or seal one the
			// chain then drops. A block the RPC still lacks goes to the RPC path.
			if _, err = waitExecutionBlock(f.ctx, f.client, block.Ref.Hash, min(f.cfg.RPCWait, 2*time.Second), min(f.cfg.RPCPoll, 100*time.Millisecond)); err == nil {
				fees, reads, err = canonicalFees(f.ctx, f.cfg, f.client, block, due, f.previous)
			} else if errors.Is(err, rpc.ErrBlockUnavailable) {
				return false, errStreamAhead
			}
		}
		// This RPC served the block, so a read that finds it missing reached
		// a load-balanced backend still behind it.
		for attempt := 0; err != nil && source == telemetry.CanonicalRPC && rpc.IsBlockUnavailable(err) && attempt < laggingBackendRetries; attempt++ {
			select {
			case <-f.ctx.Done():
				return false, nil
			case <-time.After(laggingBackendWait):
			}
			fees, reads, err = canonicalFees(f.ctx, f.cfg, f.client, block, due, f.previous)
		}
		if err != nil {
			return false, fmt.Errorf("canonical fees at %d: %w", block.Ref.Number, err)
		}
		f.feeReads.mark(fees, block.Ref.Number)
		metric = f.metric.read(f.ctx, f.previous, block, transactions)
	}
	update := canonicalUpdate{metric: metric, block: block, feeParams: feeParams, tip: tip, transactions: transactions, catchingUp: catchingUp, nearTip: nearTip, accepted: make(chan struct{}), fees: fees, feesScheduled: scheduled, reads: reads, nextPools: make(chan []pool.State, 1)}
	f.metrics.Add(telemetry.Standard, source, 1)
	ready := time.Now()
	if !f.woke.IsZero() && !catchingUp {
		f.metrics.Observe(telemetry.Standard, telemetry.FollowFetch, ready.Sub(f.woke))
	}
	select {
	case <-f.ctx.Done():
		return false, nil
	case f.out <- update:
	}
	taken := time.Now()
	f.metrics.Observe(telemetry.Standard, telemetry.FollowQueue, taken.Sub(ready))
	select {
	case <-f.ctx.Done():
		return false, nil
	case <-update.accepted:
	}
	f.metrics.Observe(telemetry.Standard, telemetry.FollowAccept, time.Since(taken))
	select {
	case pools := <-update.nextPools:
		f.specs = specsFor(pools)
		f.previous = pools
	default:
	}
	f.head = block.Ref
	// The wake that announced this block is served; a later one, already
	// buffered in wake, restarts the fast follow.
	f.woke = time.Time{}
	if f.flash != nil {
		f.flash.accepted(f.head)
	}
	return true, nil
}

// reseed replaces the pool state at tip after a reorg, or after stream blocks
// that differ from the chain. It reports false when it published nothing.
func (f *follower) reseed(tip rpc.ExecutionBlock) (bool, error) {
	// Reading every pool is bootstrap work, counted apart from following.
	store, err := seed(rpc.WithCaller(f.ctx, rpc.CallerBootstrap), f.cfg, f.client, f.specs, tip, f.radius)
	if err != nil {
		return false, fmt.Errorf("RPC reorg bootstrap: %w", err)
	}
	f.previous = store.Pools()
	f.feeReads = feeSchedule{every: f.feeReads.every}
	update := canonicalUpdate{block: tip, reset: store, accepted: make(chan struct{})}
	if !sendCanonical(f.ctx, f.out, update) {
		return false, nil
	}
	f.head = tip.Ref
	if f.flash != nil {
		f.flash.reset()
		f.flash.accepted(f.head)
	}
	return true, nil
}

func (f *follower) wait(interval time.Duration) error {
	f.poll.Reset(interval)
	select {
	case <-f.ctx.Done():
		return f.ctx.Err()
	case <-f.poll.C:
	case f.hint = <-f.wake:
		f.woke = time.Now()
	case seal := <-f.seals:
		f.flash.add(seal, f.head, time.Now())
	}
	return nil
}

// recheckTip reads the chain again after the tip block's receipts and reports
// whether the block left it. Following the moving head it reads latest, which
// also names the next tip when one has arrived.
func (f *follower) recheckTip(tip *rpc.ExecutionBlock, block rpc.ExecutionBlock) (bool, error) {
	checkTag := fmt.Sprintf("0x%x", block.Ref.Number)
	if f.cfg.Blocks == 0 {
		checkTag = "latest"
	}
	check, err := f.client.ExecutionBlock(f.ctx, checkTag)
	if err != nil {
		return false, err
	}
	if checkTag == "latest" && check.Ref.Number > block.Ref.Number {
		*tip = check
		if check.Ref.Number == block.Ref.Number+1 && check.Ref.ParentHash == block.Ref.Hash {
			return false, nil
		}
		if check, err = f.client.ExecutionBlock(f.ctx, fmt.Sprintf("0x%x", block.Ref.Number)); err != nil {
			return false, err
		}
	}
	return check.Ref.Hash != block.Ref.Hash, nil
}

// waitNext waits for the next head, unless the stream already holds it.
func (f *follower) waitNext() error {
	if f.flash != nil {
		if f.flash.drain(f.head, time.Now()); f.flash.ready(f.head) {
			return nil
		}
	}
	return f.wait(f.cfg.RPCPoll)
}

func waitExecutionBlock(ctx context.Context, client *rpc.Client, tag any, budget, poll time.Duration) (rpc.ExecutionBlock, error) {
	deadline := time.Now().Add(budget)
	for {
		block, err := client.ExecutionBlock(ctx, tag)
		if !errors.Is(err, rpc.ErrBlockUnavailable) || budget <= 0 || !time.Now().Before(deadline) {
			return block, err
		}
		timer := time.NewTimer(min(poll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return rpc.ExecutionBlock{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func sendCanonical(ctx context.Context, out chan<- canonicalUpdate, update canonicalUpdate) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- update:
	}
	select {
	case <-ctx.Done():
		return false
	case <-update.accepted:
		return true
	}
}

// offerWake leaves the newest hint for the follower: an older pending one names
// a block it may already hold, and reading it would cost a header read.
func offerWake(wake chan domain.Hash, hash domain.Hash) {
	select {
	case <-wake:
	default:
	}
	select {
	case wake <- hash:
	default:
	}
}
