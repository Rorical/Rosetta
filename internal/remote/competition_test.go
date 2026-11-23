package remote

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
)

func TestCompetitionObserverRecordsRivalBidsOffTheFeedPath(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	to := common.Address{0xbb}
	signed, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(10)), &types.DynamicFeeTx{ChainID: big.NewInt(10), Nonce: 7, GasTipCap: big.NewInt(5_000_000), GasFeeCap: big.NewInt(10_000_000), Gas: 400_000, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reactive.jsonl.competition.jsonl")
	ctx, cancel := context.WithCancel(t.Context())
	var rivals []competitorRecord // the writer goroutine is the only caller
	o, err := openCompetitionObserver(ctx, path, 10, []domain.Address{{0x01}}, func(r competitorRecord) { rivals = append(rivals, r) })
	if err != nil {
		t.Fatal(err)
	}
	a, b := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{0xa}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{0xb}}
	tx := feed.FlashTransaction{Hash: domain.Hash(signed.Hash()), Raw: raw, Successful: true}
	block := feed.Flashblock{Number: 5, Index: 2}
	o.observeFlash(block, tx, 40, []domain.PoolKey{a, a}, big.NewInt(6_000_000), time.Now()) // one pool twice: a plain swap
	o.observeFlash(block, tx, 41, []domain.PoolKey{b, a, a}, big.NewInt(6_000_000), time.Now())
	cancel()
	<-o.done
	// A transaction on one of our pools reaches the estimator too: it can move
	// a pool ahead of us as well as a two-pool rival can.
	if len(rivals) != 2 || rivals[0].Hash != tx.Hash || len(rivals[0].Pools) != 1 || len(rivals[1].Pools) != 2 {
		t.Fatal("rival bids did not reach the estimator callback", len(rivals))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	// The one-pool transaction tips 4M wei per gas, a bid rather than a plain
	// swap, so the file keeps it.
	if len(lines) != 2 {
		t.Fatalf("want the one-pool bid and the multi-pool record, got %d: %s", len(lines), data)
	}
	var got competitorRecord
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatal(err)
	}
	// Fee cap 10M minus base fee 6M leaves 4M, below the 5M tip cap.
	if got.From != domain.Address(crypto.PubkeyToAddress(key.PublicKey)) || got.Own || got.Tip.Int64() != 4_000_000 || got.Index != 41 || len(got.Pools) != 2 || got.GasLimit != 400_000 || got.To == nil || *got.To != domain.Address(to) {
		t.Fatalf("unexpected record: %+v", got)
	}
}

// On Arbitrum One a Fast Feed message is one sequenced transaction; tips order
// it within its priority gas auction round, which is its frame.
func TestCompetitionObserverRecordsArbitrumFastRivals(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	to := common.Address{0xbb}
	signed, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(42161)), &types.DynamicFeeTx{ChainID: big.NewInt(42161), GasTipCap: big.NewInt(3_000), GasFeeCap: big.NewInt(20_000_000), Gas: 700_000, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "competition.jsonl")
	ctx, cancel := context.WithCancel(t.Context())
	var rivals []competitorRecord
	o, err := openCompetitionObserver(ctx, path, 42161, []domain.Address{{0x01}}, func(r competitorRecord) { rivals = append(rivals, r) })
	if err != nil {
		t.Fatal(err)
	}
	a, b := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{0xa}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{0xb}}
	message := feed.FastMessage{PGARound: 77, BlockNumber: 900, TxIndex: 3, RawTx: raw, TxHash: domain.Hash(signed.Hash()), Receipt: feed.FastReceipt{Successful: true, BaseFee: big.NewInt(10_000_000)}}
	o.observeFast(message, 5, []domain.PoolKey{a, b}, time.Now())
	cancel()
	<-o.done
	if len(rivals) != 1 {
		t.Fatalf("%d rivals", len(rivals))
	}
	if got := rivals[0]; got.Block != 900 || got.Frame != 77 || got.Index != 5 || got.Tip.Int64() != 3_000 || got.From != domain.Address(crypto.PubkeyToAddress(key.PublicKey)) {
		t.Fatalf("unexpected record: %+v", got)
	}
}

// Robinhood has no preconfirmation stream: rivals come from canonical
// receipts, which carry the sender and the paid price but no raw transaction.
func TestCompetitionObserverRecordsCanonicalRivals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "competition.jsonl")
	ctx, cancel := context.WithCancel(t.Context())
	var rivals []competitorRecord
	o, err := openCompetitionObserver(ctx, path, 4663, []domain.Address{{0x01}}, func(r competitorRecord) { rivals = append(rivals, r) })
	if err != nil {
		t.Fatal(err)
	}
	a, b := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{0xa}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{0xb}}
	rival := domain.Address{0x77}
	fees := &domain.ReceiptFees{BaseFee: big.NewInt(10_000_000), GasPrice: big.NewInt(10_000_500)}
	o.observeCanonical(300, ingest.Transaction{Hash: domain.Hash{1}, From: &rival, Index: 4, Successful: true, Fees: fees}, []domain.PoolKey{a, b}, time.Now())
	o.observeCanonical(300, ingest.Transaction{Hash: domain.Hash{2}, Index: 5, Successful: true, Fees: fees}, []domain.PoolKey{a, b}, time.Now()) // no sender
	cancel()
	<-o.done
	if len(rivals) != 1 {
		t.Fatalf("%d rivals", len(rivals))
	}
	if got := rivals[0]; got.From != rival || got.Block != 300 || got.Index != 4 || got.Frame != 0 || got.Tip.Int64() != 500 || len(got.Pools) != 2 {
		t.Fatalf("unexpected record: %+v", got)
	}
}
