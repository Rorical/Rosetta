// Package discovery builds the pool table from factory and PoolManager
// creation logs at startup or on a slow refresh.
//
// It scans Uniswap V2/V3/V4, Pancake V3, Slipstream and Camelot V3 creation events,
// filters candidates by token allowlist, fee, hooks, and active liquidity, and
// ranks survivors by liquidity (sqrt(x*y) reserves for V2). These are raw token
// units, not USD values. Configured seed pools are always included.
//
// Two rules bound what Discover returns:
//
//   - Quote tokens: when QuoteTokens is non-empty, both of a pool's tokens
//     must belong to it. When it is empty, every token pair is eligible. An
//     empty allowlist is global discovery and materially raises the risk of
//     filling the table with illiquid or hostile tokens; the caller can still
//     bound the result with MaxPools, MaxFee, and MinLiquidity.
//   - Liquidity: a pool with zero active liquidity is always dropped, even
//     when MinLiquidity is zero, because it has no position that can fill a
//     quote. Pools below MinLiquidity are dropped as well.
//
// Discover is synchronous.
// Each periodic refresh scans the whole
// configured lookback window ending at the head, not just the blocks after the
// last scan, so a pool dropped for zero liquidity is re-checked on later
// refreshes until it gains liquidity or leaves the window. The ingest package
// filters receipts against the pool table this package produces; the Source
// interface used there deliberately does not gain eth_getLogs.
package discovery
