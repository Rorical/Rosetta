package exec

import (
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"math/big"
)

// executorOutcome validates the same event identity for preconfirmation and settlement.
func executorOutcome(p PendingTransaction, logs []rpc.Log) (profit, loan *big.Int, err error) {
	profit, loan = new(big.Int), new(big.Int)
	topic := keccak([]byte("Executed(address,address,uint256,uint256,uint256)"))
	count := 0
	for _, log := range logs {
		if log.Address != p.Intent.Call.Executor || len(log.Topics) != 3 || log.Topics[0] != topic {
			continue
		}
		var operator, asset domain.Address
		copy(operator[:], log.Topics[1][12:])
		copy(asset[:], log.Topics[2][12:])
		if operator != p.Intent.Call.Sender || asset != p.Intent.Route.Asset || len(log.Data) != 96 {
			return nil, nil, fmt.Errorf("executor event identity mismatch")
		}
		if new(big.Int).SetBytes(log.Data[:32]).Cmp(p.Intent.Route.Amount) != 0 {
			return nil, nil, fmt.Errorf("executor event principal mismatch")
		}
		loan.SetBytes(log.Data[32:64])
		profit.SetBytes(log.Data[64:])
		count++
	}
	if count != 1 || profit.Cmp(p.Intent.Route.MinProfit) < 0 || loan.Cmp(p.Intent.Route.MaxLoanFee) > 0 {
		return nil, nil, fmt.Errorf("missing or inconsistent executor success event")
	}
	return profit, loan, nil
}
