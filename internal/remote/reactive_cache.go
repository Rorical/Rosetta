package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type reactiveCache struct {
	costs  planning.ReactiveCosts
	wallet map[domain.Address]*big.Int
	native *big.Int
	block  domain.BlockRef
}

// reactiveAccountInterval bounds how long wallet balances and the executor
// configuration version go unread while none of our transactions lands: only
// an outside transfer or an owner setter changes them then. The runtime
// converts it to blocks of its chain (accountBlocks).
const reactiveAccountInterval = 5 * time.Minute

// reactiveGasLow is the gas of the operator fee's low sample.
const reactiveGasLow = 100_000

// reactivePosterSeconds is how long a Nitro L1 price read is reused. ArbOS
// moves the price when a batch poster report arrives, minutes apart; the
// poster gas reserve carries twofold headroom for a move in between. Reading
// every few seconds would spend paid RPC calls on a price that rarely moves.
const reactivePosterSeconds = 60

// arbGasInfo is Nitro's ArbGasInfo precompile.
var arbGasInfo = addressAt(0x6c)

// reactiveReads are the chain reads behind the last good context. A refresh
// rereads only what can have changed: the fee samples when the block's OP fee
// key moves, and account state after our own landing or every
// reactiveAccountBlocks. An L1 base fee change alone rescales the L1 samples.
type reactiveReads struct {
	feeBase opFeeParams // parameters of the block the fees were read at
	fees    *reactiveFees
	account uint64 // block of the last account read
	native  *big.Int
	tokens  map[domain.Address]*big.Int // min(balance, allowance)
	// inventory is the executor's balance of each asset funded from it.
	inventory map[domain.Address]*big.Int
	lanes     []*big.Int // the extra lanes' native balances
	// claimEpoch is the hot executor's; our own landing claim marks the
	// account stale, so the next refresh reads the raised value.
	claimEpoch uint64
	// Nitro's L1 price and the block timestamp it was read at.
	poster   *planning.PosterPrice
	posterAt uint64
}

type reactiveFees struct {
	l1, operator, l1Low, operatorLow *big.Int
}

// at returns the fees read at L1 rate from for a block at rate to. The scaled
// L1 samples round up from an upper bound of the unrounded oracle value, so
// they never fall below what the oracle would return.
func (f *reactiveFees) at(from, to *big.Int) *reactiveFees {
	if f == nil || from == nil || to == nil || from.Sign() == 0 || from.Cmp(to) == 0 {
		return f
	}
	scale := func(fee *big.Int) *big.Int {
		scaled := new(big.Int).Mul(new(big.Int).Add(fee, big.NewInt(1)), to)
		scaled.Add(scaled, new(big.Int).Sub(from, big.NewInt(1)))
		return scaled.Quo(scaled, from)
	}
	return &reactiveFees{l1: scale(f.l1), operator: f.operator, l1Low: scale(f.l1Low), operatorLow: f.operatorLow}
}

// refresh builds the hot path's cost and wallet context for block. The base
// fee comes from the header the follower already verified; the remaining reads
// bind to the block hash with requireCanonical and share one batch. The hot
// path only sees the immutable completed result.
// startContext loads the first context at the resumed block. A resume
// snapshot can predate the configured executor, deployed while the bot was
// down; the context describes the account now, so it falls back to the head.
// A failure at the head too, such as a wrong code hash, still stops startup.
func (s *reactiveRuntime) startContext(ctx context.Context, resumed rpc.ExecutionBlock, feeParams opFeeParams) (*reactiveCache, error) {
	cache, err := s.refresh(ctx, resumed, feeParams)
	if err == nil {
		return cache, nil
	}
	head, headErr := s.client.ExecutionBlock(ctx, "latest")
	if headErr != nil || head.Ref.Number <= resumed.Ref.Number {
		return nil, errors.Join(err, headErr)
	}
	cache, retry := s.refresh(ctx, head, feeParams)
	if retry != nil {
		return nil, errors.Join(err, retry)
	}
	return cache, nil
}

