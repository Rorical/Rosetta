// Package bootstrap seeds the in-memory store from on-chain pool state at a
// fixed block and appends pools to an existing store at its current head. The
// protocol loaders issue the eth_calls. A failed seed restores the pre-seed
// snapshot, and a failed append rolls back its frame, so a partially written
// set is never observed.
package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/solidlyv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/dex/wrap"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// maxFeePips is 100% in hundredths of a basis point, the fee unit shared by
// both loaders. The loaders keep their own unexported copy; a spec outside the
// range is rejected here so no pool is touched before the error surfaces.
const maxFeePips uint32 = 1_000_000

// Sentinel errors for callers that need to branch on the failure class.
var (
	// ErrInvalidSpec reports unusable Seed input: a nil caller or store, an
	// empty spec list or block, a malformed pool spec, or a duplicate address.
	ErrInvalidSpec = errors.New("bootstrap: invalid spec")
	// ErrUnsupportedKind reports a spec whose pool kind has no loader.
	ErrUnsupportedKind = errors.New("bootstrap: unsupported pool kind")
)

// Standalone callers default to the Arbitrum One manager. Runtime callers
// supply the manager through their chain-bound contract caller.
var poolManagerAddress = uniswapv4.PoolManager()

// Spec identifies one pool to seed. Fee and TickSpacing are immutable pool
// parameters that the on-chain read calls do not return for V2 and the
// V3-shaped families, so the caller supplies them from pool creation data. A V4
// pool ignores the configured fee for pricing (slot0 supplies the LP fee) but
// still needs it and the tick spacing to reproduce the PoolID hash; a zero LP
// fee is valid for V4. PoolID must match those fields or the spec is rejected.
// PoolID and Hooks are zero for every other kind.
type Spec struct {
	Address     domain.Address
	Token0      domain.Address
	Token1      domain.Address
	Kind        domain.PoolKind
	PoolID      domain.Hash
	Hooks       domain.Address
	Fee         uint32
	TickSpacing int32
	// Shallow skips the V3 deep tick load. It is only valid for
	// domain.PoolKindV3; other kinds are rejected. A shallow pool can price a
	// swap that stays in its current tick; a quote that would move to another
	// tick fails with math.ErrOutOfWindow because no tick data was loaded. A
	// shallow seed at tick 0 is rejected: its zero-width window cannot be
	// distinguished from the "window unknown" sentinel.
	Shallow bool
	// Retained immutable metadata avoids repeated getters on canonical fee reads.
	Velodrome *pool.VelodromeState
	// Metric is a MetricOmm pool's registration: its immutables, oracle and
	// quote extension. The kind has no factory lookup, so the catalog fills
	// it; storage words in it are ignored.
	Metric *pool.MetricState
}

// key returns the store key of the pool the spec describes.
func (s Spec) key() domain.PoolKey {
	return domain.PoolKey{Address: s.Address, Kind: s.Kind, PoolID: s.PoolID, Hooks: s.Hooks}
}

// label renders a spec for an error message. V4 pools share the PoolManager
// address, so the pool ID is part of the name when it is set.
func (s Spec) label() string {
	if s.PoolID == (domain.Hash{}) {
		return s.Address.Hex()
	}
	return s.Address.Hex() + "/" + s.PoolID.Hex()
}

// Seed reads every spec at ref and stores the resulting shallow state. V3,
// Camelot V3, Slipstream, and V4 pools also get deep state within
// pool.WindowRadius(spacing, wordRadius) bitmap words of the current tick, so
// wordRadius is a floor under the price coverage target. Every loader call
// uses ref.Number as a fixed hex quantity, each loaded deep state is stamped with the full ref, and
// after all specs apply the store head is set to ref exactly once with
// BeginBlock/Commit. A zero-number or zero-hash ref is rejected. All specs are
// validated before any call or write.
//
// If a load, write, or head update fails, the store is restored to its snapshot
// from before seeding and the original error is returned, joined with the
// restore error when restoring also fails.
func Seed(ctx context.Context, caller pool.ContractCaller, store *state.Store, specs []Spec, ref domain.BlockRef, wordRadius int16) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSeed(caller, store, specs, ref); err != nil {
		return err
	}

	snapshot := store.Checkpoint()
	if err := seedAll(ctx, caller, store, specs, ref, wordRadius); err != nil {
		return restoreSeedSnapshot(store, snapshot, err)
	}
	if err := store.BeginBlock(ref); err != nil {
		return restoreSeedSnapshot(store, snapshot,
			fmt.Errorf("bootstrap: record block %d: %w", ref.Number, err))
	}
	store.Commit()
	return nil
}

