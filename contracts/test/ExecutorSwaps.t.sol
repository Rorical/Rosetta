// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {TestV3Pool, TestV2Pool, TestMetricPool} from "./PoolMocks.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {TestV4Manager} from "./V4Mock.sol";

contract ExecutorSwapsTest is ExecutorFixture {
    function testEveryConcentratedAdapterAndDirection() public {
        for (uint8 kind = T.V3; kind <= T.SLIPSTREAM; ++kind) {
            if (kind == T.V4) continue;
            for (uint8 reverse; reverse < 2; ++reverse) {
                uint8 callbackKind = kind == T.CAMELOT_V3 ? 2 : 0;
                runConcentrated(kind, callbackKind, reverse != 0);
            }
        }
        runConcentrated(T.V3, 1, false);
        runConcentrated(T.V3, 1, true);
        runConcentrated(T.SOLIDLY_V3, 3, false);
        runConcentrated(T.SOLIDLY_V3, 3, true);
    }

    // A Solidly pool calls solidlyV3SwapCallback; any other kind label on the
    // same pool, or a direct call from an unrelated contract, is rejected.
    function testSolidlyCallbackRequiresSolidlyKindAndActiveTarget() public {
        TestV3Pool first = v3Pool(2_000_000, 3);
        TestV3Pool second = v3Pool(600_000, 3);
        T.Plan memory plan = walletPlan(
            register(T.V3, address(first)), register(T.V3, address(second)), address(token0), address(token1)
        );
        vm.prank(OPERATOR);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        executor.execute(plan);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        executor.solidlyV3SwapCallback(1, -1, "");
    }

    // MetricOmmPool: its own swap signature and
    // callback. Selling token0 the price limit is a floor, buying it a cap, so
    // the executor sends zero and the maximum; minAmountOut bounds the leg.
    function testMetricOmmLegInBothDirections() public {
        for (uint8 reverse; reverse < 2; ++reverse) {
            TestMetricPool first = metricPool(2_000_000);
            TestV3Pool second = v3Pool(600_000, 0);
            address asset = reverse != 0 ? address(token1) : address(token0);
            address middle = reverse != 0 ? address(token0) : address(token1);
            T.Plan memory plan =
                walletPlan(register(T.METRIC_OMM, address(first)), register(T.V3, address(second)), asset, middle);
            vm.prank(OPERATOR);
            (uint256 profit,) = executor.execute(plan);
            assertEq(profit, 200);
            assertEq(first.lastLimit(), reverse != 0 ? type(uint128).max : 0);
        }
    }

    function testMetricOmmRejectsPartialFillsForeignCallbacksAndPriceLimits() public {
        TestMetricPool first = metricPool(2_000_000);
        TestV3Pool second = v3Pool(600_000, 0);
        T.Plan memory plan = walletPlan(
            register(T.METRIC_OMM, address(first)), register(T.V3, address(second)), address(token0), address(token1)
        );
        first.setMode(1);
        vm.prank(OPERATOR);
        vm.expectRevert(SwapExecutor.InvalidSwap.selector);
        executor.execute(plan);
        first.setMode(3);
        vm.prank(OPERATOR);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        executor.execute(plan);
        first.setMode(0);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        executor.metricOmmSwapCallback(1, -1, "");
        // A V3 pool labelled as a Metric pool has no Metric swap entry point.
        plan.steps[0].kind = T.V3;
        vm.prank(OPERATOR);
        vm.expectRevert();
        executor.execute(plan);
        plan.steps[0].kind = T.METRIC_OMM;
        plan.steps[0].sqrtPriceLimitX96 = 1;
        vm.prank(OPERATOR);
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        executor.execute(plan);
    }

    function metricPool(uint256 rate) private returns (TestMetricPool pool) {
        pool = new TestMetricPool(address(token0), address(token1), rate);
        token0.mint(address(pool), 10_000_000);
        token1.mint(address(pool), 10_000_000);
    }

    function runConcentrated(uint8 kind, uint8 callbackKind, bool reverse) private {
        TestV3Pool first = v3Pool(2_000_000, callbackKind);
        TestV3Pool second = v3Pool(600_000, callbackKind);
        bytes32 key0 = register(kind, address(first));
        bytes32 key1 = register(kind, address(second));
        address asset = reverse ? address(token1) : address(token0);
        address middle = reverse ? address(token0) : address(token1);
        T.Plan memory plan = walletPlan(key0, key1, asset, middle);
        uint256 operatorBefore = reverse ? token1.balanceOf(OPERATOR) : token0.balanceOf(OPERATOR);
        uint256 kept0 = token0.balanceOf(address(executor));
        uint256 kept1 = token1.balanceOf(address(executor));
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(profit, 200);
        assertEq(fee, 0);
        assertEq(reverse ? token1.balanceOf(OPERATOR) : token0.balanceOf(OPERATOR), operatorBefore);
        // Only the asset's profit stays behind; the middle token passes through.
        assertKept(token0, kept0 + (reverse ? 0 : 200));
        assertKept(token1, kept1 + (reverse ? 200 : 0));
    }

    function testV2ActualReserveInvariantAndOutputs() public {
        TestV2Pool first = new TestV2Pool(address(token0), address(token1));
        TestV2Pool second = new TestV2Pool(address(token0), address(token1));
        token0.mint(address(first), 1_000_000);
        token1.mint(address(first), 2_000_000);
        token0.mint(address(second), 1_000_000);
        token1.mint(address(second), 1_000_000);
        first.sync();
        second.sync();
        bytes32 key0 = register(T.V2, address(first));
        bytes32 key1 = register(T.V2, address(second));
        T.Plan memory plan = walletPlan(key0, key1, address(token0), address(token1));
        uint256 out0 = uint256(1000 * 997) * 2_000_000 / (1_000_000 * 1000 + 1000 * 997);
        uint256 out1 = out0 * 997 * 1_000_000 / (1_000_000 * 1000 + out0 * 997);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, out1 - 1000);
        (uint112 r0, uint112 r1,) = first.getReserves();
        assertEq(r0, 1_001_000);
        assertEq(r1, 2_000_000 - out0);
    }

    function testMixedRoutePassesActualOutputBetweenProtocols() public {
        TestV2Pool first = new TestV2Pool(address(token0), address(token1));
        token0.mint(address(first), 1_000_000);
        token1.mint(address(first), 2_000_000);
        first.sync();
        bytes32 key0 = register(T.V2, address(first));
        bytes32 key1 = register(T.V3, address(v3Pool(800_000, 1)));
        bytes32 key2 = register(T.CAMELOT_V3, address(v3Pool(2_000_000, 2)));
        TestV4Manager manager = new TestV4Manager(400_000);
        token0.mint(address(manager), 1_000_000);
        bytes32 key3 = registerPool(T.V4, address(manager), address(token0), address(token1), 3000, 60);
        T.Plan memory plan = walletPlan(key0, key1, address(token0), address(token1));
        plan.steps = new T.Step[](4);
        plan.steps[0] = step(key0, address(token0), 1, 0);
        plan.steps[1] = step(key1, address(token1), 1, 0);
        plan.steps[2] = step(key2, address(token0), 1, 0);
        plan.steps[3] = step(key3, address(token1), 1, 0);
        uint256 firstOut = uint256(1000 * 997) * 2_000_000 / (1_000_000 * 1000 + 1000 * 997);
        uint256 expected = ((firstOut * 8 / 10) * 2) * 4 / 10;
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, expected - 1000);
        assertKept(token0, profit);
        assertEq(token1.balanceOf(address(executor)), 0);
    }

    function testV3MalformedCallbacksRollbackAllBalances() public {
        TestV3Pool first = v3Pool(2_000_000, 0);
        TestV3Pool second = v3Pool(600_000, 0);
        bytes32 key0 = register(T.V3, address(first));
        bytes32 key1 = register(T.V3, address(second));
        T.Plan memory plan = walletPlan(key0, key1, address(token0), address(token1));
        for (uint8 mode = 1; mode <= 8; ++mode) {
            first.setMode(mode);
            vm.expectRevert();
            vm.prank(OPERATOR);
            executor.execute(plan);
            assertEq(token0.balanceOf(OPERATOR), 100_000);
            assertEq(token0.balanceOf(address(first)), 10_000_000);
            assertEq(token1.balanceOf(address(first)), 10_000_000);
            assertEq(token0.balanceOf(address(executor)), 0);
        }
        first.setMode(0);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testForeignAndWrongProtocolCallbacksFail() public {
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        executor.uniswapV3SwapCallback(1000, -2000, "");
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        executor.unlockCallback("");
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        executor.uniswapV3FlashCallback(0, 0, "");
        TestV3Pool wrongCallback = v3Pool(2_000_000, 2);
        TestV3Pool second = v3Pool(600_000, 0);
        bytes32 key0 = register(T.V3, address(wrongCallback));
        bytes32 key1 = register(T.V3, address(second));
        T.Plan memory plan = walletPlan(key0, key1, address(token0), address(token1));
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testSlippageAndDisconnectedRouteRollback() public {
        TestV3Pool first = v3Pool(2_000_000, 0);
        TestV3Pool second = v3Pool(600_000, 0);
        bytes32 key0 = register(T.V3, address(first));
        bytes32 key1 = register(T.V3, address(second));
        T.Plan memory plan = walletPlan(key0, key1, address(token0), address(token1));
        plan.steps[1].minAmountOut = 1201;
        vm.expectRevert(SwapExecutor.Slippage.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
        plan.steps[1].minAmountOut = 1100;
        plan.steps[1].tokenOut = address(token1);
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(token1.balanceOf(address(first)), 10_000_000);
    }
}
