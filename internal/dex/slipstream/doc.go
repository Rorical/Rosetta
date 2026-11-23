// Package slipstream loads and prices Slipstream pools on Arbitrum.
//
// Slipstream's core surface matches Uniswap V3: slot0, liquidity, tickBitmap,
// and ticks use the same selectors, so loading delegates to
// internal/dex/uniswapv3. The returned state is tagged PoolKindSlipstream; the
// V3 math is reused through the routing label.
//
// Fee accounting: fee() returns hundredths of a basis point (1e-6), the same
// unit as V3 and internal/math. The value is dynamic on Arbitrum, so Load reads
// it and returns it in State.Fee; Spec.Fee is only the creation-time value used
// to validate the request. A failed or malformed fee() read fails the load
// instead of quoting with a stale tier.
package slipstream