// restoreSeedSnapshot puts the store back before seeding and joins a restore
// failure with the seeding error.
func restoreSeedSnapshot(store *state.Store, snapshot state.Snapshot, cause error) error {
	if restoreErr := store.Restore(snapshot); restoreErr != nil {
		return errors.Join(cause, fmt.Errorf("bootstrap: restore snapshot: %w", restoreErr))
	}
	return cause
}

func seedPool(ctx context.Context, caller pool.ContractCaller, sink poolSink, spec Spec, block string, ref domain.BlockRef, wordRadius int16) error {
	wordRadius = pool.WindowRadius(spec.TickSpacing, wordRadius)
	switch spec.Kind {
	case domain.PoolKindVelodromeV2:
		loaded, err := LoadShallow(ctx, caller, spec, block)
		if err != nil {
			return fmt.Errorf("bootstrap: load Velodrome pool %s: %w", spec.Address, err)
		}
		return sink.Upsert(loaded)
	case domain.PoolKindV2:
		return seedV2(ctx, caller, sink, spec, block)
	case domain.PoolKindV3:
		return seedV3(ctx, caller, sink, spec, block, ref, wordRadius)
	case domain.PoolKindCamelotV3:
		return seedCamelotV3(ctx, caller, sink, spec, block, ref, wordRadius)
	case domain.PoolKindSlipstream:
		return seedSlipstream(ctx, caller, sink, spec, block, ref, wordRadius)
	case domain.PoolKindSolidlyV3:
		return seedSolidlyV3(ctx, caller, sink, spec, block, ref, wordRadius)
	case domain.PoolKindV4:
		return seedV4(ctx, caller, sink, spec, block, ref, wordRadius)
	case domain.PoolKindMetricOmm:
		// Registered without a read: the pool's words, extension state and
		// quote are read together at one canonical block by the runtime,
		// which repeats that read after every swap anyway. Until then the
		// pool is stale and quotes nothing.
		return sink.Upsert(pool.State{Key: spec.key(), Token0: spec.Token0, Token1: spec.Token1, Metric: metricomm.Unloaded(spec.Metric)})
	case domain.PoolKindWrap:
		// Nothing to read: the wrapped native token converts 1:1.
		return sink.Upsert(wrap.State(spec.Address))
	default:
		// validateSeed rejects other kinds; this keeps the switch exhaustive.
		return fmt.Errorf("%w: %d for pool %s", ErrUnsupportedKind, spec.Kind, spec.Address)
	}
}

func seedV2(ctx context.Context, caller pool.ContractCaller, sink poolSink, spec Spec, block string) error {
	loaded, err := uniswapv2.Load(ctx, caller, uniswapv2.Spec{
		Address: spec.Address,
		Token0:  spec.Token0,
		Token1:  spec.Token1,
		Fee:     spec.Fee,
	}, block)
	if err != nil {
		return fmt.Errorf("bootstrap: load v2 pool %s: %w", spec.Address, err)
	}
	loaded.Key = spec.key()
	if err := sink.Upsert(loaded); err != nil {
		return fmt.Errorf("bootstrap: store v2 pool %s: %w", spec.Address, err)
	}
	return nil
}

