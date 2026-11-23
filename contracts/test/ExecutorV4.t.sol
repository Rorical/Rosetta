// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {TestV4Manager} from "./V4Mock.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";

contract ExecutorV4Test is ExecutorFixture {
    TestV4Manager private first;
    TestV4Manager private second;

    function setUp() public override {
        super.setUp();
        first = new TestV4Manager(2_000_000);
        second = new TestV4Manager(600_000);
        token0.mint(address(first), 1_000_000);
        token1.mint(address(first), 1_000_000);
        token0.mint(address(second), 1_000_000);
        token1.mint(address(second), 1_000_000);
        vm.deal(address(first), 1_000_000);
        vm.deal(address(second), 1_000_000);
    }

    function registerV4(address manager, address a, address b) private returns (bytes32) {
        return registerPool(T.V4, manager, a, b, 3000, 60);
    }

    function planFor(bool native) private returns (T.Plan memory) {
        address a = native ? address(0) : address(token0);
        bytes32 key0 = registerV4(address(first), a, address(token1));
        bytes32 key1 = registerV4(address(second), a, address(token1));
        return walletPlan(key0, key1, a, address(token1));
    }

    function testV4ERC20SyncSettleAndTake() public {
        T.Plan memory plan = planFor(false);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 200);
        assertEq(token0.balanceOf(address(first)), 1_001_000);
        assertEq(token1.balanceOf(address(first)), 998_000);
        assertEq(token1.balanceOf(address(second)), 1_002_000);
        assertEq(token0.balanceOf(address(second)), 998_800);
    }

    function testV4NativeWalletReturnsPrincipalAndKeepsProfit() public {
        T.Plan memory plan = planFor(true);
        vm.deal(OPERATOR, 10_000);
        vm.deal(address(executor), 100);
        uint256 ownerBefore = address(this).balance;
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute{value: 1000}(plan);
        assertEq(profit, 200);
        assertEq(OPERATOR.balance, 10_000);
        assertEq(address(this).balance, ownerBefore);
        assertEq(address(executor).balance, 100 + 200);
    }

    function testV4ReverseDirection() public {
        T.Plan memory plan = planFor(false);
        plan.asset = address(token1);
        plan.steps[0].tokenOut = address(token0);
        plan.steps[1].tokenOut = address(token1);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 200);
    }

    function testV4SlippageRejectsBeforeSettlement() public {
        T.Plan memory plan = planFor(false);
        plan.steps[0].minAmountOut = 2001;
        vm.expectCall(address(first), abi.encodeWithSelector(first.sync.selector), 0);
        vm.expectRevert(SwapExecutor.Slippage.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }

    function testFlashRouteThroughNativeCurrency() public {
        T.Plan memory plan = planFor(true);
        bytes32 loan = register(T.V3, address(v3Pool(1_000_000, 0)));
        plan.funding = T.Funding.V3Flash;
        plan.maxLoanFee = 5;
        plan.asset = address(token1);
        lend(plan, loan);
        plan.steps[0].tokenOut = address(0);
        plan.steps[1].tokenOut = address(token1);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(profit, 195);
        assertEq(fee, 5);
        assertEq(address(executor).balance, 0);
    }

    function testV4CallbackPartialSettlementAndTakeFailuresRollback() public {
        T.Plan memory plan = planFor(false);
        for (uint8 mode = 1; mode <= 6; ++mode) {
            first.setMode(mode);
            vm.expectRevert();
            vm.prank(OPERATOR);
            executor.execute(plan);
            assertEq(token0.balanceOf(OPERATOR), 100_000);
            assertEq(token0.balanceOf(address(first)), 1_000_000);
            assertEq(token1.balanceOf(address(first)), 1_000_000);
        }
    }

    function testRejectUnsolicitedNativePayment() public {
        vm.deal(address(this), 1000);
        (bool ok,) = address(executor).call{value: 100}("");
        require(!ok, "accepted foreign native transfer");
    }
}
