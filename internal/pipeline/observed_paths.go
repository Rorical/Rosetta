package pipeline

import (
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/ingest"
)

// ObserveTransactions learns supported closed paths from committed receipts.
// Receipt surplus is a discovery hint, never proof of future executable profit.
func (t *Topology) ObserveTransactions(ref domain.BlockRef, transactions []ingest.Transaction) {
	if t == nil {
		return
	}
	var cycles []graph.Cycle
	decoder := backtest.SwapDecoder{Matches: dex.IsSwapTopic, Amounts: dex.DecodeSwapAmounts}
	for _, tx := range transactions {
		cycle, reason := backtest.ExtractClosedCycle(ref, tx, t.markets, decoder)
		if reason == "" {
			cycles = append(cycles, cycle.Cycle)
		}
	}
	t.Engine.RememberCycles(cycles)
}