func seedV3(ctx context.Context, caller pool.ContractCaller, sink poolSink, spec Spec, block string, ref domain.BlockRef, wordRadius int16) error {
	loaderSpec := uniswapv3.Spec{
		Address:     spec.Address,
		Token0:      spec.Token0,
		Token1:      spec.Token1,
		Fee:         spec.Fee,
		TickSpacing: spec.TickSpacing,
	}
	if spec.Shallow {
		shallow, err := uniswapv3.LoadShallow(ctx, caller, loaderSpec, block)
		if err != nil {
			return fmt.Errorf("bootstrap: load v3 pool %s: %w", spec.Address, err)
		}
		shallow.Key = spec.key()
		// A zero-width window at tick 0 is the same value as the "window
		// unknown" sentinel SwapV3ExactIn checks, so it would not be enforced.
		// Fail closed instead of seeding a pool that can misquote.
		if shallow.Tick == 0 {
			return fmt.Errorf("bootstrap: load v3 pool %s: shallow seed at tick 0 has no enforceable window; use a full seed",
				spec.Address)
		}
		if err := sink.Upsert(shallow); err != nil {
			return fmt.Errorf("bootstrap: store v3 pool %s: %w", spec.Address, err)
		}
		// No tick data was loaded, so pin the loaded window to the current
		// tick. A quote that would leave it returns math.ErrOutOfWindow
		// instead of pricing constant liquidity across unloaded ticks.
		if err := sink.SetDeep(shallow.Key, pool.DeepState{
			WindowMin: shallow.Tick,
			WindowMax: shallow.Tick,
		}); err != nil {
			return fmt.Errorf("bootstrap: store v3 shallow window %s: %w", spec.Address, err)
		}
		return nil
	}
	loaded, err := uniswapv3.Load(ctx, caller, loaderSpec, block, wordRadius)
	if err != nil {
		return fmt.Errorf("bootstrap: load v3 pool %s: %w", spec.Address, err)
	}
	key := spec.key()
	loaded.State.Key = key
	loaded.Deep.LoadedBlock = ref
	// Deep state is not part of the shallow graph, so SetDeep must run after
	// Upsert and must not mark the pool dirty.
	if err := sink.Upsert(loaded.State); err != nil {
		return fmt.Errorf("bootstrap: store v3 pool %s: %w", spec.Address, err)
	}
	if err := sink.SetDeep(key, loaded.Deep); err != nil {
		return fmt.Errorf("bootstrap: store v3 deep state %s: %w", spec.Address, err)
	}
	return nil
}

func seedCamelotV3(ctx context.Context, caller pool.ContractCaller, sink poolSink, spec Spec, block string, ref domain.BlockRef, wordRadius int16) error {
	loaded, err := camelotv3.Load(ctx, caller, camelotv3.Spec{
		Address:     spec.Address,
		Token0:      spec.Token0,
		Token1:      spec.Token1,
		Fee:         spec.Fee,
		TickSpacing: spec.TickSpacing,
	}, block)
	if err != nil {
		return fmt.Errorf("bootstrap: load camelot v3 pool %s: %w", spec.Address, err)
	}
	deep, err := camelotv3.LoadDeep(ctx, caller, loaded, block, wordRadius)
	if err != nil {
		return fmt.Errorf("bootstrap: load camelot v3 deep state %s: %w", spec.Address, err)
	}
	deep.LoadedBlock = ref
	key := spec.key()
	loaded.Key = key
	if err := sink.Upsert(loaded); err != nil {
		return fmt.Errorf("bootstrap: store camelot v3 pool %s: %w", spec.Address, err)
	}
	if err := sink.SetDeep(key, deep); err != nil {
		return fmt.Errorf("bootstrap: store camelot v3 deep state %s: %w", spec.Address, err)
	}
	return nil
}

func seedSlipstream(ctx context.Context, caller pool.ContractCaller, sink poolSink, spec Spec, block string, ref domain.BlockRef, wordRadius int16) error {
	loaded, deep, err := slipstream.Load(ctx, caller, slipstream.Spec{
		Address:     spec.Address,
		Token0:      spec.Token0,
		Token1:      spec.Token1,
		Fee:         spec.Fee,
		TickSpacing: spec.TickSpacing,
	}, block, wordRadius)
	if err != nil {
		return fmt.Errorf("bootstrap: load slipstream pool %s: %w", spec.Address, err)
	}
	deep.LoadedBlock = ref
	key := spec.key()
	loaded.Key = key
	if err := sink.Upsert(loaded); err != nil {
		return fmt.Errorf("bootstrap: store slipstream pool %s: %w", spec.Address, err)
	}
	if err := sink.SetDeep(key, deep); err != nil {
		return fmt.Errorf("bootstrap: store slipstream deep state %s: %w", spec.Address, err)
	}
	return nil
}

