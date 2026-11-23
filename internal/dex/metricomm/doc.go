// Package metricomm ports the swap math of MetricOmmPool, an oracle-anchored
// market maker on Arbitrum One, from its verified Solidity source.
//
// A pool holds liquidity in a fixed grid of bins. Each bin spans a price range
// expressed as a distance from a reference price that the pool's price
// provider supplies on every swap, together with a bid and an ask. The
// reference anchors the price path; the bid and ask only set the base fee
// (reference/bid - 1 for sells, ask/reference - 1 for buys).
//
// Swap reproduces _swapCore and _executeSwap bit for bit, including rounding
// and revert conditions. The pool's extension hooks (beforeSwap, afterSwap)
// are not modelled: an extension can reject a swap this package accepts.
//
// State is filled from raw storage words (StorageSlots, DecodeState), read
// with eth_getStorageAt or the pool's extsload.
package metricomm
