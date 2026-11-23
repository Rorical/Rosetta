// Package uniswapv4 identifies V4 pool logs, reads V4 pool state, and prices
// hooks-free V4 pools.
//
// V4 is a singleton: every pool lives in the PoolManager, so logs are filtered
// by topic1 (the pool ID) instead of by a per-pool address. A pool ID is
// keccak256(abi.encode(currency0, currency1, fee, tickSpacing, hooks)).
//
// Load and LoadState read slot0 and active liquidity through the deployed
// StateView lens on Arbitrum One; DeepLoader reads the tick bitmap and tick
// liquidity around the current tick. DecodeEvent turns a PoolManager Swap or
// ModifyLiquidity log into the V3-shaped event the store applies. Adapter
// quotes exact input with the V3 curve, using the effective swap fee (LP fee
// plus the direction's protocol fee).
//
// Scope: hooks-free, static-fee pools are quotable. Pools with non-zero hooks
// are rejected, because a hook can override the fee or change the swap itself,
// and the dynamic-LP-fee flag is rejected by SwapFee. There is no execution
// path: this package only decodes logs, loads state, and quotes.
package uniswapv4
