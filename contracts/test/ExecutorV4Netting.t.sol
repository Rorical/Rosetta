// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {V4NettingFixture} from "./V4NettingFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {TestV4Manager} from "./V4Mock.sol";

contract ExecutorV4NettingTest is V4NettingFixture {
    function testCycleTakesOnlyNetProfitAndPreservesEveryInventory() public {
        T.Plan memory plan = cyclePlan();
        token0.mint(address(executor), 123);
        token1.mint(address(executor), 456);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.unlock.selector), 1);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.settle.selector), 0);
        vm.expectCall(address(manager), abi.encodeCall(manager.take, (address(token0), address(executor), 200)), 1);
        vm.expectCall(address(token1), abi.encodeWithSelector(token1.transfer.selector), 0);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 200);
        assertKept(token0, 123 + 200);
        assertEq(token1.balanceOf(address(executor)), 456);
        assertEq(token0.balanceOf(address(manager)), 9_999_800);
        assertEq(token1.balanceOf(address(manager)), 10_000_000);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }

    function testFlashSwapTailSettlesBoundariesAndKeepsNativeInsideManager() public {
        T.Plan memory plan = flashTailPlan();
        vm.prank(OPERATOR);
        token0.approve(address(executor), 0);
        vm.deal(address(executor), 77);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.unlock.selector), 1);
        vm.expectCall(address(manager), abi.encodeCall(manager.sync, (address(token1))), 1);
        vm.expectCall(address(manager), abi.encodeCall(manager.take, (address(0), address(executor), 2000)), 0);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(profit, 200);
        assertEq(fee, 0);
        assertEq(address(executor).balance, 77);
        assertEq(address(manager).balance, 10_000_000);
        assertEq(token1.balanceOf(address(manager)), 10_002_000);
        assertEq(token0.balanceOf(address(manager)), 9_998_800);
    }

    function testStandaloneFlashCanFundANettedCycle() public {
        T.Plan memory plan = cyclePlan();
        lend(plan, register(T.V3, address(v3Pool(1_000_000, 0))));
        plan.funding = T.Funding.V3Flash;
        plan.maxLoanFee = 5;
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(profit, 195);
        assertEq(fee, 5);
    }

    function testReverseAndNativeCycles() public {
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.settle.selector), 0);
        for (uint256 i; i < 2; ++i) {
            address asset = i == 0 ? address(token1) : address(0);
            bytes32 first = pool(asset, address(token0), 3000, 2_000_000);
            bytes32 second = pool(asset, address(token0), 500, 600_000);
            T.Plan memory plan = walletPlan(first, second, asset, address(token0));
            vm.deal(OPERATOR, 10_000);
            vm.prank(OPERATOR);
            (uint256 profit,) = executor.execute{value: i == 0 ? 0 : 1000}(plan);
            assertEq(profit, 200);
            assertEq(OPERATOR.balance, 10_000);
        }
    }

    function testOpenBatchNativeInputAndOutput() public {
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.unlock.selector), 2);
        for (uint256 i; i < 2; ++i) {
            address asset = i == 0 ? address(0) : address(token0);
            address end = i == 0 ? address(token0) : address(0);
            bytes32 first = pool(asset, address(token1), 3000, 2_000_000);
            bytes32 second = pool(address(token1), end, 500, 1_000_000);
            TestV4Manager other = new TestV4Manager(600_000);
            vm.deal(address(other), 1_000_000);
            token0.mint(address(other), 1_000_000);
            bytes32 last = registerPool(T.V4, address(other), address(0), address(token0), 3000, 60);
            T.Plan memory plan = walletPlan(first, second, asset, address(token1));
            plan.steps = new T.Step[](3);
            plan.steps[0] = step(first, asset, 1900, 0);
            plan.steps[1] = step(second, address(token1), 1900, 0);
            plan.steps[2] = step(last, end, 1100, 0);
            vm.deal(OPERATOR, 10_000);
            vm.expectCall(address(other), abi.encodeWithSelector(other.unlock.selector), 1);
            vm.prank(OPERATOR);
            (uint256 profit,) = executor.execute{value: i == 0 ? 1000 : 0}(plan);
            assertEq(profit, 200);
            // The native round keeps its profit; the token0 round only passes native through.
            assertEq(address(executor).balance, 200);
            assertKept(token0, i * 200);
        }
    }

    function testLossAndZeroNetSegmentsSpendOnlyTheirNetDebt() public {
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.settle.selector), 1);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.take.selector), 0);
        for (uint256 i; i < 2; ++i) {
            T.Plan memory plan = cyclePlan();
            plan.steps[1] =
                step(pool(address(token0), address(token1), 500, i == 0 ? 450_000 : 500_000), address(token1), 1100, 0);
            bytes32 third = register(T.V3, address(v3Pool(2_000_000, 0)));
            bytes32 last = register(T.V3, address(v3Pool(600_000, 0)));
            T.Step memory first = plan.steps[0];
            T.Step memory second = plan.steps[1];
            second.minAmountOut = 900;
            plan.steps = new T.Step[](4);
            plan.steps[0] = first;
            plan.steps[1] = second;
            plan.steps[2] = step(third, address(token0), 1, 0);
            plan.steps[3] = step(last, address(token1), 1, 0);
            plan.minProfit = 50;
            vm.prank(OPERATOR);
            (uint256 profit,) = executor.execute(plan);
            assertEq(profit, i == 0 ? 80 : 200);
        }
    }

    function testEveryLegRejectsBeforeAnySettlement() public {
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.sync.selector), 0);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.take.selector), 0);
        for (uint256 i; i < 2; ++i) {
            T.Plan memory plan = cyclePlan();
            plan.steps[i].minAmountOut = i == 0 ? 2001 : 1201;
            vm.expectRevert(SwapExecutor.Slippage.selector);
            vm.prank(OPERATOR);
            executor.execute(plan);
        }
    }

    function testProtocolBoundaryClosesUnlockBeforeReusingManager() public {
        bytes32 first = pool(address(token0), address(token1), 3000, 1_500_000);
        bytes32 third = pool(address(token0), address(token1), 500, 1_000_000);
        bytes32 second = register(T.V3, address(v3Pool(1_000_000, 0)));
        bytes32 last = register(T.V3, address(v3Pool(800_000, 0)));
        T.Plan memory plan = walletPlan(first, second, address(token0), address(token1));
        plan.steps = new T.Step[](4);
        plan.steps[0] = step(first, address(token0), 1, 0);
        plan.steps[1] = step(second, address(token1), 1, 0);
        plan.steps[2] = step(third, address(token0), 1, 0);
        plan.steps[3] = step(last, address(token1), 1, 0);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.unlock.selector), 2);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 200);
    }

    function testInputOutsideSignedDeltaRangeRejectsBeforeSwap() public {
        T.Plan memory plan = cyclePlan();
        plan.amount = uint256(uint128(type(int128).max)) + 1;
        token0.mint(OPERATOR, plan.amount);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.swap.selector), 0);
        vm.expectRevert(SwapExecutor.InvalidSwap.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testOldInventoryCannotSubsidizeInsufficientProfit() public {
        T.Plan memory plan = cyclePlan();
        token0.mint(address(executor), 10_000);
        plan.minProfit = 201;
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.take.selector), 0);
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(address(executor)), 10_000);
    }

    function testCallbacksPartialFillSettlementAndFalseResultRollback() public {
        T.Plan memory plan = flashTailPlan();
        // The exact profit as the minimum: a manager that takes a wei less
        // leaves the route short of it.
        plan.minProfit = 200;
        for (uint8 mode = 1; mode <= 6; ++mode) {
            manager.setMode(mode);
            vm.expectRevert();
            vm.prank(OPERATOR);
            executor.execute(plan);
            assertEq(token0.balanceOf(address(manager)), 10_000_000);
            assertEq(token1.balanceOf(address(manager)), 10_000_000);
            assertEq(token0.balanceOf(address(executor)), 0);
        }
        // An inflated unlock result changes nothing that arrived: the plan's
        // checks read actual balances, so the route keeps its exact profit.
        manager.setMode(7);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 200);
        manager.setMode(0);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testCompletedBatchCannotReplayAndStateClearsForNextExecution() public {
        T.Plan memory plan = cyclePlan();
        manager.setMode(8);
        vm.prank(OPERATOR);
        executor.execute(plan);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        manager.setMode(0);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertKept(token0, 400);
    }

    function testIntermediateInventoryChangeIsRejected() public {
        T.Plan memory plan = cyclePlan();
        token1.mint(address(executor), 456);
        manager.setDisturbedToken(address(token1));
        vm.expectRevert(SwapExecutor.InvalidSwap.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token1.balanceOf(address(executor)), 456);
    }

    function testTaxedBoundaryInputAndOutputRevert() public {
        T.Plan memory plan = flashTailPlan();
        // Enable tax after the V3 first hop delivers its full output, before V4 settlement.
        token1.reenter(address(token1), abi.encodeCall(token1.configure, (10_000, false, false)));
        vm.expectRevert();
        vm.prank(OPERATOR);
        executor.execute(plan);
        token1.reenter(address(0), "");
        token0.configure(10_000, false, false);
        // The first pool rejects the taxed payment that completes it.
        vm.expectRevert();
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(address(manager)), 10_000_000);
    }

    function testFuzzChainedOutputAndDust(uint32 seed, uint64 dust) public {
        T.Plan memory plan = cyclePlan();
        plan.amount = 10 + uint256(seed) % 90_000;
        plan.minProfit = 1;
        plan.steps[0].minAmountOut = 1;
        plan.steps[1].minAmountOut = 1;
        token0.mint(address(executor), dust);
        token1.mint(address(executor), dust);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, plan.amount * 2 * 6 / 10 - plan.amount);
        assertKept(token0, dust + profit);
        assertEq(token1.balanceOf(address(executor)), dust);
    }
}
