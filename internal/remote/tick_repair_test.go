package remote

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/state"
)

func repairFixture(t *testing.T) (*runtime, tickAnswer, canonicalUpdate) {
	t.Helper()
	anchor := domain.BlockRef{Number: 10, Hash: domain.Hash{10}, ParentHash: domain.Hash{9}}
	p := pool.State{Key: domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindV3}, Token0: domain.Address{2}, Token1: domain.Address{3}, TickSpacing: 10, Fee: 500, Tick: 0, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1000)}
	store := state.New()
	if err := store.BeginBlock(anchor); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(p); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	ref := domain.BlockRef{Number: 11, Hash: domain.Hash{11}, ParentHash: anchor.Hash}
	topic := func(tick int64) domain.Hash {
		n := big.NewInt(tick)
		if n.Sign() < 0 {
			n.Add(n, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		var hash domain.Hash
		n.FillBytes(hash[:])
		return hash
	}
	tx := ingest.Transaction{Hash: domain.Hash{42}, Index: 0, Successful: true, Logs: []ingest.Log{{Address: p.Key.Address, Topics: []domain.Hash{uniswapv3.TopicMint, {}, topic(-60), topic(60)}, Data: wordData(0, 200, 0, 0)}}}
	branch, err := ingest.NewTransactionBranch(store)
	if err != nil {
		t.Fatal(err)
	}
	if err = branch.Begin(ref); err != nil {
		t.Fatal(err)
	}
	if _, err = branch.Apply(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err = branch.Commit(ref, []domain.Hash{tx.Hash}); err != nil {
		t.Fatal(err)
	}
	current := branch.Base()
	updated, _ := current.Pool(p.Key)
	updated.Fee = 420
	if err = current.Upsert(updated); err != nil {
		t.Fatal(err)
	}
	fast, err := ingest.NewFastBranch(current, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deep := pool.DeepState{WindowMin: -2560, WindowMax: 2559, LoadedBlock: anchor, Bitmap: map[int16]*big.Int{-1: new(big.Int).SetBit(new(big.Int), 244, 1), 0: new(big.Int).SetBit(new(big.Int), 12, 1)}, Ticks: map[int32]pool.TickInfo{-120: {LiquidityGross: big.NewInt(1000), LiquidityNet: big.NewInt(1000)}, 120: {LiquidityGross: big.NewInt(1000), LiquidityNet: big.NewInt(-1000)}}}
	r := &runtime{store: current, fast: fast, version: domain.StateVersion{Source: domain.SourceStandard, Anchor: ref}}
	answer := tickAnswer{task: tickTask{reader: &snapshot{block: rpc.ExecutionBlock{Ref: anchor}}, state: p, radius: 1}, deep: deep}
	update := canonicalUpdate{block: rpc.ExecutionBlock{Ref: ref, Transactions: []domain.Hash{tx.Hash}}, transactions: []ingest.Transaction{tx}}
	return r, answer, update
}

func TestTickRepairReplaysLiquidityLogsAndPreservesCurrentFees(t *testing.T) {
	r, answer, update := repairFixture(t)
	key := answer.task.state.Key
	d := &tickRepair{busy: true, demand: map[domain.PoolKey]int16{key: 1}, retryAt: map[domain.PoolKey]uint64{}, tail: []canonicalUpdate{update}}
	if err := d.consume(t.Context(), r, answer); err != nil {
		t.Fatal(err)
	}
	deep, ok := r.store.View().Deep(key)
	if !ok {
		t.Fatal("replayed window remained stale")
	}
	if deep.LoadedBlock != r.store.Block() || deep.Ticks[-60].LiquidityGross.Cmp(big.NewInt(200)) != 0 {
		t.Fatal("repair lost logs after its anchor")
	}
	got, _ := r.store.Pool(key)
	if got.Fee != 420 || got.Liquidity.Cmp(big.NewInt(1200)) != 0 {
		t.Fatal("repair overwrote canonical pool metadata", got)
	}
	if len(d.demand) != 0 {
		t.Fatal("completed repair stayed queued")
	}
}

func TestTickRepairRejectsDetachedOrIncompleteReplay(t *testing.T) {
	for _, mode := range []string{"missing receipts", "different ancestry", "new generation"} {
		t.Run(mode, func(t *testing.T) {
			r, answer, update := repairFixture(t)
			key := answer.task.state.Key
			d := &tickRepair{busy: true, demand: map[domain.PoolKey]int16{key: 1}, retryAt: map[domain.PoolKey]uint64{}, tail: []canonicalUpdate{update}}
			switch mode {
			case "missing receipts":
				d.tail = nil
			case "different ancestry":
				d.tail[0].block.Ref.ParentHash = domain.Hash{88}
			case "new generation":
				d.generation++
			}
			if err := d.consume(t.Context(), r, answer); err != nil {
				t.Fatal(err)
			}
			if _, ok := r.store.View().Deep(key); ok {
				t.Fatal("unproven repair was adopted")
			}
		})
	}
}

func TestTickRepairCloseCancelsProviderRecovery(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	_, answer, _ := repairFixture(t)
	d := newTickRepair(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	d.cancelTask = cancel
	answer.task.ctx = ctx
	answer.task.reader.client = rpc.NewClient(server.URL, rpc.Options{RetryReads: true})
	d.jobs <- answer.task
	select {
	case <-started:
	case <-time.After(time.Second):
		d.close()
		t.Fatal("repair did not start")
	}
	done := make(chan struct{})
	go func() { d.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("repair close waited on a provider cooldown")
	}
}