func (s *reactiveRuntime) refresh(ctx context.Context, block rpc.ExecutionBlock, feeParams opFeeParams) (*reactiveCache, error) {
	// Any failure forgets the reads, so the next refresh rereads everything; a
	// low balance must not stay cached until the account interval.
	previous := s.reads
	s.reads = nil
	reads, err := s.read(ctx, block, feeParams, previous)
	if err != nil {
		return nil, err
	}
	cfg := s.settings.planning
	// read verified the executor unpaused at this configVersion.
	// An extra lane takes jobs only while it can pay a transaction's budget.
	for i, lane := range s.extra {
		lane.funded.Store(i < len(reads.lanes) && reads.lanes[i].Cmp(s.settings.limits.PerTransaction) >= 0)
	}
	out := &reactiveCache{costs: planning.ReactiveCosts{BaseFee: new(big.Int).Set(block.BaseFee), Extra: new(big.Int), ConfigVersion: s.verifiedConfig, ClaimEpoch: reads.claimEpoch}, wallet: map[domain.Address]*big.Int{}, block: block.Ref, native: reads.native}
	if out.native.Cmp(s.settings.limits.PerTransaction) < 0 {
		return nil, fmt.Errorf("operator cannot cover transaction budget")
	}
	if fees := reads.fees.at(reads.feeBase.l1Rate, feeParams.l1Rate); fees != nil {
		size, sizeLow, err := s.feeSizes()
		if err != nil {
			return nil, err
		}
		// Cached data fees can change before inclusion. Reserve headroom and stop
		// using the context at its expiry; receipts still check the actual charge.
		out.costs.Extra.Mul(new(big.Int).Add(fees.l1, fees.operator), big.NewInt(2))
		if sizeLow < size && reactiveGasLow < cfg.GasLimit {
			out.costs.L1 = &planning.LinearFee{X0: sizeLow, X1: size, Y0: fees.l1Low, Y1: fees.l1}
			out.costs.Operator = &planning.LinearFee{X0: reactiveGasLow, X1: cfg.GasLimit, Y0: fees.operatorLow, Y1: fees.operator}
		}
	}
	out.costs.Poster, out.costs.Inventory = reads.poster, reads.inventory
	for token := range s.settings.minimum {
		if !s.settings.wallet[token] {
			continue
		}
		if token == (domain.Address{}) {
			out.wallet[token] = new(big.Int).Sub(out.native, s.settings.limits.PerTransaction)
			continue
		}
		out.wallet[token] = reads.tokens[token]
	}
	out.costs.ExpiresMS = time.Now().Add(time.Duration(s.settings.document.ContextMaxAgeMS) * time.Millisecond).UnixMilli()
	s.reads = reads
	return out, nil
}

// feeSizes are the L1 size bounds of the largest allowed calldata and of the
// shortest route: the two samples of the linear L1 fee, so each intent
// reserves for its own length and gas (ExtraFor).
func (s *reactiveRuntime) feeSizes() (size, sizeLow uint64, err error) {
	if size, err = execution.OPL1SizeBound(s.settings.document.MaxCalldataBytes + 200); err != nil {
		return 0, 0, err
	}
	sizeLow, err = execution.OPL1SizeBound(execution.PackedCalldataBound(2) + 200)
	return size, sizeLow, err
}

