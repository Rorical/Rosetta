// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T} from "./ExecutorTypes.sol";

/// @dev Versions 2 to 4 use big-endian fixed-width fields. All calldata loads
/// are bounded first; exact consumption rejects truncated or appended instructions.
///
/// Header (95 bytes): version, funding, step count, asset(20), amount(32),
/// minProfit(32), deadline(8). Version 4 appends minRate(8), the lowest
/// product of the bounded legs' rate changes, in 1e18. Standalone V3 flash
/// then appends lender(20), lenderToken0(1), maxLoanFee(32). Each step is
/// kind(1), target(20), tokenOut(20), fee(3), tickSpacing(3), minAmountOut(32),
/// flags(1); flag 1 appends a 20-byte sqrt price limit. Versions 3 and 4 add
/// flag 2, which appends a nonzero 20-byte price after any limit: a bound in
/// version 3, the quote's price in version 4 (see PriceCheck).
library PackedPlan {
    error InvalidPackedPlan();

    /// @dev prices is empty for version 2; otherwise it has one entry per
    /// step, zero where the step is unchecked. minRate is nonzero only in
    /// version 4, whose prices are references for PriceCheck.checkCumulative.
    function decode(bytes calldata encoded)
        internal
        pure
        returns (T.Plan memory plan, uint256[] memory prices, uint256 minRate)
    {
        if (encoded.length < 95) revert InvalidPackedPlan();
        uint8 version = uint8(encoded[0]);
        uint8 funding = uint8(encoded[1]);
        uint256 count = uint8(encoded[2]);
        if (version < 2 || version > 4 || funding > uint8(T.Funding.Inventory) || count == 0 || count > 16) {
            revert InvalidPackedPlan();
        }
        plan.funding = T.Funding(funding);
        // Plan memory words: funding, lender, lenderToken0, asset, amount,
        // maxLoanFee, steps, minProfit, deadline.
        assembly ("memory-safe") {
            mstore(add(plan, 96), shr(96, calldataload(add(encoded.offset, 3))))
            mstore(add(plan, 128), calldataload(add(encoded.offset, 23)))
            mstore(add(plan, 224), calldataload(add(encoded.offset, 55)))
            mstore(add(plan, 256), shr(192, calldataload(add(encoded.offset, 87))))
        }
        uint256 cursor = 95;
        if (version == 4) {
            if (encoded.length < 103) revert InvalidPackedPlan();
            minRate = uint64(bytes8(encoded[95:103]));
            if (minRate == 0 || minRate >= 1e18) revert InvalidPackedPlan();
            cursor = 103;
        }
        if (plan.funding == T.Funding.V3Flash) {
            uint256 lender = cursor;
            if (encoded.length < lender + 53 || uint8(encoded[lender + 20]) > 1) revert InvalidPackedPlan();
            assembly ("memory-safe") {
                mstore(add(plan, 32), shr(96, calldataload(add(encoded.offset, lender))))
                mstore(add(plan, 64), byte(0, calldataload(add(encoded.offset, add(lender, 20)))))
                mstore(add(plan, 160), calldataload(add(encoded.offset, add(lender, 21))))
            }
            cursor = lender + 53;
        }
        plan.steps = new T.Step[](count);
        T.Step[] memory steps = plan.steps;
        uint256 maxFlags = 1;
        if (version >= 3) {
            prices = new uint256[](count);
            maxFlags = 3;
        }
        // The array already owns each Step. Fill those objects directly rather
        // than allocate and then replace a second struct for every route leg.
        // Step memory words: kind, target, tokenOut, fee, tickSpacing,
        // minAmountOut, sqrtPriceLimitX96. count <= 16 bounds the cursor.
        assembly ("memory-safe") {
            function invalidEncoding() {
                mstore(0, shl(224, 0x736c735f))
                revert(0, 4)
            }
            // Reads a nonzero 20-byte field at cursor.
            function nonzero160(data, length, at) -> value {
                if lt(length, add(at, 20)) { invalidEncoding() }
                value := shr(96, calldataload(add(data, at)))
                if iszero(value) { invalidEncoding() }
            }
            for { let i := 0 } lt(i, count) { i := add(i, 1) } {
                if lt(encoded.length, add(cursor, 80)) { invalidEncoding() }
                let step := mload(add(add(steps, 32), mul(i, 32)))
                let start := add(encoded.offset, cursor)
                mstore(step, byte(0, calldataload(start)))
                mstore(add(step, 32), shr(96, calldataload(add(start, 1))))
                mstore(add(step, 64), shr(96, calldataload(add(start, 21))))
                mstore(add(step, 96), shr(232, calldataload(add(start, 41))))
                mstore(add(step, 128), signextend(2, shr(232, calldataload(add(start, 44)))))
                mstore(add(step, 160), calldataload(add(start, 47)))
                let flags := byte(0, calldataload(add(start, 79)))
                cursor := add(cursor, 80)
                if flags {
                    if gt(flags, maxFlags) { invalidEncoding() }
                    if and(flags, 1) {
                        mstore(add(step, 192), nonzero160(encoded.offset, encoded.length, cursor))
                        cursor := add(cursor, 20)
                    }
                    if and(flags, 2) {
                        let bound := nonzero160(encoded.offset, encoded.length, cursor)
                        mstore(add(add(prices, 32), mul(i, 32)), bound)
                        cursor := add(cursor, 20)
                    }
                }
            }
            if iszero(eq(cursor, encoded.length)) { invalidEncoding() }
        }
    }
}
