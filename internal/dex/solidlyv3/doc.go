// Package solidlyv3 loads and prices Solidly V3 pools.
//
// Solidly V3 is a Uniswap V3 fork: slot0, liquidity, tickBitmap and ticks use
// the V3 selectors and the swap math is V3 SwapMath, so loading and quoting
// delegate to internal/dex/uniswapv3 under the PoolKindSolidlyV3 label. The
// differences are the fee, which lives in slot0 (no fee() getter) and changes
// only through a logged SetFee(feeOld, feeNew), the PoolCreated layout, which
// indexes tickSpacing instead of fee, and the swap callback name.
package solidlyv3