// seedSolidlyV3 loads the V3-shaped state; the fee comes from slot0, not the
// creation tier in spec.
func seedSolidlyV3(ctx context.Context, caller pool.ContractCaller, sink poolSink, spec Spec, block string, ref domain.BlockRef, wordRadius int16) error {
	loaded, deep, err := solidlyv3.Load(ctx, caller, solidlyv3.Spec{Address: spec.Address, Token0: spec.Token0, Token1: spec.Token1, TickSpacing: spec.TickSpacing}, block, wordRadius)
	if err != nil {
		return fmt.Errorf("bootstrap: load Solidly V3 pool %s: %w", spec.Address, err)
	}
	deep.LoadedBlock = ref
	key := spec.key()
	loaded.Key = key
	if err := sink.Upsert(loaded); err != nil {
		return fmt.Errorf("bootstrap: store Solidly V3 pool %s: %w", spec.Address, err)
	}
	if err := sink.SetDeep(key, deep); err != nil {
		return fmt.Errorf("bootstrap: store Solidly V3 deep state %s: %w", spec.Address, err)
	}
	return nil
}

// seedV4 reads slot0 and active liquidity through StateView and the tick words
// around the current tick. The spec's fee is only the configured LP fee: slot0
// lpFee replaces it, and the packed protocol fee is kept separate because it is
// directional.
func seedV4(ctx context.Context, caller pool.ContractCaller, sink poolSink, spec Spec, block string, ref domain.BlockRef, wordRadius int16) error {
	loaded, err := uniswapv4.Load(ctx, caller, uniswapv4.Spec{
		PoolID:      spec.PoolID,
		Token0:      spec.Token0,
		Token1:      spec.Token1,
		TickSpacing: spec.TickSpacing,
		Hooks:       spec.Hooks,
	}, block)
	if err != nil {
		return fmt.Errorf("bootstrap: load v4 pool %s: %w", spec.label(), err)
	}
	key := spec.key()
	loaded.Key = key
	if err := sink.Upsert(loaded); err != nil {
		return fmt.Errorf("bootstrap: store v4 pool %s: %w", spec.label(), err)
	}
	deep, err := uniswapv4.LoadDeep(ctx, caller, loaded, block, wordRadius)
	if err != nil {
		return fmt.Errorf("bootstrap: load v4 deep state %s: %w", spec.label(), err)
	}
	deep.LoadedBlock = ref
	if err := sink.SetDeep(key, deep); err != nil {
		return fmt.Errorf("bootstrap: store v4 deep state %s: %w", spec.label(), err)
	}
	return nil
}

func validateSeed(caller pool.ContractCaller, store *state.Store, specs []Spec, ref domain.BlockRef) error {
	if caller == nil {
		return fmt.Errorf("%w: nil contract caller", ErrInvalidSpec)
	}
	if store == nil {
		return fmt.Errorf("%w: nil store", ErrInvalidSpec)
	}
	if len(specs) == 0 {
		return fmt.Errorf("%w: no pool specs", ErrInvalidSpec)
	}
	if err := validateRef(ref); err != nil {
		return err
	}
	manager, _ := uniswapv4.Deployment(caller)
	return validateSpecs(specs, manager)
}

// validateRef rejects the zero block identity every seed and append needs.
func validateRef(ref domain.BlockRef) error {
	if ref.Number == 0 {
		return fmt.Errorf("%w: zero block number", ErrInvalidSpec)
	}
	if ref.Hash == (domain.Hash{}) {
		return fmt.Errorf("%w: zero block hash", ErrInvalidSpec)
	}
	return nil
}

