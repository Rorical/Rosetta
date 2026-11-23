// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {V4NettingFixture} from "./V4NettingFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";

contract ExecutorV4SingleTest is V4NettingFixture {
    error PoolFailure();

    function testShortSwapReturnRejectedAndPoolErrorPreserved() public {
        T.Plan memory plan = abi.decode(encodedSeparatedCycle, (T.Plan));
        bytes memory selector = abi.encodeWithSelector(manager.swap.selector);
        vm.mockCall(address(manager), selector, hex"01");
        vm.expectRevert(SwapExecutor.InvalidSwap.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        vm.clearMockedCalls();
        vm.mockCallRevert(address(manager), selector, abi.encodeWithSelector(PoolFailure.selector));
        vm.expectRevert(PoolFailure.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testSingleFailuresKeepInventory() public {
        T.Plan memory plan = abi.decode(encodedSeparatedCycle, (T.Plan));
        token0.mint(address(executor), 123);
        token1.mint(address(executor), 456);
        for (uint8 mode = 1; mode <= 7; ++mode) {
            manager.setMode(mode);
            vm.expectRevert();
            vm.prank(OPERATOR);
            executor.execute(plan);
            assertEq(token0.balanceOf(address(executor)), 123);
            assertEq(token1.balanceOf(address(executor)), 456);
        }
        manager.setMode(0);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 200);
    }

    function testSinglePayloadCannotReplayIntoSingleOrBatch() public {
        T.Plan memory plan = abi.decode(encodedSeparatedCycle, (T.Plan));
        manager.setMode(8);
        vm.prank(OPERATOR);
        executor.execute(plan);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan = cyclePlan();
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        manager.setMode(0);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertKept(token0, 400);
    }
}
