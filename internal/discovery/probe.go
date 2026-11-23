package discovery

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// ActiveProbe keeps a bounded negative cooldown on the discovery worker. Swap
// emitters are hints; only a configured factory's authoritative lookup admits one.
// V4 metadata comes from a separate, exact-ID Initialize backfill.
type ActiveProbe struct {
	After                      map[domain.PoolKey]uint64
	Report                     []Assessment
	historicalRangeUnsupported bool
	v4Scans                    map[domain.Address]*v4Scan // unfinished chunked lookups
}
type ProbeOptions struct {
	Emitters           []Emitter
	Activity           map[domain.PoolKey]Activity
	Known              []domain.PoolKey
	Metadata           []Creation
	Seen               map[domain.PoolKey]bool
	Anchor             domain.BlockRef
	Limit              int
	MinSwaps, Cooldown uint64
	// HistoryChunk, when nonzero, splits historical lookups into ranges of
	// this many blocks (Config.HistoryChunkBlocks).
	HistoryChunk uint64
	// HistoryChunksPerRound bounds the ranges one round queries; an
	// unfinished lookup resumes next round. Zero means no bound.
	HistoryChunksPerRound int
}

func (p *ActiveProbe) Run(ctx context.Context, caller pool.ContractCaller, options ProbeOptions) ([]Creation, error) {
	return p.RunWithLogs(ctx, caller, nil, options)
}

func (p *ActiveProbe) RunWithLogs(ctx context.Context, caller pool.ContractCaller, logs LogSource, options ProbeOptions) ([]Creation, error) {
	p.Report = nil
	if options.Limit <= 0 {
		return nil, nil
	}
	if p.After == nil {
		p.After = map[domain.PoolKey]uint64{}
	}
	for key, until := range p.After {
		if until <= options.Anchor.Number {
			delete(p.After, key)
		}
	}
	knownV4 := map[domain.PoolKey]bool{}
	known := map[domain.Address]bool{}
	for _, row := range options.Metadata {
		if !row.Spec.PoolKey().IsV4() {
			// Activity keys carry no protocol kind. A recorded pool outside the
			// catalog keeps its selection reason instead of being re-probed.
			known[row.Spec.Address] = true
			continue
		}
		key := row.Spec.PoolKey()
		key.Hooks = domain.Address{}
		knownV4[key] = true
		if row.Spec.Hooks != (domain.Address{}) {
			p.assess(key, "unsupported_v4_hooks", options)
		}
	}
	for _, key := range options.Known {
		if key.IsV4() {
			// A loaded V4 pool needs no metadata; resolving it again would
			// admit a second copy into the graph.
			knownV4[key] = true
		} else {
			known[key.Address] = true
		}
	}
	keys := make([]domain.PoolKey, 0, len(options.Activity))
	for key := range options.Activity {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b domain.PoolKey) int {
		as, bs := options.Activity[a], options.Activity[b]
		if as.MultiSwapUses > bs.MultiSwapUses {
			return -1
		}
		if as.MultiSwapUses < bs.MultiSwapUses {
			return 1
		}
		sa, sb := as.Swaps, bs.Swaps
		if sa > sb {
			return -1
		}
		if sa < sb {
			return 1
		}
		return domain.ComparePoolKeys(a, b)
	})
	v4Reserve := 0
	for _, key := range keys {
		if key.IsV4() && !knownV4[key] && !options.Seen[key] && p.After[key] <= options.Anchor.Number {
			v4Reserve = min(v4ScanIDs, max(1, options.Limit/4))
			break
		}
	}
	var rows []Creation
	attempts := 0
	var oldV4 []domain.PoolKey
	for _, key := range keys {
		if key.IsV4() && knownV4[key] {
			continue
		}
		if options.Seen[key] || known[key.Address] || options.Activity[key].Swaps < max(options.MinSwaps, 1) {
			continue
		}
		if p.After[key] > options.Anchor.Number {
			p.assess(key, "metadata_cooldown", options)
			continue
		}
		if key.IsV4() {
			oldV4 = append(oldV4, key)
			continue
		}
		if key.Kind != 0 {
			continue
		}
		if attempts >= options.Limit-v4Reserve {
			p.assess(key, "metadata_probe_budget", options)
			continue
		}
		attempts++
		if len(p.After) < 4096 {
			p.After[key] = options.Anchor.Number + max(options.Cooldown, 1)
		}
		row, matched, err := probeMarket(ctx, caller, key.Address, options.Emitters, options.Anchor)
		if err != nil {
			if ProbeRejected(err) {
				p.assess(key, "metadata_contract_rejected", options)
				continue
			}
			return nil, err
		}
		if !matched || options.Seen[row.Spec.PoolKey()] {
			p.assess(key, "unconfigured_or_unregistered_factory", options)
			continue
		}
		options.Seen[row.Spec.PoolKey()] = true
		rows = append(rows, row)
		p.assess(key, "metadata_verified", options)
	}
	backfill := options
	backfill.Limit = options.Limit - attempts
	extra, err := p.backfillV4(ctx, logs, oldV4, backfill)
	if err != nil {
		return nil, err
	}
	return append(rows, extra...), nil
}