// validateSpecs checks every spec before any call or write.
func validateSpecs(specs []Spec, managers ...domain.Address) error {
	// V4 pools share the PoolManager address, so identity is the pool key, not
	// the address. Any other address still names one pool across all non-V4
	// families, and no non-V4 pool may sit at the PoolManager address.
	seenKeys := make(map[domain.PoolKey]struct{}, len(specs))
	nonV4Addresses := make(map[domain.Address]struct{}, len(specs))
	v4Managers := make(map[domain.Address]struct{}, 1)
	for i, spec := range specs {
		if err := validateSpec(spec, managers...); err != nil {
			return fmt.Errorf("bootstrap: spec %d: %w", i, err)
		}
		key := spec.key()
		if _, ok := seenKeys[key]; ok {
			return fmt.Errorf("%w: spec %d: duplicate pool %s", ErrInvalidSpec, i, spec.label())
		}
		seenKeys[key] = struct{}{}

		if key.IsV4() {
			if _, ok := nonV4Addresses[spec.Address]; ok {
				return fmt.Errorf("%w: spec %d: v4 pool %s shares an address with a non-v4 pool",
					ErrInvalidSpec, i, spec.Address)
			}
			v4Managers[spec.Address] = struct{}{}
			continue
		}
		if _, ok := v4Managers[spec.Address]; ok {
			return fmt.Errorf("%w: spec %d: pool %s shares an address with a v4 pool",
				ErrInvalidSpec, i, spec.Address)
		}
		if _, ok := nonV4Addresses[spec.Address]; ok {
			return fmt.Errorf("%w: spec %d: duplicate pool address %s", ErrInvalidSpec, i, spec.Address)
		}
		nonV4Addresses[spec.Address] = struct{}{}
	}
	return nil
}

func validateSpec(spec Spec, managers ...domain.Address) error {
	if spec.Address == (domain.Address{}) {
		return fmt.Errorf("%w: zero pool address", ErrInvalidSpec)
	}
	// Only the V3 loader has a shallow path; a flag that no seed path honors
	// would silently run the full load instead.
	if spec.Shallow && spec.Kind != domain.PoolKindV3 {
		return fmt.Errorf("%w: shallow load for pool %s kind %d", ErrInvalidSpec, spec.Address, spec.Kind)
	}
	if spec.Kind == domain.PoolKindV4 {
		return validateV4Spec(spec, managers...)
	}
	if spec.Kind == domain.PoolKindMetricOmm {
		return validateMetricSpec(spec)
	}
	if spec.Kind == domain.PoolKindWrap {
		if spec.TickSpacing != 0 || spec.Velodrome != nil || spec.Metric != nil {
			return fmt.Errorf("%w: wrap %s has fields of another kind", ErrInvalidSpec, spec.Address)
		}
		if err := wrap.Validate(pool.State{Key: spec.key(), Token0: spec.Token0, Token1: spec.Token1, Fee: spec.Fee}); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSpec, err)
		}
		return nil
	}
	switch spec.Kind {
	case domain.PoolKindV2, domain.PoolKindV3, domain.PoolKindCamelotV3, domain.PoolKindSlipstream, domain.PoolKindVelodromeV2, domain.PoolKindSolidlyV3:
	default:
		return fmt.Errorf("%w: %d for pool %s", ErrUnsupportedKind, spec.Kind, spec.Address)
	}
	if spec.PoolID != (domain.Hash{}) || spec.Hooks != (domain.Address{}) {
		return fmt.Errorf("%w: pool %s kind %d has V4 identity fields",
			ErrInvalidSpec, spec.Address, spec.Kind)
	}
	if err := validateTokens(spec); err != nil {
		return err
	}
	if spec.Fee == 0 && spec.Kind != domain.PoolKindVelodromeV2 {
		return fmt.Errorf("%w: missing fee", ErrInvalidSpec)
	}
	if spec.Fee >= maxFeePips {
		return fmt.Errorf("%w: fee %d pips outside [1, %d)", ErrInvalidSpec, spec.Fee, maxFeePips)
	}
	if spec.Kind.UsesTicks() && spec.TickSpacing <= 0 {
		return fmt.Errorf("%w: missing tick spacing", ErrInvalidSpec)
	}
	return nil
}

