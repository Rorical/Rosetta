package discovery

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// LogSource fetches logs. rpc.Client satisfies it. Ingest's source interface
// is separate: discovery must not pull eth_getLogs into the live block path.
type LogSource interface {
	GetLogs(context.Context, rpc.LogFilter) ([]rpc.Log, error)
}

// Stats counts what one Discover call did. Counter names describe the first
// filter that rejected a pool, so they sum to at most Logs.
type Stats struct {
	// ScannedBlocks counts block numbers queried, once per enabled source.
	ScannedBlocks uint64
	// Logs counts logs returned by the log source, before decoding.
	Logs int
	// Decoded counts logs whose creation-event shape was valid.
	Decoded int
	// SkippedToken counts pools whose pair is not fully in QuoteTokens when
	// that list is non-empty.
	SkippedToken int
	// SkippedFee counts pools whose fee exceeds MaxFee. A zero MaxFee is valid
	// and keeps only pools whose supported fee directions are all zero.
	SkippedFee int
	// SkippedHooks counts V4 pools with a non-zero hooks address.
	SkippedHooks int
	// SkippedLiquidity counts pools whose liquidity read was rejected by the
	// node, is unreadable, is zero, or is below MinLiquidity.
	SkippedLiquidity int
	// SkippedInvalid counts malformed logs, a wrong pool ID, an unusable token
	// pair, a log from the wrong emitter, or a fee() read the node rejected.
	SkippedInvalid int
	// Selected counts discovered pools that survived filters and the cap.
	// Seeds are not counted.
	Selected int
}

// Result is the outcome of one scan. Pools holds the ranked discovered pools
// followed by the seeds; the seeds are also returned separately, sorted, so a
// caller can treat them as pinned.
type Result struct {
	Pools []Spec
	Seeds []Spec
	Stats Stats
}

// Discoverer scans creation logs and sizes the resulting pools.
type Discoverer struct {
	Logs   LogSource
	Calls  pool.ContractCaller
	Config Config
}