// ProbeRejected distinguishes a single contract rejection from provider/rate,
// request budget and transport failures, which must stop the discovery round.
func ProbeRejected(err error) bool {
	var reply *rpc.RPCError
	return errors.Is(err, pool.ErrCallRejected) || errors.As(err, &reply) && reply.Code == 3
}
func probeRead(ctx context.Context, caller pool.ContractCaller, to domain.Address, data []byte, anchor domain.BlockRef) ([]byte, error) {
	var output string
	if err := caller.CallContract(ctx, to, data, fmt.Sprintf("0x%x", anchor.Number), &output); err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(output, "0x"))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid metadata hex", pool.ErrCallRejected)
	}
	return raw, nil
}
func probeSelector(signature string) []byte { hash := eventHash(signature); return hash[:4] }
func probeAddress(ctx context.Context, caller pool.ContractCaller, to domain.Address, data []byte, anchor domain.BlockRef) (domain.Address, error) {
	raw, err := probeRead(ctx, caller, to, data, anchor)
	if err != nil {
		return domain.Address{}, err
	}
	if len(raw) != 32 || new(big.Int).SetBytes(raw[:12]).Sign() != 0 {
		return domain.Address{}, fmt.Errorf("%w: invalid metadata address", pool.ErrCallRejected)
	}
	var address domain.Address
	copy(address[:], raw[12:])
	return address, nil
}
func probeUint(ctx context.Context, caller pool.ContractCaller, to domain.Address, signature string, anchor domain.BlockRef) (uint64, error) {
	raw, err := probeRead(ctx, caller, to, probeSelector(signature), anchor)
	if err != nil {
		return 0, err
	}
	value := new(big.Int).SetBytes(raw)
	if len(raw) != 32 || !value.IsUint64() {
		return 0, fmt.Errorf("%w: invalid metadata uint", pool.ErrCallRejected)
	}
	return value.Uint64(), nil
}
func probeMarket(ctx context.Context, caller pool.ContractCaller, address domain.Address, emitters []Emitter, anchor domain.BlockRef) (Creation, bool, error) {
	factory, err := probeAddress(ctx, caller, address, probeSelector("factory()"), anchor)
	if err != nil {
		if ProbeRejected(err) {
			for _, candidate := range emitters {
				if candidate.Source == SourceVelodromeV1 {
					return probeVelodromeV1(ctx, caller, address, candidate, anchor, err)
				}
			}
		}
		return Creation{}, false, err
	}
	var emitter Emitter
	for _, candidate := range emitters {
		if candidate.Address == factory && candidate.Source != SourceUniswapV4 {
			emitter = candidate
			break
		}
	}
	if !emitter.Source.Valid() {
		return Creation{}, false, nil
	}
	spec := Spec{Address: address, Kind: emitter.Source.PoolKind()}
	spec.Token0, err = probeAddress(ctx, caller, address, probeSelector("token0()"), anchor)
	if err != nil {
		return Creation{}, false, err
	}
	spec.Token1, err = probeAddress(ctx, caller, address, probeSelector("token1()"), anchor)
	if err != nil {
		return Creation{}, false, err
	}
	if spec.Token0 == (domain.Address{}) || spec.Token1 == (domain.Address{}) || spec.Token0 == spec.Token1 {
		return Creation{}, false, nil
	}
	signature := "getPool(address,address,uint24)"
	tier := uint64(0)
	switch spec.Kind {
	case domain.PoolKindVelodromeV2:
		stable, err := probeUint(ctx, caller, address, "stable()", anchor)
		if err != nil {
			return Creation{}, false, err
		}
		if stable > 1 {
			return Creation{}, false, nil
		}
		spec.Factory, spec.Stable = factory, stable == 1
		signature, tier = "getPool(address,address,bool)", stable
	case domain.PoolKindV2:
		spec.Fee = 3000
		signature = "getPair(address,address)"
	case domain.PoolKindCamelotV3:
		signature = "poolByPair(address,address)"
	case domain.PoolKindSolidlyV3:
		// No fee() getter: the fee is slot0's third word; the factory keys
		// pools by tick spacing.
		spacing, err := probeUint(ctx, caller, address, "tickSpacing()", anchor)
		if err != nil {
			return Creation{}, false, err
		}
		slot0, err := probeRead(ctx, caller, address, probeSelector("slot0()"), anchor)
		if err != nil {
			return Creation{}, false, err
		}
		if spacing == 0 || spacing > 32767 || len(slot0) < 128 {
			return Creation{}, false, nil
		}
		fee := new(big.Int).SetBytes(slot0[64:96])
		if !fee.IsUint64() || fee.Uint64() > 100_000 {
			return Creation{}, false, nil
		}
		spec.Fee, spec.TickSpacing = uint32(fee.Uint64()), int32(spacing)
		signature, tier = "getPool(address,address,int24)", spacing
	default:
		fee, err := probeUint(ctx, caller, address, "fee()", anchor)
		if err != nil {
			return Creation{}, false, err
		}
		if fee >= 1000000 {
			return Creation{}, false, nil
		}
		spacing, err := probeUint(ctx, caller, address, "tickSpacing()", anchor)
		if err != nil {
			return Creation{}, false, err
		}
		if spacing == 0 || spacing > 32767 {
			return Creation{}, false, nil
		}
		spec.Fee = uint32(fee)
		spec.TickSpacing = int32(spacing)
		tier = fee
		if spec.Kind == domain.PoolKindSlipstream {
			signature = "getPool(address,address,int24)"
			tier = spacing
		}
	}
	data := make([]byte, 100)
	copy(data, probeSelector(signature))
	copy(data[16:36], spec.Token0[:])
	copy(data[48:68], spec.Token1[:])
	new(big.Int).SetUint64(tier).FillBytes(data[68:])
	if spec.Kind == domain.PoolKindV2 || spec.Kind == domain.PoolKindCamelotV3 {
		data = data[:68]
	}
	actual, err := probeAddress(ctx, caller, factory, data, anchor)
	if err != nil {
		return Creation{}, false, err
	}
	if actual != address {
		return Creation{}, false, nil
	}
	if spec.Kind == domain.PoolKindVelodromeV2 {
		loaded, err := velodromev2.LoadFee(ctx, caller, pool.State{Key: spec.PoolKey(), Velodrome: &pool.VelodromeState{Factory: factory, Stable: spec.Stable}}, fmt.Sprintf("0x%x", anchor.Number))
		if err != nil {
			return Creation{}, false, err
		}
		spec.Fee = loaded.Fee
	}
	return Creation{Spec: spec, Source: emitter.Source, Emitter: factory, Block: anchor.Number}, true, nil
}