// validateMetricSpec checks a MetricOmm registration. The pool has no fee
// tier or tick spacing; its scale multipliers and bin grid come from
// getImmutables through the catalog.
func validateMetricSpec(spec Spec) error {
	if spec.PoolID != (domain.Hash{}) || spec.Hooks != (domain.Address{}) || spec.Fee != 0 || spec.TickSpacing != 0 {
		return fmt.Errorf("%w: MetricOmm pool %s has fields of another kind", ErrInvalidSpec, spec.Address)
	}
	if err := validateTokens(spec); err != nil {
		return err
	}
	m := spec.Metric
	if m == nil || m.Token0Scale == nil || m.Token0Scale.Sign() <= 0 || m.Token1Scale == nil || m.Token1Scale.Sign() <= 0 ||
		m.HighestBin < m.LowestBin || m.Oracle == (domain.Address{}) || m.QuoteExtension == (domain.Address{}) {
		return fmt.Errorf("%w: MetricOmm pool %s lacks its registration", ErrInvalidSpec, spec.Address)
	}
	return nil
}

// validateV4Spec checks the identity a V4 pool needs before any StateView call.
// Fee only participates in the pool ID because the loader replaces it with
// slot0 lpFee: zero is a valid LP fee, while maxFeePips and above are rejected
// as out of range. That ID is
// keccak256(abi.encode(currency0, currency1, fee, tickSpacing, hooks)), and the
// loader reads ticks with the configured spacing, so an ID that does not match
// the rest of the key would read the right pool and decode its bitmap at the
// wrong spacing. Rejecting the mismatch here turns a silent mis-quote into a
// startup error.
func validateV4Spec(spec Spec, managers ...domain.Address) error {
	manager := poolManagerAddress
	if len(managers) > 0 {
		manager = managers[0]
	}
	if manager == (domain.Address{}) || spec.Address != manager {
		return fmt.Errorf("%w: v4 pool address %s is not the PoolManager %s",
			ErrInvalidSpec, spec.Address, manager)
	}
	if spec.PoolID == (domain.Hash{}) {
		return fmt.Errorf("%w: v4 pool has a zero pool ID", ErrInvalidSpec)
	}
	if spec.Hooks != (domain.Address{}) {
		return fmt.Errorf("%w: hooked v4 pool %s: %w",
			ErrInvalidSpec, spec.label(), uniswapv4.ErrUnsupportedHooks)
	}
	if err := validateTokens(spec); err != nil {
		return err
	}
	// The PoolId hash commits to currency0 < currency1, so a reverse-ordered
	// config would not match the pool it names.
	if bytes.Compare(spec.Token0[:], spec.Token1[:]) >= 0 {
		return fmt.Errorf("%w: token0 %s must sort before token1 %s",
			ErrInvalidSpec, spec.Token0, spec.Token1)
	}
	if spec.TickSpacing <= 0 {
		return fmt.Errorf("%w: missing tick spacing", ErrInvalidSpec)
	}
	if spec.Fee >= maxFeePips {
		return fmt.Errorf("%w: v4 pool %s fee %d pips outside [0, %d)",
			ErrInvalidSpec, spec.label(), spec.Fee, maxFeePips)
	}
	expected, err := uniswapv4.PoolID(spec.Token0, spec.Token1, spec.Fee, spec.TickSpacing, spec.Hooks)
	if err != nil {
		return fmt.Errorf("%w: v4 pool %s: %v", ErrInvalidSpec, spec.label(), err)
	}
	if expected != spec.PoolID {
		return fmt.Errorf("%w: v4 pool %s: pool ID does not match fee %d, tick spacing %d, hooks %s (want %s)",
			ErrInvalidSpec, spec.label(), spec.Fee, spec.TickSpacing, spec.Hooks, expected.Hex())
	}
	return nil
}

func validateTokens(spec Spec) error {
	// V4 uses address(0) for native ETH as currency0. Every other pool, and
	// currency1 in all kinds, is a contract token.
	if spec.Token0 == (domain.Address{}) && spec.Kind != domain.PoolKindV4 {
		return fmt.Errorf("%w: zero token0", ErrInvalidSpec)
	}
	if spec.Token1 == (domain.Address{}) {
		return fmt.Errorf("%w: zero token1", ErrInvalidSpec)
	}
	if spec.Token0 == spec.Token1 {
		return fmt.Errorf("%w: token0 and token1 are equal", ErrInvalidSpec)
	}
	return nil
}
