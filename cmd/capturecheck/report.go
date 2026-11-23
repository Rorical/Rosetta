package main

import (
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
)

// writeReport prints the ranking, the direct quote, and a one-line summary.
func writeReport(w io.Writer, c caseData, cycles int, opportunities []arb.Opportunity, direct directQuote, rank, top int) {
	fmt.Fprintf(w, "case=%s block=%d pre_block=%d universe=%d tx=%s\n",
		c.Name, c.Block, c.PreBlock, len(c.Universe), c.OnChain.Tx)
	fmt.Fprintf(w, "cycles=%d opportunities=%d\n", cycles, len(opportunities))
	for i, opportunity := range opportunities {
		if i >= top {
			break
		}
		fmt.Fprintf(w, "opportunity rank=%d start=%s pools=%s input=%s net=%s net_after_gas=%s gas_comparable=%t\n",
			i+1,
			opportunity.Cycle.Start.Hex(),
			poolAddresses(opportunity.Cycle.Pools),
			opportunity.Input,
			opportunity.Net,
			opportunity.NetAfterGas,
			opportunity.GasComparable,
		)
	}

	bps := diffBps(direct.Net, c.OnChain.Net)
	fmt.Fprintf(w, "direct input=%s output=%s net=%s onchain_net=%s diff=%s diff_bps=%s\n",
		c.OnChain.Input,
		direct.Output,
		direct.Net,
		c.OnChain.Net,
		new(big.Int).Sub(direct.Net, c.OnChain.Net),
		formatBps(bps),
	)
	for i, leg := range direct.Legs {
		fmt.Fprintf(w, "direct leg=%d pool=%s in=%s out=%s\n",
			i+1, leg.Pool.Hex(), leg.AmountIn, leg.AmountOut)
	}

	found := rank > 0
	fmt.Fprintf(w, "rank found=%t", found)
	if found {
		opportunity := opportunities[rank-1]
		fmt.Fprintf(w, " rank=%d input=%s net=%s", rank, opportunity.Input, opportunity.Net)
	}
	fmt.Fprintln(w)

	top1 := rank == 1
	fmt.Fprintf(w, "top1=%t", top1)
	switch {
	case len(opportunities) == 0:
		fmt.Fprint(w, " start=none")
	case !top1:
		best := opportunities[0]
		fmt.Fprintf(w, " start=%s pools=%s net=%s",
			best.Cycle.Start.Hex(), poolAddresses(best.Cycle.Pools), best.Net)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "case=%s block=%d universe=%d cycles=%d opportunities=%d found=%t rank=%d top1=%t direct_net=%s onchain_net=%s diff_bps=%s\n",
		c.Name,
		c.Block,
		len(c.Universe),
		cycles,
		len(opportunities),
		found,
		rank,
		top1,
		direct.Net,
		c.OnChain.Net,
		formatBps(bps),
	)
}

func poolAddresses(pools []domain.PoolKey) string {
	addresses := make([]string, len(pools))
	for i, key := range pools {
		addresses[i] = key.Address.Hex()
	}
	return strings.Join(addresses, ",")
}
