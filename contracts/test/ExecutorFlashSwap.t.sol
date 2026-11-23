// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {HotV3Executor} from "../src/HotV3Executor.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {TokenActions} from "../src/TokenActions.sol";
import {TestV3Pool} from "./PoolMocks.sol";
import {TestV4Manager} from "./V4Mock.sol";

contract ExecutorFlashSwapTest is ExecutorFixture {
    TestV3Pool private first;
    TestV3Pool private second;
    bytes private encodedPlan;

    function setUp() public override {
        super.setUp();
        first = v3Pool(2_000_000, 0);
        second = v3Pool(600_000, 0);
        bytes32 key0 = register(T.V3, address(first));
        bytes32 key1 = register(T.V3, address(second));
        T.Plan memory plan = walletPlan(key0, key1, address(token0), address(token1));
        plan.funding = T.Funding.V3FlashSwap;
        encodedPlan = abi.encode(plan);
        vm.prank(OPERATOR);
        token0.approve(address(executor), 0);
        vm.prank(OPERATOR);
        token1.approve(address(executor), 0);
    }

    function savedPlan() private view returns (T.Plan memory) {
        return abi.decode(encodedPlan, (T.Plan));
    }

    function testNoPrincipalTransferOrStandaloneLoanAndDustPreserved() public {
        T.Plan memory plan = savedPlan();
        token0.mint(address(executor), 123);
        token1.mint(address(executor), 456);
        vm.expectCall(address(token0), abi.encodeWithSelector(token0.transferFrom.selector), 0);
        vm.expectCall(address(first), abi.encodeWithSelector(first.flash.selector), 0);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(profit, 200);
        assertEq(fee, 0);
        assertKept(token0, 123 + 200);
        assertEq(token1.balanceOf(address(executor)), 456);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
        assertEq(token0.balanceOf(address(first)), 10_001_000);
        assertEq(token1.balanceOf(address(first)), 9_998_000);
    }

    function testSlipstreamFlashSwapWithoutWalletApproval() public {
        T.Plan memory plan = savedPlan();
        plan.steps[0].kind = T.SLIPSTREAM;
        plan.steps[0].target = address(first);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(profit, 200);
        assertEq(fee, 0);
    }

    function testSolidlyFlashSwapWithoutWalletApproval() public {
        TestV3Pool solidly = v3Pool(2_000_000, 3);
        T.Plan memory plan = savedPlan();
        plan.steps[0].kind = T.SOLIDLY_V3;
        plan.steps[0].target = address(solidly);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(profit, 200);
        assertEq(fee, 0);
        assertKept(token0, 200);
        assertEq(token1.balanceOf(address(executor)), 0);
    }

    function testPancakeAndReverseDirectionWithoutInventory() public {
        TestV3Pool pancake = v3Pool(2_000_000, 1);
        T.Plan memory plan = savedPlan();
        plan.steps[0].kind = T.V3;
        plan.steps[0].target = address(pancake);
        plan.asset = address(token1);
        plan.steps[0].tokenOut = address(token0);
        plan.steps[1].tokenOut = address(token1);
        for (uint256 i; i < 2; ++i) {
            vm.prank(OPERATOR);
            (uint256 profit, uint256 fee) = executor.execute(plan);
            assertEq(profit, 200);
            assertEq(fee, 0);
            // Sweeping the kept profit makes the second run start without inventory too.
            executor.sweep(address(token1), profit);
        }
        assertEq(token1.balanceOf(address(this)), 400);
        assertEq(token0.balanceOf(address(executor)), 0);
        assertEq(token1.balanceOf(address(executor)), 0);
    }

    function testMalformedFirstCallbacksAndReturnValuesRollback() public {
        T.Plan memory plan = savedPlan();
        for (uint8 mode = 1; mode <= 8; ++mode) {
            first.setMode(mode);
            vm.expectRevert();
            vm.prank(OPERATOR);
            executor.execute(plan);
            assertEq(token0.balanceOf(address(first)), 10_000_000);
            assertEq(token1.balanceOf(address(first)), 10_000_000);
            assertEq(token0.balanceOf(address(executor)), 0);
        }
        first.setMode(0);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testMalformedTailCannotLeaveTheFirstSwapPaid() public {
        T.Plan memory plan = savedPlan();
        // The exact profit as the minimum: a tail that delivers or reports a
        // wei less leaves the route short of it.
        plan.minProfit = 200;
        for (uint8 mode = 1; mode <= 8; ++mode) {
            second.setMode(mode);
            vm.expectRevert();
            vm.prank(OPERATOR);
            executor.execute(plan);
            assertEq(token0.balanceOf(address(first)), 10_000_000);
            assertEq(token1.balanceOf(address(first)), 10_000_000);
        }
    }

    /// @dev A taxed first output leaves less than the tail pays: without
    /// inventory the payment fails, with it the pool rejects the taxed transfer.
    function testTaxedFirstOutputCannotBePaidFromInventory() public {
        T.Plan memory plan = savedPlan();
        token1.configure(10_000, false, false);
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        token1.mint(address(executor), 1_000_000);
        vm.expectRevert();
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token1.balanceOf(address(first)), 10_000_000);
        assertEq(token1.balanceOf(address(executor)), 1_000_000);
    }

    function testCompletedContinuationCannotBeReplayed() public {
        T.Plan memory plan = savedPlan();
        first.setMode(11);
        vm.prank(OPERATOR);
        executor.execute(plan);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertKept(token0, 200);
        first.setMode(0);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testSlippageRejectsBeforeTailAndFirstPayment() public {
        T.Plan memory plan = savedPlan();
        plan.steps[0].minAmountOut = 2001;
        vm.expectCall(address(second), abi.encodeWithSelector(second.swap.selector), 0);
        vm.expectCall(address(token0), abi.encodeCall(token0.transfer, (address(first), 1000)), 0);
        vm.expectRevert(SwapExecutor.Slippage.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testOldInventoryCannotSubsidizeProfit() public {
        T.Plan memory plan = savedPlan();
        token0.mint(address(executor), 10_000);
        plan.minProfit = 201;
        vm.expectCall(address(token0), abi.encodeCall(token0.transfer, (address(first), 1000)), 0);
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(address(executor)), 10_000);
    }

    function testFirstPoolCannotAppearInTail() public {
        T.Plan memory plan = savedPlan();
        plan.steps[1].kind = plan.steps[0].kind;
        plan.steps[1].target = plan.steps[0].target;
        vm.expectCall(address(first), abi.encodeWithSelector(first.swap.selector), 0);
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testUnsupportedFirstProtocolAndLoanTermsRejected() public {
        T.Plan memory plan = savedPlan();
        plan.steps[0].kind = T.CAMELOT_V3;
        plan.steps[0].target = address(first);
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan = savedPlan();
        plan.maxLoanFee = 1;
        vm.expectRevert(HotV3Executor.InvalidPlan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan.maxLoanFee = 0;
        plan.lender = plan.steps[0].target;
        vm.expectRevert(HotV3Executor.InvalidPlan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testSixteenStepsReuseOnlyUnlockedTailPools() public {
        T.Plan memory plan = savedPlan();
        bytes32 third = register(T.V3, address(v3Pool(2_000_000, 0)));
        T.Step memory initial = plan.steps[0];
        T.Step memory returning = plan.steps[1];
        plan.steps = new T.Step[](16);
        plan.steps[0] = initial;
        uint256 expected = 2000;
        for (uint256 i = 1; i < 16; ++i) {
            plan.steps[i] = i % 2 == 1 ? returning : step(third, address(token0), 1, 0);
            expected = i % 2 == 1 ? expected * 6 / 10 : expected * 2;
        }
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, expected - 1000);
    }

    function testTailCanSettleV4ThroughNativeCurrency() public {
        T.Plan memory plan = savedPlan();
        TestV4Manager middle = new TestV4Manager(1_000_000);
        TestV4Manager last = new TestV4Manager(600_000);
        vm.deal(address(middle), 1_000_000);
        token0.mint(address(last), 1_000_000);
        bytes32 key1 = nativePool(address(middle), address(token1));
        bytes32 key2 = nativePool(address(last), address(token0));
        T.Step memory initial = plan.steps[0];
        plan.steps = new T.Step[](3);
        plan.steps[0] = initial;
        plan.steps[1] = step(key1, address(token1), 1900, 0);
        plan.steps[2] = step(key2, address(0), 1100, 0);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 200);
        assertEq(address(executor).balance, 0);
    }

    function nativePool(address manager, address token) private returns (bytes32) {
        return registerPool(T.V4, manager, address(0), token, 3000, 60);
    }
}