// New validates a Discoverer. The log source and caller must be non-nil and
// Config must pass the same checks LoadConfig applies. A nil MinLiquidity is
// treated as no floor.
func New(logs LogSource, calls pool.ContractCaller, config Config) (*Discoverer, error) {
	if logs == nil {
		return nil, fmt.Errorf("%w: nil log source", ErrInvalidConfig)
	}
	if calls == nil {
		return nil, fmt.Errorf("%w: nil contract caller", ErrInvalidConfig)
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	if config.MinLiquidity == nil {
		config.MinLiquidity = new(big.Int)
	}
	return &Discoverer{Logs: logs, Calls: calls, Config: config}, nil
}

// Discover scans [from, to] for pools created by the configured sources and
// returns them ranked by active liquidity at to. It runs in this order:
//
//  1. For every source, fetch creation logs in ScanChunkBlocks chunks.
//  2. Decode them, drop malformed ones, and apply the static filters: both
//     tokens in QuoteTokens when that list is non-empty, V4 hooks zero, a V4
//     ID that matches the key, and a usable token pair. Duplicate pool keys
//     collapse.
//  3. Read Slipstream fee() for every Slipstream candidate, because the event
//     does not carry the mutable fee, and drop fees above MaxFee.
//  4. Read active liquidity for every survivor at to, through StateView for
//     V4, and drop pools with zero active liquidity or below MinLiquidity.
//  5. Sort by liquidity descending, break ties with domain.ComparePoolKeys,
//     and cap at MaxPools.
//  6. Append the configured seeds, which skip every filter and the cap, in
//     key order and without duplicating a discovered pool.
//
// A log source error or a context cancellation aborts the scan. A node
// rejecting one fee() or liquidity() call drops only that pool and is counted;
// when a batch fails it is retried one call per candidate so one reverting
// pool cannot hide the rest. A transport failure during that fallback aborts.
// Batch callers group getter rounds when available: Slipstream fee, Camelot
// metadata and liquidity are at most three rounds, separate from log requests.
// Otherwise calls run sequentially.
func (d *Discoverer) Discover(ctx context.Context, from, to uint64) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if from > to {
		return Result{}, fmt.Errorf("discovery: block range %d-%d is reversed", from, to)
	}

	state := &scanState{
		quotes: make(map[domain.Address]struct{}, len(d.Config.QuoteTokens)),
		seen:   make(map[domain.PoolKey]struct{}),
	}
	for _, quote := range d.Config.QuoteTokens {
		state.quotes[quote] = struct{}{}
	}

	var candidates []*candidate
	for _, emitter := range d.Config.Emitters() {
		found, err := d.scanEmitter(ctx, emitter, from, to, state)
		if err != nil {
			return Result{}, err
		}
		candidates = append(candidates, found...)
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	candidates, err := d.readFees(ctx, candidates, to, &state.stats)
	if err != nil {
		return Result{}, err
	}
	candidates, err = d.readCamelotMetadata(ctx, candidates, to, &state.stats)
	if err != nil {
		return Result{}, err
	}
	candidates, err = d.readLiquidity(ctx, candidates, to, &state.stats)
	if err != nil {
		return Result{}, err
	}

	// Liquidity descending, then the canonical key order so equal pools still
	// come out in one fixed order.
	slices.SortFunc(candidates, func(a, b *candidate) int {
		if diff := b.liquidity.Cmp(a.liquidity); diff != 0 {
			return diff
		}
		return domain.ComparePoolKeys(a.spec.PoolKey(), b.spec.PoolKey())
	})
	if len(candidates) > d.Config.MaxPools {
		candidates = candidates[:d.Config.MaxPools]
	}
	state.stats.Selected = len(candidates)

	pools := make([]Spec, 0, len(candidates)+len(d.Config.Seeds))
	inPools := make(map[domain.PoolKey]struct{}, len(candidates)+len(d.Config.Seeds))
	for _, c := range candidates {
		pools = append(pools, c.spec)
		inPools[c.spec.PoolKey()] = struct{}{}
	}

	seeds := slices.Clone(d.Config.Seeds)
	slices.SortFunc(seeds, func(a, b Spec) int {
		return domain.ComparePoolKeys(a.PoolKey(), b.PoolKey())
	})
	for _, seed := range seeds {
		key := seed.PoolKey()
		if _, ok := inPools[key]; ok {
			continue
		}
		inPools[key] = struct{}{}
		pools = append(pools, seed)
	}

	return Result{Pools: pools, Seeds: seeds, Stats: state.stats}, nil
}

// readFees resolves Slipstream's mutable fee and drops pools priced out of the
// configured range. Other kinds already carry their fee from the log.
func (d *Discoverer) readFees(ctx context.Context, candidates []*candidate, block uint64, stats *Stats) ([]*candidate, error) {
	var err error
	candidates, err = d.readVelodromeFees(ctx, candidates, block, stats)
	if err != nil {
		return nil, err
	}
	var targets []*candidate
	for _, c := range candidates {
		if c.spec.Kind == domain.PoolKindSlipstream {
			targets = append(targets, c)
		}
	}
	if len(targets) == 0 {
		return candidates, nil
	}

	tag := fmt.Sprintf("0x%x", block)
	calls := make([]pool.ContractCall, len(targets))
	for i, c := range targets {
		calls[i] = pool.ContractCall{To: c.spec.Address, Data: feeCall, Block: tag}
	}
	outputs, failures, err := d.callRound(ctx, calls)
	if err != nil {
		return nil, fmt.Errorf("discovery: read slipstream fees: %w", err)
	}

	drop := make(map[*candidate]struct{}, len(targets))
	for i, c := range targets {
		if failures[i] != nil {
			if !isCallError(failures[i]) {
				return nil, fmt.Errorf("discovery: read slipstream fees: %w", failures[i])
			}
			stats.SkippedInvalid++
			drop[c] = struct{}{}
			continue
		}
		fee, err := parseFeeResult(outputs[i])
		if err != nil || fee == 0 || fee > d.Config.MaxFee {
			stats.SkippedFee++
			drop[c] = struct{}{}
			continue
		}
		c.spec.Fee = fee
	}
	return without(candidates, drop), nil
}

// readLiquidity attaches active liquidity at block and drops pools below the
// configured floor.
func (d *Discoverer) readLiquidity(ctx context.Context, candidates []*candidate, block uint64, stats *Stats) ([]*candidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}

	tag := fmt.Sprintf("0x%x", block)
	calls := make([]pool.ContractCall, len(candidates))
	for i, c := range candidates {
		calls[i] = pool.ContractCall{
			To:    liquidityTarget(c.spec, d.Config.Deployments),
			Data:  liquidityCallData(c.spec),
			Block: tag,
		}
	}
	outputs, failures, err := d.callRound(ctx, calls)
	if err != nil {
		return nil, fmt.Errorf("discovery: read liquidity: %w", err)
	}

	floor := d.Config.minLiquidity()
	drop := make(map[*candidate]struct{})
	for i, c := range candidates {
		if failures[i] != nil {
			if !isCallError(failures[i]) {
				return nil, fmt.Errorf("discovery: read liquidity: %w", failures[i])
			}
			stats.SkippedLiquidity++
			drop[c] = struct{}{}
			continue
		}
		liquidity, err := parsePoolLiquidity(c.spec, outputs[i])
		// Zero active liquidity means no initialized position, so the pool
		// cannot fill a quote; drop it even when the floor is zero.
		if err != nil || liquidity.Sign() == 0 || liquidity.Cmp(floor) < 0 {
			stats.SkippedLiquidity++
			drop[c] = struct{}{}
			continue
		}
		c.liquidity = liquidity
	}
	return without(candidates, drop), nil
}

