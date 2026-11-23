// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {TokenActions} from "../src/TokenActions.sol";
import {TestV3Pool, TestV2Pool} from "./PoolMocks.sol";

contract PayloadTamperingPool {
    address public immutable token0;
    address public immutable token1;

    constructor(address a, address b) {
        token0 = a;
        token1 = b;
    }

    function swap(address, bool, int256 amount, uint160, bytes calldata payload) external returns (int256, int256) {
        SwapExecutor.SwapPayment memory payment = abi.decode(payload, (SwapExecutor.SwapPayment));
        payment.minimum = 0;
        payment.profitFloor = 0;
        SwapExecutor(payable(msg.sender)).uniswapV3SwapCallback(amount, -2 * amount, abi.encode(payment));
        return (amount, -2 * amount);
    }
}

contract ExecutorOptimizationTest is ExecutorFixture {
    TestV3Pool private first;
    TestV3Pool private second;
    bytes32 private lenderKey;
    bytes private encodedPlan;

    function setUp() public override {
        super.setUp();
        first = v3Pool(2_000_000, 0);
        second = v3Pool(600_000, 0);
        bytes32 key0 = register(T.V3, address(first));
        bytes32 key1 = register(T.V3, address(second));
        lenderKey = register(T.V3, address(v3Pool(1_000_000, 0)));
        encodedPlan = abi.encode(walletPlan(key0, key1, address(token0), address(token1)));
    }

    function savedPlan() private view returns (T.Plan memory) {
        return abi.decode(encodedPlan, (T.Plan));
    }

    function testRejectsEntireRouteBeforePullingPrincipal() public {
        T.Plan memory plan = savedPlan();
        vm.expectCall(address(token0), abi.encodeWithSelector(0x23b872dd), 0);
        plan.steps[1].tokenOut = address(0x9999);
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testRejectsLastPriceLimitBeforeCallingFirstPool() public {
        T.Plan memory plan = savedPlan();
        plan.steps[1].sqrtPriceLimitX96 = 1;
        vm.expectCall(address(first), abi.encodeWithSelector(first.swap.selector), 0);
        vm.expectRevert(SwapExecutor.InvalidSwap.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testV3SlippageRejectsBeforeCallbackPayment() public {
        T.Plan memory plan = savedPlan();
        plan.steps[0].minAmountOut = 2001;
        vm.expectCall(address(token0), abi.encodeCall(token0.transfer, (address(first), 1000)), 0);
        vm.expectRevert(SwapExecutor.Slippage.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testProfitRejectsBeforeLastCallbackPayment() public {
        T.Plan memory plan = savedPlan();
        plan.minProfit = 201;
        vm.expectCall(address(token1), abi.encodeCall(token1.transfer, (address(second), 2000)), 0);
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testV2SlippageRejectsBeforePoolPayment() public {
        TestV2Pool pair = new TestV2Pool(address(token0), address(token1));
        token0.mint(address(pair), 1_000_000);
        token1.mint(address(pair), 2_000_000);
        pair.sync();
        T.Plan memory plan = savedPlan();
        plan.steps[0].kind = T.V2;
        plan.steps[0].target = address(pair);
        plan.steps[0].minAmountOut = 2000;
        vm.expectCall(address(token0), abi.encodeCall(token0.transfer, (address(pair), 1000)), 0);
        vm.expectRevert(SwapExecutor.Slippage.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testRevertedSettlementLeavesNoLock() public {
        vm.warp(2 days);
        T.Plan memory plan = savedPlan();
        plan.deadline = block.timestamp + 60;
        // Returning the principal is the last settlement transfer.
        bytes memory payment = abi.encodeCall(token0.transfer, (OPERATOR, 1000));
        vm.mockCallRevert(address(token0), payment, hex"12345678");
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        vm.clearMockedCalls();
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
        assertKept(token0, 200);
    }

    function testMixedFundingCallsShareNoCompletedLoanState() public {
        T.Plan memory plan = savedPlan();
        for (uint256 i; i < 6; ++i) {
            plan.funding = T.Funding(i % 3);
            bool flash = plan.funding == T.Funding.V3Flash;
            plan.lender = address(0);
            plan.lenderToken0 = false;
            if (flash) lend(plan, lenderKey);
            plan.maxLoanFee = flash ? 5 : 0;
            vm.prank(OPERATOR);
            (uint256 profit, uint256 fee) = executor.execute(plan);
            assertEq(profit, flash ? 195 : 200);
            assertEq(fee, flash ? 5 : 0);
        }
        assertKept(token0, 1190);
    }

    function testPreviousExecutionPayloadCannotAuthorizeAnotherSwap() public {
        first.setMode(11);
        T.Plan memory plan = savedPlan();
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

    function testFlashDirectionResetsBetweenConsecutiveLoans() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3Flash;
        plan.maxLoanFee = 5;
        for (uint256 i; i < 2; ++i) {
            plan.asset = i == 0 ? address(token0) : address(token1);
            lend(plan, lenderKey);
            plan.steps[0].tokenOut = i == 0 ? address(token1) : address(token0);
            plan.steps[1].tokenOut = plan.asset;
            vm.prank(OPERATOR);
            (uint256 profit, uint256 fee) = executor.execute(plan);
            assertEq(profit, 195);
            assertEq(fee, 5);
        }
        assertKept(token0, 195);
        assertKept(token1, 195);
    }

    function testPoolCannotLowerAuthenticatedOutputFloors() public {
        PayloadTamperingPool pool = new PayloadTamperingPool(address(token0), address(token1));
        T.Plan memory plan = savedPlan();
        plan.steps[0].kind = T.V3;
        plan.steps[0].target = address(pool);
        plan.steps[0].minAmountOut = 2001;
        vm.expectCall(address(token0), abi.encodeCall(token0.transfer, (address(pool), 1000)), 0);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }
}