// probeVelodromeV1 identifies a Velodrome V1 pair, which has no factory()
// getter: metadata() names its tokens and curve, and the configured V1 factory
// must return this address from getPair. Otherwise the original rejection
// stands. V1 shares V2's invariants (dex/velodromev2/testdata/v1_quotes.json).
func probeVelodromeV1(ctx context.Context, caller pool.ContractCaller, address domain.Address, emitter Emitter, anchor domain.BlockRef, rejected error) (Creation, bool, error) {
	raw, err := probeRead(ctx, caller, address, probeSelector("metadata()"), anchor)
	if err != nil || len(raw) != 7*32 {
		return Creation{}, false, rejected
	}
	stable := new(big.Int).SetBytes(raw[128:160])
	if stable.BitLen() > 1 || new(big.Int).SetBytes(raw[160:172]).Sign() != 0 || new(big.Int).SetBytes(raw[192:204]).Sign() != 0 {
		return Creation{}, false, nil
	}
	spec := Spec{Address: address, Kind: domain.PoolKindVelodromeV2, Factory: emitter.Address, Stable: stable.Sign() != 0}
	copy(spec.Token0[:], raw[172:192])
	copy(spec.Token1[:], raw[204:224])
	if spec.Token0 == (domain.Address{}) || spec.Token1 == (domain.Address{}) || spec.Token0 == spec.Token1 {
		return Creation{}, false, nil
	}
	data := make([]byte, 100)
	copy(data, probeSelector("getPair(address,address,bool)"))
	copy(data[16:36], spec.Token0[:])
	copy(data[48:68], spec.Token1[:])
	if spec.Stable {
		data[99] = 1
	}
	actual, err := probeAddress(ctx, caller, emitter.Address, data, anchor)
	if err != nil {
		return Creation{}, false, err
	}
	if actual != address {
		return Creation{}, false, nil
	}
	loaded, err := velodromev2.LoadFee(ctx, caller, pool.State{Key: spec.PoolKey(), Velodrome: &pool.VelodromeState{Factory: emitter.Address, Stable: spec.Stable}}, fmt.Sprintf("0x%x", anchor.Number))
	if err != nil {
		return Creation{}, false, err
	}
	spec.Fee = loaded.Fee
	return Creation{Spec: spec, Source: SourceVelodromeV1, Emitter: emitter.Address, Block: anchor.Number}, true, nil
}
