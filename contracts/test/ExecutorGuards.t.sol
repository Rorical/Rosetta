// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {ExecutorAdmin} from "../src/ExecutorAdmin.sol";
import {ArbitrageExecutor} from "../src/ArbitrageExecutor.sol";
import {HotV3Executor} from "../src/HotV3Executor.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {TokenActions} from "../src/TokenActions.sol";
import {TestV3Pool} from "./PoolMocks.sol";

contract ExecutorGuardsTest is ExecutorFixture {
    bytes private encodedPlan;
    address private firstPool;

    function setUp() public override {
        super.setUp();
        TestV3Pool first = v3Pool(2_000_000, 0);
        TestV3Pool second = v3Pool(600_000, 0);
        firstPool = address(first);
        bytes32 key0 = register(T.V3, address(first));
        bytes32 key1 = register(T.V3, address(second));
        encodedPlan = abi.encode(walletPlan(key0, key1, address(token0), address(token1)));
    }

    function planAtCurrentVersion() private view returns (T.Plan memory plan) {
        plan = abi.decode(encodedPlan, (T.Plan));
    }

    function testExecutionGuards() public {
        T.Plan memory plan = planAtCurrentVersion();
        vm.expectRevert(ExecutorAdmin.Unauthorized.selector);
        vm.prank(address(0x5678));
        executor.execute(plan);
        executor.setPaused(true);
        vm.expectRevert(HotV3Executor.Unavailable.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        executor.setPaused(false);
        plan.deadline = block.timestamp - 1;
        vm.expectRevert(HotV3Executor.InvalidPlan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan = planAtCurrentVersion();
        plan.minProfit = 0;
        vm.expectRevert(HotV3Executor.InvalidPlan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        plan = planAtCurrentVersion();
        plan.steps = new T.Step[](0);
        vm.expectRevert(HotV3Executor.InvalidPlan.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testUnauthorizedAdminAndConfigVersion() public {
        vm.expectRevert(ExecutorAdmin.Unauthorized.selector);
        vm.prank(OPERATOR);
        executor.setPaused(true);
        vm.expectRevert(ExecutorAdmin.Unauthorized.selector);
        vm.prank(OPERATOR);
        executor.setOperator(OPERATOR, false);
        uint64 version = executor.configVersion();
        executor.setOperator(address(0x5678), true);
        require(executor.configVersion() == version + 1, "operator change kept version");
        executor.setPaused(true);
        require(executor.configVersion() == version + 2, "pause change kept version");
        vm.expectRevert(ExecutorAdmin.InvalidConfiguration.selector);
        executor.setOperator(address(0), true);
    }

    function expectInvalidRoute(T.Plan memory plan) private {
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testRouteParametersAreValidatedBeforeFunding() public {
        T.Plan memory plan = planAtCurrentVersion();
        plan.steps[1].kind = 0;
        expectInvalidRoute(plan);
        plan.steps[1].kind = T.METRIC_OMM + 1;
        expectInvalidRoute(plan);
        plan = planAtCurrentVersion();
        plan.steps[0].target = address(0);
        expectInvalidRoute(plan);
        plan.steps[0].target = address(executor);
        expectInvalidRoute(plan);
        plan = planAtCurrentVersion();
        plan.steps[0].tokenOut = address(token0);
        expectInvalidRoute(plan);
        plan = planAtCurrentVersion();
        plan.steps[1].tokenOut = address(token1);
        expectInvalidRoute(plan);
        // A route must close in its funding asset.
        plan = planAtCurrentVersion();
        plan.steps[1].tokenOut = address(0x9999);
        expectInvalidRoute(plan);
        // Native currency is only settled by the V4 manager.
        plan = planAtCurrentVersion();
        plan.steps[0].tokenOut = address(0);
        plan.steps[1].kind = T.V4;
        expectInvalidRoute(plan);
        for (uint8 kind = T.V2; kind <= T.VELODROME_V2; kind += T.VELODROME_V2 - T.V2) {
            plan = planAtCurrentVersion();
            plan.steps[1].kind = kind;
            plan.steps[1].sqrtPriceLimitX96 = 4295128740;
            expectInvalidRoute(plan);
        }
        assertEq(token0.balanceOf(OPERATOR), 100_000);
        assertEq(token0.balanceOf(firstPool), 10_000_000);
    }

    function testWrongChainAndPausedByDefault() public {
        ArbitrageExecutor fresh = new ArbitrageExecutor(address(this), [OPERATOR, address(0)]);
        require(fresh.paused(), "deployment not paused");
        vm.chainId(4663);
        ArbitrageExecutor other = new ArbitrageExecutor(address(this), [OPERATOR, address(0)]);
        require(other.chainId() == 4663 && fresh.chainId() == 42161, "deployment chain identity");
        T.Plan memory plan = planAtCurrentVersion();
        vm.expectRevert(HotV3Executor.Unavailable.selector);
        executor.execute(plan);
    }

    function testNoAllowanceTaxedAndFalseReturnTokensFailWithoutLoss() public {
        T.Plan memory plan = planAtCurrentVersion();
        vm.prank(OPERATOR);
        token0.approve(address(executor), 0);
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        vm.prank(OPERATOR);
        token0.approve(address(executor), type(uint256).max);
        token0.configure(10_000, false, false);
        vm.expectRevert(TokenActions.UnexpectedTransfer.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        token0.configure(0, true, false);
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
        assertEq(token0.balanceOf(address(executor)), 0);
        token0.configure(0, false, true);
        token1.configure(0, false, true);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertKept(token0, 200);
    }

    function testTaxedOutputFailsAndPreservesPoolReserves() public {
        T.Plan memory plan = planAtCurrentVersion();
        token1.configure(10_000, false, false);
        vm.expectRevert();
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token1.balanceOf(firstPool), 10_000_000);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }

    function testTokenReentrancyCannotExecuteOrChangeAdmin() public {
        T.Plan memory plan = planAtCurrentVersion();
        executor.setOperator(address(token0), true);
        token0.reenter(address(executor), abi.encodeCall(executor.execute, (plan)));
        vm.prank(OPERATOR);
        executor.execute(plan);
        require(token0.reentryBlocked(), "reentered execute");
        token0.reenter(address(executor), abi.encodeCall(executor.setPaused, (true)));
        vm.prank(OPERATOR);
        executor.execute(plan);
        require(token0.reentryBlocked() && !executor.paused(), "reentered admin");
    }

    function testFuzzWalletProfitAndDust(uint32 requested, uint64 dust) public {
        T.Plan memory plan = planAtCurrentVersion();
        uint256 amount = 1000 + uint256(requested) % 90_001;
        plan.amount = amount;
        plan.steps[0].minAmountOut = amount * 2;
        plan.steps[1].minAmountOut = amount * 12 / 10;
        plan.minProfit = 1;
        token0.mint(address(executor), dust);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, amount / 5);
        assertKept(token0, dust + profit);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }
}
