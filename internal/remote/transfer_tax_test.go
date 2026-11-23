package remote

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

func loadTaxFixture(t *testing.T) (domain.BlockRef, []ingest.Transaction, map[domain.Address]pool.State) {
	t.Helper()
	raw, err := os.ReadFile("testdata/e280-transfer-tax.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Block struct {
			Number     uint64
			Hash       string
			ParentHash string `json:"parent_hash"`
		}
		Tx    string
		Pools []struct {
			Address, Token0, Token1 string
			Kind                    domain.PoolKind
		}
		Logs []struct {
			Address, Data string
			Topics        []string
		}
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	hash, _ := domain.ParseHash(f.Block.Hash)
	parent, _ := domain.ParseHash(f.Block.ParentHash)
	txHash, _ := domain.ParseHash(f.Tx)
	tx := ingest.Transaction{Hash: txHash, Successful: true}
	for _, l := range f.Logs {
		a, _ := domain.ParseAddress(l.Address)
		data, _ := hex.DecodeString(strings.TrimPrefix(l.Data, "0x"))
		var topics []domain.Hash
		for _, s := range l.Topics {
			h, _ := domain.ParseHash(s)
			topics = append(topics, h)
		}
		tx.Logs = append(tx.Logs, ingest.Log{Address: a, Topics: topics, Data: data})
	}
	pools := map[domain.Address]pool.State{}
	for _, p := range f.Pools {
		a, _ := domain.ParseAddress(p.Address)
		t0, _ := domain.ParseAddress(p.Token0)
		t1, _ := domain.ParseAddress(p.Token1)
		pools[a] = pool.State{Key: domain.PoolKey{Address: a, Kind: p.Kind}, Token0: t0, Token1: t1}
	}
	return domain.BlockRef{Number: f.Block.Number, Hash: hash, ParentHash: parent}, []ingest.Transaction{tx}, pools
}

// A real Arbitrum One transaction: the E280/GMX pool's Swap sent 145.36M E280
// to the E280/ARB pool, which received 139.55M; 5.81M (4%) went to the
// token's tax address. Only E280 delivered less than its pool sent.
func TestTransferShortfallsFindTheTaxedToken(t *testing.T) {
	ref, txs, pools := loadTaxFixture(t)
	e280, _ := domain.ParseAddress("0x058e7b30200d001130232e8fbfdf900590e0baa9")
	found := transferShortfalls(ref, txs, pools, nil)
	if len(found) != 1 || found[0].Token != e280 || found[0].Source != receiptTaxSource || found[0].Block != ref {
		t.Fatalf("shortfalls %+v", found)
	}
	sent, _ := new(big.Int).SetString("145360869142869767094697634", 10)
	received, _ := new(big.Int).SetString("139546434377154976410909729", 10)
	if found[0].Requested.Cmp(sent) != 0 || found[0].Received.Cmp(received) != 0 {
		t.Fatal("requested/received", found[0].Requested, found[0].Received)
	}
	if found := transferShortfalls(ref, txs, pools, map[domain.Address]bool{e280: true}); len(found) != 0 {
		t.Fatal("a protected token was recorded", found)
	}
	failed := txs[0]
	failed.Successful = false
	if found := transferShortfalls(ref, []ingest.Transaction{failed}, pools, nil); len(found) != 0 {
		t.Fatal("a failed transaction produced evidence", found)
	}
}

// The canonical writer records the taxed token in the runtime's registry,
// which ranking consults, and leaves the tokens that delivered in full.
func TestCanonicalBlockRecordsTheTaxedToken(t *testing.T) {
	ref, txs, pools := loadTaxFixture(t)
	store := state.New()
	if err := store.BeginBlock(domain.BlockRef{Number: ref.Number - 1, Hash: ref.ParentHash}); err != nil {
		t.Fatal(err)
	}
	for _, p := range pools {
		p.Reserve0, p.Reserve1 = big.NewInt(1), big.NewInt(1)
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	store.Commit()
	risks, err := tokenrisk.Open("", domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161})
	if err != nil {
		t.Fatal(err)
	}
	defer risks.Close()
	r := &runtime{store: store, risks: risks}
	r.recordTransferTaxes(ref, txs)
	e280, _ := domain.ParseAddress("0x058e7b30200d001130232e8fbfdf900590e0baa9")
	if !errors.Is(risks.Check(e280), tokenrisk.ErrBlocked) {
		t.Fatal("the taxed token was not excluded")
	}
	for _, p := range pools {
		if risks.Check(p.Token1) != nil {
			t.Fatal("a token that delivered in full was excluded", p.Token1)
		}
	}
}
