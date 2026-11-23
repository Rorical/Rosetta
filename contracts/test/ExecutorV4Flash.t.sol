// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {V4NettingFixture} from "./V4NettingFixture.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {HotV3Executor} from "../src/HotV3Executor.sol";
import {ExecutorAdmin} from "../src/ExecutorAdmin.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";

contract ExecutorV4FlashTest is V4NettingFixture {
    event GasMeasured(string scenario, uint256 gasUsed);

    function unfundedPlan() private returns (T.Plan memory plan) {
        plan = cyclePlan();
        plan.funding = T.Funding.V4Flash;
        vm.prank(OPERATOR);
        token0.approve(address(executor), 0);
        uint256 balance = token0.balanceOf(OPERATOR);
        vm.prank(OPERATOR);
        token0.transfer(address(0xdead), balance);
    }

    function testOnlyNetProfitMovesWithoutPrincipalOrAllowance() public {
        T.Plan memory plan = unfundedPlan();
        token0.mint(address(executor), 123);
        token1.mint(address(executor), 456);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.unlock.selector), 1);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.sync.selector), 0);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.settle.selector), 0);
        vm.expectCall(address(manager), abi.encodeCall(manager.take, (address(token0), address(executor), 200)), 1);
        vm.expectCall(address(token0), abi.encodeWithSelector(token0.transferFrom.selector), 0);
        vm.expectCall(address(token0), abi.encodeCall(token0.transfer, (OPERATOR, plan.amount)), 0);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(profit, 200);
        assertEq(fee, 0);
        assertEq(token0.balanceOf(OPERATOR), 0);
        assertKept(token0, 123 + 200);
        assertEq(token1.balanceOf(address(executor)), 456);
        assertEq(token0.balanceOf(address(manager)), 9_999_800);
    }

    function testNativeCycleUsesZeroValueAndKeepsDust() public {
        bytes32 first = pool(address(0), address(token0), 3000, 2_000_000);
        bytes32 second = pool(address(0), address(token0), 500, 600_000);
        T.Plan memory plan = walletPlan(first, second, address(0), address(token0));
        plan.funding = T.Funding.V4Flash;
        vm.deal(OPERATOR, 0);
        vm.deal(address(executor), 77);
        uint256 ownerBalance = address(this).balance;
        bytes memory payload = PackedTestPlan.encode(plan);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.executePacked(payload);
        assertEq(profit, 200);
        assertEq(fee, 0);
        assertEq(OPERATOR.balance, 0);
        assertEq(address(executor).balance, 77 + 200);
        assertEq(address(this).balance, ownerBalance);
        assertEq(address(manager).balance, 9_999_800);
    }

    function testRejectsMixedProtocolsManagersAndOpenRouteBeforeUnlock() public {
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.unlock.selector), 0);
        for (uint256 i; i < 3; ++i) {
            T.Plan memory plan = i == 0 ? flashTailPlan() : abi.decode(encodedSeparatedCycle, (T.Plan));
            plan.funding = T.Funding.V4Flash;
            if (i == 2) {
                plan = cyclePlan();
                plan.funding = T.Funding.V4Flash;
                plan.steps = new T.Step[](1);
                plan.steps[0] = cyclePlan().steps[0];
            }
            vm.expectRevert(SwapExecutor.InvalidRoute.selector);
            vm.prank(OPERATOR);
            executor.execute(plan);
        }
    }

    function testRejectsLoanTermsAndUnexpectedValue() public {
        for (uint256 i; i < 3; ++i) {
            T.Plan memory plan = cyclePlan();
            plan.funding = T.Funding.V4Flash;
            if (i == 0) plan.lender = plan.steps[0].target;
            if (i == 1) plan.maxLoanFee = 1;
            vm.deal(OPERATOR, 1);
            vm.expectRevert(HotV3Executor.InvalidPlan.selector);
            vm.prank(OPERATOR);
            executor.execute{value: i == 2 ? 1 : 0}(plan);
        }
    }

    function testInventoryCannotCoverLossOrInsufficientProfit() public {
        token0.mint(address(executor), 10_000);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.settle.selector), 0);
        vm.expectCall(address(manager), abi.encodeWithSelector(manager.take.selector), 0);
        for (uint256 i; i < 3; ++i) {
            T.Plan memory plan = cyclePlan();
            plan.funding = T.Funding.V4Flash;
            plan.minProfit = i == 0 ? 201 : 1;
            plan.steps[1].minAmountOut = 1;
            if (i != 0) {
                plan.steps[1] =
                    step(pool(plan.asset, address(token1), 500, i == 1 ? 450_000 : 500_000), address(token1), 1, 0);
            }
            vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
            vm.prank(OPERATOR);
            executor.execute(plan);
            assertEq(token0.balanceOf(address(executor)), 10_000);
        }
    }

    function testCallbacksPartialFillsAndFalseOutputRollback() public {
        T.Plan memory plan = unfundedPlan();
        // The exact profit as the minimum: a manager that takes a wei less
        // leaves the route short of it.
        plan.minProfit = 200;
        for (uint8 mode = 1; mode <= 6; ++mode) {
            if (mode == 5) continue; // No settle call exists in this funding mode.
            manager.setMode(mode);
            vm.expectRevert();
            vm.prank(OPERATOR);
            executor.execute(plan);
            assertEq(token0.balanceOf(address(manager)), 10_000_000);
            assertEq(token0.balanceOf(address(executor)), 0);
        }
        manager.setMode(0);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertKept(token0, 200);
    }

    function testOldPayloadCannotReplay() public {
        T.Plan memory plan = unfundedPlan();
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

    function testFuzzUnfundedInputAndDust(uint32 seed, uint64 dust) public {
        T.Plan memory plan = unfundedPlan();
        plan.amount = 10 + uint256(seed) % 90_000;
        plan.minProfit = 1;
        plan.steps[0].minAmountOut = 1;
        plan.steps[1].minAmountOut = 1;
        token0.mint(address(executor), dust);
        token1.mint(address(executor), dust);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, plan.amount * 12 / 10 - plan.amount);
        assertEq(token0.balanceOf(OPERATOR), 0);
        assertKept(token0, dust + profit);
        assertEq(token1.balanceOf(address(executor)), dust);
    }

    function testGasV4FlashCycle() public {
        measure("v4_unfunded_cycle", false);
    }

    function testGasPackedV4FlashCycle() public {
        measure("packed_v4_unfunded_cycle", true);
    }

    function measure(string memory scenario, bool packed) private {
        T.Plan memory plan = cyclePlan();
        plan.funding = T.Funding.V4Flash;
        bytes memory payload = packed
            ? abi.encodeCall(executor.executePacked, (PackedTestPlan.encode(plan)))
            : abi.encodeCall(executor.execute, (plan));
        vm.prank(OPERATOR);
        (bool ok, bytes memory result, uint256 used) = measuredCall(address(executor), payload);
        require(ok, "execution failed");
        emit GasMeasured(scenario, used);
        (uint256 profit, uint256 fee) = abi.decode(result, (uint256, uint256));
        assertEq(profit, 200);
        assertEq(fee, 0);
    }
}
