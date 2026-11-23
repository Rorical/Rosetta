// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T} from "./ExecutorTypes.sol";

/// @dev Optional check, before funding and the first swap, that each bounded
/// pool has not moved against the route since the quote. A rival that lands
/// first on our pools would otherwise let every swap run and fail only at the
/// closing profit check; here the transaction reverts after a few reads.
///
/// Bounds are one-sided in the leg's direction (zeroForOne is tokenIn < tokenOut),
/// so any favourable move passes:
/// - V3-family and V4: the sqrt price from slot0 (Algebra globalState; V4 the
///   pool's slot0 word through PoolManager.extsload). zeroForOne requires
///   price >= bound, otherwise price <= bound.
/// - V2 and Velodrome V2: reserveOut / reserveIn from getReserves as a Q96
///   ratio; requires reserveOut * 2^96 >= bound * reserveIn.
/// The off-chain quote chooses each bound and so the tolerated movement.
///
/// checkCumulative (packed version 4) instead takes each leg's price at the
/// quote and multiplies the legs' rate changes: several pools that each moved
/// a little against the route fail together, which per-step bounds let
/// through until the closing profit check. A favourable
/// move offsets an adverse one, counted at most RATE_CAP.
library PriceCheck {
    error PoolMoved(uint256 step);
    error UnreadablePool(uint256 step);

    uint256 private constant PRICE_MASK = 0x00ffffffffffffffffffffffffffffffffffffffff;
    // StateLibrary.POOLS_SLOT: PoolManager's mapping from PoolId to Pool.State.
    uint256 private constant V4_POOLS_SLOT = 6;
    uint256 private constant ONE = 1e18;
    uint256 private constant RATE_CAP = 4e18;

    /// @param bounds Empty, or one per step with zero meaning unchecked.
    function check(T.Plan memory plan, uint256[] memory bounds) internal view {
        address tokenIn = plan.asset;
        for (uint256 i; i < bounds.length; ++i) {
            T.Step memory step = plan.steps[i];
            uint256 bound = bounds[i];
            if (bound != 0 && !within(step, tokenIn, bound, i)) revert PoolMoved(i);
            tokenIn = step.tokenOut;
        }
    }

    /// @param references One per step, zero meaning unchecked; minRate in 1e18.
    function checkCumulative(T.Plan memory plan, uint256[] memory references, uint256 minRate) internal view {
        address tokenIn = plan.asset;
        uint256 product = ONE;
        uint256 worst = type(uint256).max;
        uint256 worstStep;
        for (uint256 i; i < references.length; ++i) {
            T.Step memory step = plan.steps[i];
            uint256 reference_ = references[i];
            if (reference_ != 0) {
                uint256 rate = rateChange(step, tokenIn, reference_, i);
                product = min(mulDiv(product, rate, ONE), RATE_CAP);
                if (rate < worst) (worst, worstStep) = (rate, i);
            }
            tokenIn = step.tokenOut;
        }
        if (product < minRate) revert PoolMoved(worstStep);
    }

    function within(T.Step memory step, address tokenIn, uint256 bound, uint256 index) private view returns (bool) {
        (bool checked, bool pair, bool zeroForOne, uint256 price, uint256 reserveIn, uint256 reserveOut) =
            read(step, tokenIn, index);
        if (!checked) return true;
        if (pair) return atLeast(reserveOut, 1 << 96, bound, reserveIn);
        return zeroForOne ? price >= bound : price <= bound;
    }

    /// @dev The leg's rate now over its rate at the quote, in 1e18, capped.
    /// A pair's reference is reserveOut / reserveIn in Q96; a V3-family or V4
    /// leg's is the sqrt price, whose square is the rate selling token0.
    function rateChange(T.Step memory step, address tokenIn, uint256 reference_, uint256 index)
        private
        view
        returns (uint256)
    {
        (bool checked, bool pair, bool zeroForOne, uint256 price, uint256 reserveIn, uint256 reserveOut) =
            read(step, tokenIn, index);
        if (!checked) return ONE;
        if (pair) {
            if (reserveIn == 0) revert UnreadablePool(index);
            return min(mulDiv(mulDiv(reserveOut, 1 << 96, reserveIn), ONE, reference_), RATE_CAP);
        }
        if (price == 0) revert UnreadablePool(index);
        // Square root of the cap, so the squared change stays within RATE_CAP.
        uint256 root = min(zeroForOne ? mulDiv(price, ONE, reference_) : mulDiv(reference_, ONE, price), 2e18);
        return root * root / ONE;
    }

    /// @dev Reads the leg's pool. Unknown kinds are unchecked; route
    /// validation after the check rejects them.
    function read(T.Step memory step, address tokenIn, uint256 index)
        private
        view
        returns (bool checked, bool pair, bool zeroForOne, uint256 price, uint256 reserveIn, uint256 reserveOut)
    {
        zeroForOne = tokenIn < step.tokenOut;
        uint8 kind = step.kind;
        if (kind < T.V2 || kind > T.SOLIDLY_V3) return (false, false, zeroForOne, 0, 0, 0);
        checked = true;
        if (kind == T.V2 || kind == T.VELODROME_V2) {
            (uint256 reserve0, uint256 reserve1) = reserves(step.target, index);
            (reserveIn, reserveOut) = zeroForOne ? (reserve0, reserve1) : (reserve1, reserve0);
            return (true, true, zeroForOne, 0, reserveIn, reserveOut);
        }
        if (kind == T.V4) {
            (address currency0, address currency1) = zeroForOne ? (tokenIn, step.tokenOut) : (step.tokenOut, tokenIn);
            price = v4Price(step, currency0, currency1, index);
        } else {
            // slot0() for Uniswap, Pancake, Slipstream and Solidly V3; Algebra's globalState().
            price = firstWord(step.target, kind == T.CAMELOT_V3 ? 0xe76c01e4 : 0x3850c7bd, index);
        }
    }

    function firstWord(address target, uint256 selector, uint256 index) private view returns (uint256 price) {
        bool ok;
        assembly ("memory-safe") {
            mstore(0, shl(224, selector))
            ok := staticcall(gas(), target, 0, 4, 0, 32)
            ok := and(ok, iszero(lt(returndatasize(), 32)))
            price := and(mload(0), PRICE_MASK)
        }
        if (!ok) revert UnreadablePool(index);
    }

    function reserves(address target, uint256 index) private view returns (uint256 reserve0, uint256 reserve1) {
        bool ok;
        // getReserves(): uint112 words on V2, uint256 on Velodrome V2.
        assembly ("memory-safe") {
            mstore(0, shl(224, 0x0902f1ac))
            ok := staticcall(gas(), target, 0, 4, 0, 64)
            ok := and(ok, iszero(lt(returndatasize(), 64)))
            reserve0 := mload(0)
            reserve1 := mload(32)
        }
        if (!ok) revert UnreadablePool(index);
    }

    function v4Price(T.Step memory step, address currency0, address currency1, uint256 index)
        private
        view
        returns (uint256 price)
    {
        address manager = step.target;
        uint256 fee = step.fee;
        int256 spacing = step.tickSpacing;
        bool ok;
        // PoolId is the hash of the hooks-free PoolKey; slot0 is the first word
        // of the pool's state: sqrtPriceX96 in the low 160 bits.
        assembly ("memory-safe") {
            let key := mload(0x40)
            mstore(key, currency0)
            mstore(add(key, 32), currency1)
            mstore(add(key, 64), fee)
            mstore(add(key, 96), spacing)
            mstore(add(key, 128), 0)
            mstore(0, keccak256(key, 160))
            mstore(32, V4_POOLS_SLOT)
            let slot := keccak256(0, 64)
            mstore(0, shl(224, 0x1e2eaeaf))
            mstore(4, slot)
            ok := staticcall(gas(), manager, 0, 36, 0, 32)
            ok := and(ok, iszero(lt(returndatasize(), 32)))
            price := and(mload(0), PRICE_MASK)
        }
        if (!ok) revert UnreadablePool(index);
    }

    /// @dev a * b >= c * d over full 512-bit products.
    function atLeast(uint256 a, uint256 b, uint256 c, uint256 d) private pure returns (bool) {
        (uint256 high0, uint256 low0) = product(a, b);
        (uint256 high1, uint256 low1) = product(c, d);
        return high0 > high1 || (high0 == high1 && low0 >= low1);
    }

    function min(uint256 a, uint256 b) private pure returns (uint256) {
        return a < b ? a : b;
    }

    /// @dev floor(a * b / d) over a 512-bit product. A quotient that does not
    /// fit saturates: the callers cap it anyway.
    function mulDiv(uint256 a, uint256 b, uint256 d) internal pure returns (uint256 result) {
        (uint256 high, uint256 low) = product(a, b);
        if (high == 0) return low / d;
        if (high >= d) return type(uint256).max;
        // 512 by 256 division (Remco Bloemen, MIT): d > high, so the quotient fits.
        assembly ("memory-safe") {
            let remainder := mulmod(a, b, d)
            high := sub(high, gt(remainder, low))
            low := sub(low, remainder)
            let twos := and(sub(0, d), d)
            d := div(d, twos)
            low := div(low, twos)
            twos := add(div(sub(0, twos), twos), 1)
            low := or(low, mul(high, twos))
            let inverse := xor(mul(3, d), 2)
            inverse := mul(inverse, sub(2, mul(d, inverse)))
            inverse := mul(inverse, sub(2, mul(d, inverse)))
            inverse := mul(inverse, sub(2, mul(d, inverse)))
            inverse := mul(inverse, sub(2, mul(d, inverse)))
            inverse := mul(inverse, sub(2, mul(d, inverse)))
            inverse := mul(inverse, sub(2, mul(d, inverse)))
            result := mul(low, inverse)
        }
    }

    function product(uint256 a, uint256 b) private pure returns (uint256 high, uint256 low) {
        assembly ("memory-safe") {
            let full := mulmod(a, b, not(0))
            low := mul(a, b)
            high := sub(sub(full, low), lt(full, low))
        }
    }
}
