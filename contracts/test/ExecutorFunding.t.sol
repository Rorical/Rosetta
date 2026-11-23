// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {ArbitrageExecutor} from "../src/ArbitrageExecutor.sol";
import {HotV3Executor} from "../src/HotV3Executor.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {TokenActions} from "../src/TokenActions.sol";
import {TestV3Pool} from "./PoolMocks.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";

contract ExecutorFundingTest is ExecutorFixture {
    TestV3Pool private lender;
    bytes private encodedPlan;

    function setUp() public override {
        super.setUp();
        TestV3Pool first = v3Pool(2_000_000, 0);
        TestV3Pool second = v3Pool(600_000, 0);
        lender = v3Pool(1_000_000, 0);
        bytes32 key0 = register(T.V3, address(first));
        bytes32 key1 = register(T.V3, address(second));
        bytes32 loan = register(T.V3, address(lender));
        T.Plan memory plan = walletPlan(key0, key1, address(token0), address(token1));
        plan.funding = T.Funding.V3Flash;
        lend(plan, loan);
        plan.maxLoanFee = 5;
        encodedPlan = abi.encode(plan);
    }

    function savedPlan() private view returns (T.Plan memory) {
        return abi.decode(encodedPlan, (T.Plan));
    }

    function testFlashRepaysActualFeeAndPreservesDust() public {
        token0.mint(address(executor), 123);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(savedPlan());
        assertEq(profit, 195);
        assertEq(fee, 5);
        assertEq(token0.balanceOf(address(lender)), 10_000_005);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
        assertKept(token0, 123 + 195);
        // A second invocation cannot reuse callback state from the first loan.
        vm.prank(OPERATOR);
        executor.execute(savedPlan());
        assertKept(token0, 123 + 2 * 195);
    }

    function testPancakeFlashAndToken1Borrow() public {
        TestV3Pool pancake = v3Pool(1_000_000, 1);
        T.Plan memory plan = savedPlan();
        plan.asset = address(token1);
        lend(plan, register(T.V3, address(pancake)));
        plan.steps[0].tokenOut = address(token0);
        plan.steps[1].tokenOut = address(token1);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 195);
        assertEq(token1.balanceOf(address(pancake)), 10_000_005);
    }

    function testLoanFeeCapAndMinProfitRollback() public {
        T.Plan memory plan = savedPlan();
        plan.maxLoanFee = 4;
        vm.expectRevert(ArbitrageExecutor.InvalidLoan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan.maxLoanFee = 5;
        plan.minProfit = 196;
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(address(lender)), 10_000_000);
        assertEq(token0.balanceOf(address(this)), 0);
    }

    function testMalformedLoanCallbacksAndMissingPrincipalRollback() public {
        uint8[6] memory modes = [uint8(2), 3, 4, 5, 8, 9];
        for (uint256 i; i < modes.length; ++i) {
            lender.setMode(modes[i]);
            vm.expectRevert();
            vm.prank(OPERATOR);
            executor.execute(savedPlan());
            assertEq(token0.balanceOf(address(lender)), 10_000_000);
            assertEq(token0.balanceOf(OPERATOR), 100_000);
        }
    }

    function testCannotSwapThroughLockedLender() public {
        T.Plan memory plan = savedPlan();
        plan.steps[0].target = plan.lender;
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testWalletCannotUseDustToPayProfit() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.Wallet;
        plan.lender = address(0);
        plan.lenderToken0 = false;
        plan.maxLoanFee = 0;
        plan.minProfit = 201;
        token0.mint(address(executor), 10_000);
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(address(executor)), 10_000);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }

    function testLenderDirectionAndPresenceAreChecked() public {
        T.Plan memory plan = savedPlan();
        // A lender asked for the wrong token sends token1; the first leg's
        // payment of token0 then fails.
        plan.lenderToken0 = false;
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan.lenderToken0 = true;
        plan.lender = address(0);
        vm.expectRevert(HotV3Executor.InvalidPlan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan.lender = address(executor);
        vm.expectRevert(HotV3Executor.InvalidPlan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(address(lender)), 10_000_000);
        assertEq(token1.balanceOf(address(lender)), 10_000_000);
    }

    function testWalletAndFlashSwapRejectLenderFields() public {
        T.Plan memory plan = savedPlan();
        plan.maxLoanFee = 0;
        for (uint8 funding; funding < 2; ++funding) {
            plan.funding = funding == 0 ? T.Funding.Wallet : T.Funding.V3FlashSwap;
            plan.lender = address(lender);
            plan.lenderToken0 = false;
            vm.expectRevert(HotV3Executor.InvalidPlan.selector);
            vm.prank(OPERATOR);
            executor.execute(plan);
            plan.lender = address(0);
            plan.lenderToken0 = true;
            vm.expectRevert(HotV3Executor.InvalidPlan.selector);
            vm.prank(OPERATOR);
            executor.execute(plan);
        }
    }

    function inventoryPlan() private view returns (T.Plan memory plan) {
        plan = savedPlan();
        plan.funding = T.Funding.Inventory;
        plan.lender = address(0);
        plan.lenderToken0 = false;
        plan.maxLoanFee = 0;
    }

    /// The principal comes from the executor's own balance: nothing is pulled
    /// from the operator and nothing is paid back to it.
    function testInventoryFundsThePrincipalFromTheExecutor() public {
        token0.mint(address(executor), 5_000);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(inventoryPlan());
        assertEq(profit, 200);
        assertEq(fee, 0);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
        assertKept(token0, 5_000 + 200);
        vm.prank(OPERATOR);
        (profit,) = executor.executePacked(PackedTestPlan.encode(inventoryPlan()));
        assertEq(profit, 200);
        assertKept(token0, 5_000 + 400);
    }

    function testInventoryKeepsItsBalanceWhenTheRouteFallsShort() public {
        T.Plan memory plan = inventoryPlan();
        // Less than the principal held: the first swap cannot be paid.
        token0.mint(address(executor), 999);
        vm.expectRevert();
        vm.prank(OPERATOR);
        executor.execute(plan);
        token0.mint(address(executor), 4_001);
        plan.minProfit = 201;
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan.minProfit = 100;
        plan.maxLoanFee = 1;
        vm.expectRevert(HotV3Executor.InvalidPlan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertKept(token0, 5_000);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }
}