// callRound runs one read round. It returns one output or one failure per
// call. A batch failure falls back to one call per candidate, because the
// batch interface reports only the first per-call error and one reverting pool
// must not hide the others. The returned error covers only context
// cancellation and a batch that returns the wrong number of results; the
// readers decide whether a recorded per-call failure is transport-level.
func (d *Discoverer) callRound(ctx context.Context, calls []pool.ContractCall) ([]string, []error, error) {
	if len(calls) == 0 {
		return nil, nil, nil
	}
	if batch, ok := d.Calls.(pool.BatchContractCaller); ok {
		outputs, err := batch.CallContracts(ctx, calls)
		if err == nil {
			if len(outputs) != len(calls) {
				return nil, nil, fmt.Errorf("eth_call batch returned %d results for %d calls", len(outputs), len(calls))
			}
			return outputs, make([]error, len(calls)), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
	}

	outputs := make([]string, len(calls))
	failures := make([]error, len(calls))
	for i, call := range calls {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := d.Calls.CallContract(ctx, call.To, call.Data, call.Block, &outputs[i]); err != nil {
			failures[i] = err
		}
	}
	return outputs, failures, nil
}

// isCallError reports whether err is the node answering one call, as a revert
// does, rather than the request failing on the way. Only the former is safe to
// absorb per pool; a transport failure aborts the round.
func isCallError(err error) bool {
	var rpcErr *rpc.RPCError
	return errors.As(err, &rpcErr) || errors.Is(err, pool.ErrCallRejected)
}

// without removes dropped candidates while preserving order.
func without(candidates []*candidate, drop map[*candidate]struct{}) []*candidate {
	if len(drop) == 0 {
		return candidates
	}
	kept := make([]*candidate, 0, len(candidates)-len(drop))
	for _, c := range candidates {
		if _, ok := drop[c]; ok {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}
