// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";

library PackedTestPlan {
    function encode(T.Plan memory plan) internal pure returns (bytes memory) {
        return encodeVersion(plan, new uint160[](plan.steps.length), 2, 0);
    }

    /// @dev Version 3: a nonzero bound sets step flag 2 and follows any price limit.
    function encodeChecked(T.Plan memory plan, uint160[] memory bounds) internal pure returns (bytes memory) {
        return encodeVersion(plan, bounds, 3, 0);
    }

    /// @dev Version 4: each nonzero entry is the step's price at the quote;
    /// minRate (1e18) follows the header.
    function encodeCumulative(T.Plan memory plan, uint160[] memory references, uint64 minRate)
        internal
        pure
        returns (bytes memory)
    {
        return encodeVersion(plan, references, 4, minRate);
    }

    function encodeVersion(T.Plan memory plan, uint160[] memory bounds, uint8 version, uint64 minRate)
        private
        pure
        returns (bytes memory encoded)
    {
        require(plan.deadline <= type(uint64).max, "deadline width");
        require(bounds.length == plan.steps.length, "bound count");
        encoded = abi.encodePacked(
            version,
            uint8(plan.funding),
            uint8(plan.steps.length),
            plan.asset,
            plan.amount,
            plan.minProfit,
            uint64(plan.deadline)
        );
        if (version == 4) encoded = bytes.concat(encoded, abi.encodePacked(minRate));
        if (plan.funding == T.Funding.V3Flash) {
            encoded = bytes.concat(encoded, abi.encodePacked(plan.lender, plan.lenderToken0, plan.maxLoanFee));
        }
        for (uint256 i; i < plan.steps.length; ++i) {
            T.Step memory step = plan.steps[i];
            uint8 flags = (step.sqrtPriceLimitX96 == 0 ? 0 : 1) | (bounds[i] == 0 ? 0 : 2);
            encoded = bytes.concat(
                encoded,
                abi.encodePacked(
                    step.kind, step.target, step.tokenOut, step.fee, step.tickSpacing, step.minAmountOut, flags
                )
            );
            if (step.sqrtPriceLimitX96 != 0) encoded = bytes.concat(encoded, abi.encodePacked(step.sqrtPriceLimitX96));
            if (bounds[i] != 0) encoded = bytes.concat(encoded, abi.encodePacked(bounds[i]));
        }
    }
}
