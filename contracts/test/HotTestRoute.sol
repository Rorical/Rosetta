// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";

/// @dev executeHot's route for a plan's asset, amount, minProfit and steps.
library HotTestRoute {
    /// @dev A route of V3-family pools: the last is swapped for the amount it
    /// takes in, lastInput, and the pools before it are asked for what is owed.
    function encode(T.Plan memory plan, uint256 lastInput) internal pure returns (bytes memory route) {
        route = abi.encodePacked(plan.asset, uint128(plan.amount), uint128(plan.minProfit), uint8(plan.steps.length));
        address tokenIn = plan.asset;
        for (uint256 i; i < plan.steps.length; ++i) {
            address tokenOut = plan.steps[i].tokenOut;
            route = bytes.concat(
                route,
                abi.encodePacked(
                    uint8(tokenIn < tokenOut ? 1 : 0),
                    uint88(i + 1 == plan.steps.length ? lastInput : 0),
                    plan.steps[i].target
                )
            );
            tokenIn = tokenOut;
        }
    }

    /// @dev Sets the claim bit of the steps byte.
    function claim(bytes memory route) internal pure returns (bytes memory) {
        route[52] = bytes1(uint8(route[52]) | 0x80);
        return route;
    }

    /// @dev Appends a sqrt price bound for step index; algebra reads globalState.
    function bound(bytes memory route, uint8 index, bool algebra, uint160 price) internal pure returns (bytes memory) {
        return bytes.concat(route, abi.encodePacked(algebra ? index | 0x80 : index, price));
    }
}