// read returns the reads for block, reusing the previous ones that cannot
// have changed.
func (s *reactiveRuntime) read(ctx context.Context, block rpc.ExecutionBlock, feeParams opFeeParams, previous *reactiveReads) (*reactiveReads, error) {
	reader := &snapshot{client: s.client, block: block, chain: s.settings.planning.Chain}
	defer reader.Close()
	cfg := s.settings.planning
	next := &reactiveReads{feeBase: feeParams}
	readFees := cfg.Chain.Family == domain.FamilyOPStack && (previous == nil || previous.fees == nil || feeParams.key == "" || feeParams.key != previous.feeBase.key)
	readPoster := cfg.Chain.Family == domain.FamilyNitro && (previous == nil || previous.poster == nil || block.Ref.Timestamp < previous.posterAt || block.Ref.Timestamp-previous.posterAt >= reactivePosterSeconds)
	stale := s.accountStale.Swap(false)
	readAccount := previous == nil || stale || block.Ref.Number < previous.account || block.Ref.Number-previous.account >= s.accountBlocks
	if previous != nil {
		if !readFees {
			next.fees, next.feeBase = previous.fees, previous.feeBase
		}
		if !readAccount {
			next.account, next.native, next.tokens, next.inventory, next.lanes, next.claimEpoch = previous.account, previous.native, previous.tokens, previous.inventory, previous.lanes, previous.claimEpoch
		}
		if !readPoster {
			next.poster, next.posterAt = previous.poster, previous.posterAt
		}
	}
	if !readFees && !readAccount && !readPoster {
		return next, nil
	}
	var calls []rpc.Call
	add := func(to domain.Address, signature string, words ...[]byte) int {
		calls = append(calls, rpc.Call{Method: "eth_call", Params: reader.contractParams(to, reactiveData(signature, words...), nil)})
		return len(calls) - 1
	}
	versionCall, nativeCall, epochCall := -1, -1, -1
	tokenCalls := map[domain.Address][2]int{}
	inventoryCalls := map[domain.Address]int{}
	if readAccount {
		versionCall = add(cfg.Executor, "configVersion()")
		if cfg.HotPath {
			epochCall = add(cfg.Executor, "claimEpoch()")
		}
		calls = append(calls, rpc.Call{Method: "eth_getBalance", Params: []any{cfg.Sender.Hex(), reader.blockID()}})
		nativeCall = len(calls) - 1
		for _, lane := range s.extra {
			calls = append(calls, rpc.Call{Method: "eth_getBalance", Params: []any{lane.sender.Hex(), reader.blockID()}})
		}
		for token := range s.settings.minimum {
			if !s.settings.wallet[token] || token == (domain.Address{}) {
				continue
			}
			tokenCalls[token] = [2]int{add(token, "balanceOf(address)", cfg.Sender[:]), add(token, "allowance(address,address)", cfg.Sender[:], cfg.Executor[:])}
		}
		for token, funded := range s.settings.inventory {
			if funded {
				inventoryCalls[token] = add(token, "balanceOf(address)", cfg.Executor[:])
			}
		}
	}
	var feeCalls [4]int
	if readFees {
		size, sizeLow, err := s.feeSizes()
		if err != nil {
			return nil, err
		}
		feeCalls = [4]int{
			add(opGasOracle, "getL1FeeUpperBound(uint256)", new(big.Int).SetUint64(size).Bytes()),
			add(opGasOracle, "getOperatorFee(uint256)", new(big.Int).SetUint64(cfg.GasLimit).Bytes()),
			add(opGasOracle, "getL1FeeUpperBound(uint256)", new(big.Int).SetUint64(sizeLow).Bytes()),
			add(opGasOracle, "getOperatorFee(uint256)", big.NewInt(reactiveGasLow).Bytes()),
		}
	}
	posterCall := -1
	if readPoster {
		posterCall = add(arbGasInfo, "getPricesInWei()")
	}
	results, err := reader.batchStrings(ctx, calls)
	if err != nil {
		return nil, err
	}
	word := func(i int) (*big.Int, error) {
		raw, err := hexData(results[i])
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("invalid reactive context word")
		}
		return new(big.Int).SetBytes(raw), nil
	}
	if readFees {
		var values [4]*big.Int
		for i, call := range feeCalls {
			if values[i], err = word(call); err != nil {
				return nil, err
			}
		}
		next.fees = &reactiveFees{l1: values[0], operator: values[1], l1Low: values[2], operatorLow: values[3]}
	}
	if readPoster {
		// perL2Tx, perL1CalldataByte, then storage and L2 gas prices.
		raw, err := hexData(results[posterCall])
		if err != nil || len(raw) != 6*32 {
			return nil, fmt.Errorf("invalid ArbGasInfo prices")
		}
		next.poster = &planning.PosterPrice{PerTx: new(big.Int).SetBytes(raw[:32]), PerByte: new(big.Int).SetBytes(raw[32:64])}
		next.posterAt = block.Ref.Timestamp
	}
	if !readAccount {
		return next, nil
	}
	version, err := word(versionCall)
	if err != nil || !version.IsUint64() || version.Sign() == 0 {
		return nil, fmt.Errorf("invalid executor config version")
	}
	if version.Uint64() != s.verifiedConfig {
		if err = s.verifyExecutor(ctx, reader); err != nil {
			return nil, err
		}
		s.verifiedConfig = version.Uint64()
	}
	if epochCall >= 0 {
		epoch, err := word(epochCall)
		if err != nil || !epoch.IsUint64() || epoch.Sign() == 0 {
			return nil, fmt.Errorf("invalid executor claim epoch")
		}
		next.claimEpoch = epoch.Uint64()
	}
	if next.native, err = integer(results[nativeCall]); err != nil {
		return nil, err
	}
	next.lanes = make([]*big.Int, len(s.extra))
	for i := range s.extra {
		if next.lanes[i], err = integer(results[nativeCall+1+i]); err != nil {
			return nil, err
		}
	}
	next.account = block.Ref.Number
	next.tokens = make(map[domain.Address]*big.Int, len(tokenCalls))
	for token, pair := range tokenCalls {
		balance, err := word(pair[0])
		if err != nil {
			return nil, err
		}
		allowance, err := word(pair[1])
		if err != nil {
			return nil, err
		}
		next.tokens[token] = balance
		if allowance.Cmp(balance) < 0 {
			next.tokens[token] = allowance
		}
	}
	next.inventory = make(map[domain.Address]*big.Int, len(inventoryCalls))
	for token, call := range inventoryCalls {
		if next.inventory[token], err = word(call); err != nil {
			return nil, err
		}
	}
	return next, nil
}

