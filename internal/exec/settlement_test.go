package exec

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"math/big"
	"testing"
)

func TestSettlementUsesProfitAfterLoanAndChargesReverts(t *testing.T) {
	i, _, _ := dispatchFixture(t)
	i.Route.MinProfit = big.NewInt(1)
	topics := []domain.Hash{keccak([]byte("Executed(address,address,uint256,uint256,uint256)")), {}, {}}
	copy(topics[1][12:], i.Call.Sender[:])
	copy(topics[2][12:], i.Route.Asset[:])
	data := make([]byte, 96)
	i.Route.Amount.FillBytes(data[:32])
	big.NewInt(120).FillBytes(data[64:])
	receipt := &rpc.TransactionReceipt{Hash: domain.Hash{5}, Success: true, Fees: &domain.ReceiptFees{Total: big.NewInt(20)}, Logs: []rpc.Log{{Address: i.Call.Executor, Topics: topics, Data: data}}}
	s, err := settleExecutor(PendingTransaction{Intent: i}, receipt)
	if err != nil || s.Net.Int64() != 100 {
		t.Fatal(s, err)
	}
	receipt.Success = false
	receipt.Logs = nil
	s, err = settleExecutor(PendingTransaction{Intent: i}, receipt)
	if err != nil || s.Net.Int64() != -20 || s.Profit.Sign() != 0 {
		t.Fatal(s, err)
	}
	receipt.Success = true
	if _, err = settleExecutor(PendingTransaction{Intent: i}, receipt); err == nil {
		t.Fatal("missing executor event accepted")
	}
}