// batchStrings sends string-valued reads in batches of eight, the member limit
// CallContracts also keeps to, and fails on any member error.
func (s *snapshot) batchStrings(ctx context.Context, calls []rpc.Call) ([]string, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	values := make([]string, 0, len(calls))
	for start := 0; start < len(calls); start += 8 {
		results, err := s.client.Batch(ctx, calls[start:min(start+8, len(calls))])
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			if result.Error != nil {
				return nil, result.Error
			}
			var value string
			if err := json.Unmarshal(result.Result, &value); err != nil {
				return nil, err
			}
			values = append(values, value)
		}
	}
	return values, nil
}

// verifyExecutor checks configuration only an owner setter can change. Every
// setter increments configVersion and the code is immutable, so a passing
// check holds until the version moves; per-block refreshes skip these reads.
func (s *reactiveRuntime) verifyExecutor(ctx context.Context, reader *snapshot) error {
	cfg := s.settings.planning
	code, err := reader.CodeHash(ctx, cfg.Executor)
	if err != nil || code != cfg.CodeHash {
		return fmt.Errorf("reactive executor code mismatch")
	}
	paused, err := reader.readWord(ctx, cfg.Executor, "paused()")
	if err != nil || paused.Sign() != 0 {
		return fmt.Errorf("executor paused or unavailable")
	}
	for _, sender := range s.senders() {
		allowed, err := reactiveWord(ctx, reader, cfg.Executor, "operators(address)", sender[:])
		if err != nil || allowed.Cmp(big.NewInt(1)) != 0 {
			return fmt.Errorf("operator %s is not enabled", sender)
		}
	}
	chainID, err := reader.readWord(ctx, cfg.Executor, "chainId()")
	if err != nil || !chainID.IsUint64() || chainID.Uint64() != cfg.Chain.ChainID {
		return fmt.Errorf("executor chain mismatch")
	}
	return nil
}
func reactiveData(signature string, words ...[]byte) []byte {
	data := make([]byte, 4+32*len(words))
	copy(data, selector(signature))
	for i, word := range words {
		copy(data[4+(i+1)*32-len(word):], word)
	}
	return data
}
func reactiveWord(ctx context.Context, s *snapshot, to domain.Address, signature string, words ...[]byte) (*big.Int, error) {
	raw, err := s.ReadContract(ctx, to, reactiveData(signature, words...))
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("invalid reactive context word")
	}
	return new(big.Int).SetBytes(raw), nil
}
