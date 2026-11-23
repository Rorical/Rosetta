// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {HotTestRoute} from "./HotTestRoute.sol";
import {ExecutorAdmin} from "../src/ExecutorAdmin.sol";
import {HotV3Executor} from "../src/HotV3Executor.sol";
import {PriceCheck} from "../src/PriceCheck.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {TestV3Pool, TestV2Pool, TestMetricPool} from "./PoolMocks.sol";
import {PricedV3Pool, ShortSlot0Pool} from "./PriceMocks.sol";

contract ExecutorHotV3Test is ExecutorFixture {
    PricedV3Pool private first;
    PricedV3Pool private second;

    function setUp() public override {
        super.setUp();
        first = pricedPool(2_000_000);
        second = pricedPool(600_000);
    }

    function pricedPool(uint256 rate) private returns (PricedV3Pool pool) {
        pool = new PricedV3Pool(address(token0), address(token1), rate, 0);
        token0.mint(address(pool), 10_000_000);
        token1.mint(address(pool), 10_000_000);
    }

    function header(address asset, uint256 amount, uint256 minProfit, uint256 steps)
        private
        pure
        returns (bytes memory)
    {
        return abi.encodePacked(asset, uint128(amount), uint128(minProfit), uint8(steps));
    }

    uint8 private constant POOL = 0;
    uint8 private constant PAIR = 1;
    uint8 private constant METRIC = 2;

    /// @dev zeroForOne is the fixture's token0 -> token1.
    function leg(address pool, bool zeroForOne, uint8 kind, uint256 amount) private pure returns (bytes memory) {
        return abi.encodePacked(uint8((zeroForOne ? 1 : 0) | kind << 1), uint88(amount), pool);
    }

    /// @dev 1000 token0 -> 2000 token1 on a, -> 1200 token0 on b: profit 200.
    /// b is swapped first for its 2000 in and a is asked for exactly those.
    function route(address a, address b, uint256 minProfit) private view returns (bytes memory) {
        return
            bytes.concat(header(address(token0), 1000, minProfit, 2), leg(a, true, POOL, 0), leg(b, false, POOL, 2000));
    }

    function route(uint256 minProfit) private view returns (bytes memory) {
        return route(address(first), address(second), minProfit);
    }

    function run(bytes memory encoded) private returns (uint256 profit) {
        vm.prank(OPERATOR);
        return executor.executeHot(encoded);
    }

    function assertPoolsUntouched() private view {
        assertEq(token0.balanceOf(address(first)), 10_000_000);
        assertEq(token1.balanceOf(address(first)), 10_000_000);
        assertEq(token0.balanceOf(address(second)), 10_000_000);
        assertEq(token1.balanceOf(address(second)), 10_000_000);
    }

    function pair() private returns (TestV2Pool pool) {
        pool = new TestV2Pool(address(token0), address(token1));
        token0.mint(address(pool), 1_000_000);
        token1.mint(address(pool), 1_000_000);
        pool.sync();
    }

    /// @dev The intermediate token goes from the first pool straight to the
    /// second and never reaches the executor; the executor sends the asset once.
    function testEachPoolPaysTheNextAndProfitStaysInTheExecutor() public {
        token0.mint(address(executor), 123);
        token1.mint(address(executor), 456);
        vm.expectCall(address(token1), abi.encodeCall(token1.transfer, (address(second), 2000)), 1);
        vm.expectCall(address(token1), abi.encodeCall(token1.transfer, (address(executor), 2000)), 0);
        vm.expectCall(address(token0), abi.encodeCall(token0.transfer, (address(first), 1000)), 1);
        assertEq(run(route(100)), 200);
        assertKept(token0, 323);
        assertEq(token1.balanceOf(address(executor)), 456);
        assertEq(token0.balanceOf(address(first)), 10_001_000);
        assertEq(token1.balanceOf(address(first)), 9_998_000);
        assertEq(token1.balanceOf(address(second)), 10_002_000);
        assertEq(token0.balanceOf(address(second)), 9_998_800);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }

    function testUniswapPancakeAndAlgebraCallbacks() public {
        for (uint8 kind; kind < 3; ++kind) {
            TestV3Pool a = v3Pool(2_000_000, kind);
            TestV3Pool b = v3Pool(600_000, (kind + 1) % 3);
            assertEq(run(route(address(a), address(b), 100)), 200);
        }
        bytes memory solidly = route(address(v3Pool(2_000_000, 3)), address(second), 100);
        vm.expectRevert(SwapExecutor.InvalidCallback.selector);
        run(solidly);
    }

    function testReverseDirection() public {
        bytes memory encoded = bytes.concat(
            header(address(token1), 1000, 100, 2),
            leg(address(first), false, POOL, 0),
            leg(address(second), true, POOL, 2000)
        );
        assertEq(run(encoded), 200);
        assertKept(token1, 200);
        assertEq(token0.balanceOf(address(executor)), 0);
    }

    /// @dev 1000 -> 2000 -> 1200 -> 2400 -> 1440: three pools are each asked
    /// for exactly what the next is owed.
    function testFourPoolsChainBackToTheFirst() public {
        TestV3Pool third = v3Pool(2_000_000, 0);
        TestV3Pool fourth = v3Pool(600_000, 1);
        bytes memory encoded = bytes.concat(
            header(address(token0), 1000, 100, 4),
            leg(address(first), true, POOL, 0),
            leg(address(second), false, POOL, 0),
            leg(address(third), true, POOL, 0),
            leg(address(fourth), false, POOL, 2400)
        );
        assertEq(run(encoded), 440);
        assertEq(token1.balanceOf(address(executor)), 0);
        assertEq(token0.balanceOf(address(second)), 9_998_800);
        assertEq(token0.balanceOf(address(third)), 10_001_200);
    }

    /// @dev A pair after the outermost pool: the pool pays the pair and the
    /// pair pays the executor. 2000 token1 buy 1990 token0 from the pair.
    function testPairAfterTheOutermostPool() public {
        TestV2Pool last = pair();
        bytes memory encoded = bytes.concat(
            header(address(token0), 1000, 100, 2),
            leg(address(first), true, POOL, 1000),
            leg(address(last), false, PAIR, 1990)
        );
        vm.expectCall(address(token1), abi.encodeCall(token1.transfer, (address(last), 2000)), 2);
        assertEq(run(encoded), 990);
        assertEq(token1.balanceOf(address(executor)), 0);
        // One wei more than the pair's curve gives fails in the pair.
        encoded = bytes.concat(
            header(address(token0), 1000, 100, 2),
            leg(address(first), true, POOL, 1000),
            leg(address(last), false, PAIR, 1991)
        );
        vm.expectRevert();
        run(encoded);
    }

    /// @dev A pair before the outermost pool is supplied its input by the
    /// executor and asked for exactly what the pool is owed: 1000 token0 buy
    /// 996 token1, which buy 1992 token0.
    function testPairBeforeTheOutermostPool() public {
        TestV2Pool start = pair();
        TestV3Pool rich = v3Pool(2_000_000, 0);
        bytes memory encoded = bytes.concat(
            header(address(token0), 1000, 100, 2),
            leg(address(start), true, PAIR, 1000),
            leg(address(rich), false, POOL, 996)
        );
        vm.expectCall(address(token1), abi.encodeCall(token1.transfer, (address(rich), 996)), 1);
        assertEq(run(encoded), 992);
        assertEq(token1.balanceOf(address(executor)), 0);
    }

    /// @dev A pair between pools: the pool before it is asked for the pair's
    /// input, sent to the pair, and the pair pays the pool after it.
    /// 1000 -> 2000 (first) -> 1990 (pair) -> 3980 (third) -> 2388 (second).
    function testPairBetweenPools() public {
        TestV2Pool middle = pair();
        TestV3Pool third = v3Pool(2_000_000, 0);
        bytes memory encoded = bytes.concat(
            header(address(token0), 1000, 100, 4),
            leg(address(first), true, POOL, 0),
            leg(address(middle), false, PAIR, 2000),
            leg(address(third), true, POOL, 0),
            leg(address(second), false, POOL, 3980)
        );
        assertEq(run(encoded), 1388);
        assertEq(token0.balanceOf(address(middle)), 1_000_000 - 1990);
        assertEq(token1.balanceOf(address(middle)), 1_000_000 + 2000);
    }

    function testMetricPoolIsTheOutermost() public {
        TestMetricPool metric = new TestMetricPool(address(token0), address(token1), 600_000);
        token0.mint(address(metric), 10_000_000);
        bytes memory encoded = bytes.concat(
            header(address(token0), 1000, 100, 2),
            leg(address(first), true, POOL, 0),
            leg(address(metric), false, METRIC, 2000)
        );
        assertEq(run(encoded), 200);
        assertEq(metric.lastLimit(), type(uint128).max);
        assertEq(token1.balanceOf(address(metric)), 2000);
    }

    /// @dev The senders are fixed at deployment: the operators mapping neither
    /// admits nor removes one, and the pause is the conditional transaction's
    /// slot-0 condition.
    function testOnlyTheDeployedSendersAndPauseIsLeftToTheConditions() public {
        bytes memory encoded = route(100);
        executor.setOperator(address(0xBEEF), true);
        vm.expectRevert(ExecutorAdmin.Unauthorized.selector);
        vm.prank(address(0xBEEF));
        executor.executeHot(encoded);
        executor.setPaused(true);
        executor.setOperator(OPERATOR, false);
        assertEq(run(encoded), 200);
    }

    /// @dev The bot's conditions hold slot 0 at paused = false (byte 0) and
    /// configVersion (bytes 1-8): exec.HotConditions builds that word.
    function testSlotZeroHoldsPauseAndConfigVersion() public {
        uint256 version = executor.configVersion();
        assertEq(uint256(vm.load(address(executor), bytes32(0))), version << 8);
        executor.setPaused(true);
        assertEq(uint256(vm.load(address(executor), bytes32(0))), (version + 1) << 8 | 1);
        executor.setOperator(address(0xBEEF), true);
        assertEq(uint256(vm.load(address(executor), bytes32(0))), (version + 2) << 8 | 1);
    }

    /// @dev Only a claiming route raises claimEpoch, in storage slot 2, which
    /// the bot's variant conditions hold.
    function testClaimRaisesTheEpochOnSuccessOnly() public {
        assertEq(executor.claimEpoch(), 1);
        assertEq(uint256(vm.load(address(executor), bytes32(uint256(2)))), 1);
        assertEq(run(route(100)), 200);
        assertEq(executor.claimEpoch(), 1);
        assertEq(run(HotTestRoute.claim(route(100))), 200);
        assertEq(uint256(vm.load(address(executor), bytes32(uint256(2)))), 2);
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        run(HotTestRoute.claim(route(201)));
        assertEq(executor.claimEpoch(), 2);
    }

    function testMalformedRoutesRejectBeforeAnySwap() public {
        vm.expectCall(address(first), abi.encodeWithSelector(first.swap.selector), 0);
        vm.expectCall(address(second), abi.encodeWithSelector(second.swap.selector), 0);
        bytes memory seventeen = header(address(token0), 1000, 100, 17);
        for (uint256 i; i < 17; ++i) {
            seventeen = bytes.concat(seventeen, leg(address(first), i % 2 == 0, POOL, 1));
        }
        bytes memory valid = route(100);
        TestV2Pool only = pair();
        bytes[8] memory invalid = [
            bytes.concat(header(address(token0), 1000, 100, 1), leg(address(first), true, POOL, 1000)),
            seventeen,
            bytes.concat(header(address(token0), 1000, 100, 3), leg(address(first), true, POOL, 0)),
            bytes.concat(valid, hex"00"),
            HotTestRoute.bound(valid, 2, false, 1),
            route(0),
            bytes.concat(
                header(address(token0), 0, 100, 2),
                leg(address(first), true, POOL, 0),
                leg(address(second), false, POOL, 2000)
            ),
            // Pairs alone: no pool delivers before it is paid.
            bytes.concat(
                header(address(token0), 1000, 100, 2),
                leg(address(only), true, PAIR, 1000),
                leg(address(only), false, PAIR, 1)
            )
        ];
        for (uint256 i; i < invalid.length; ++i) {
            vm.expectRevert(HotV3Executor.InvalidPlan.selector);
            run(invalid[i]);
        }
    }

    /// @dev zeroForOne requires price >= bound, the other direction price <=
    /// bound; a failing bound reverts before any balance read or swap.
    function testBoundsRejectPoolsThatMovedAgainstTheRoute() public {
        first.setPrice(1000);
        second.setPrice(1000);
        bytes memory bounded = HotTestRoute.bound(HotTestRoute.bound(route(100), 0, false, 1000), 1, false, 1000);
        assertEq(run(bounded), 200);
        vm.expectCall(address(first), abi.encodeWithSelector(first.swap.selector), 0);
        vm.expectCall(address(token0), abi.encodeWithSelector(token0.balanceOf.selector), 0);
        first.setPrice(999);
        vm.expectRevert(abi.encodeWithSelector(PriceCheck.PoolMoved.selector, 0));
        run(bounded);
        first.setPrice(1000);
        second.setPrice(1001);
        vm.expectRevert(abi.encodeWithSelector(PriceCheck.PoolMoved.selector, 1));
        run(bounded);
    }

    function testAlgebraBoundReadsGlobalState() public {
        first.setPrice(1000);
        vm.mockCall(address(first), abi.encodeWithSelector(0x3850c7bd), abi.encode(uint160(1)));
        vm.expectRevert(abi.encodeWithSelector(PriceCheck.PoolMoved.selector, 0));
        run(HotTestRoute.bound(route(100), 0, false, 1000));
        assertEq(run(HotTestRoute.bound(route(100), 0, true, 1000)), 200);
    }

    function testUnreadableBoundedPool() public {
        bytes memory encoded =
            HotTestRoute.bound(route(address(new ShortSlot0Pool()), address(second), 100), 0, false, 1);
        vm.expectRevert(abi.encodeWithSelector(PriceCheck.UnreadablePool.selector, 0));
        run(encoded);
    }

    /// @dev The first pool is asked for an exact output: a second callback, a
    /// callback from another contract, none at all, one wei more than the
    /// principal and a short delivery all roll back. Payloads and return
    /// values are not read: modes 4 and 7 move the same tokens and complete.
    function testMalformedFirstPoolRollsBack() public {
        bytes memory encoded = route(100);
        uint8[5] memory failing = [2, 3, 5, 6, 8];
        for (uint256 i; i < failing.length; ++i) {
            first.setMode(failing[i]);
            vm.expectRevert();
            run(encoded);
            assertPoolsUntouched();
            assertEq(token0.balanceOf(address(executor)), 0);
        }
        first.setMode(4);
        assertEq(run(encoded), 200);
        first.setMode(7);
        assertEq(run(encoded), 200);
    }

    /// @dev With the exact profit as the minimum, an outermost pool that fills
    /// half, asks a wei more or delivers a wei less fails.
    function testMalformedOutermostPoolRollsBack() public {
        bytes memory encoded = route(200);
        uint8[6] memory failing = [1, 2, 3, 5, 6, 8];
        for (uint256 i; i < failing.length; ++i) {
            second.setMode(failing[i]);
            vm.expectRevert();
            run(encoded);
            assertPoolsUntouched();
        }
        second.setMode(4);
        assertEq(run(encoded), 200);
    }

    /// @dev The route's amount is the quoted input, not a cap. A first pool
    /// that moved slightly can ask for a little more than the amount on a
    /// route that still clears its minimum; refusing it wastes the whole
    /// route's gas. A first pool that asks for more is paid while the minimum
    /// profit is left; the profit check refuses the rest and inventory is
    /// never spent.
    function testFirstPoolMayTakeMoreThanTheQuotedInputWhileTheProfitHolds() public {
        token0.mint(address(executor), 10_000);
        bytes memory encoded = bytes.concat(
            header(address(token0), 999, 100, 2),
            leg(address(first), true, POOL, 0),
            leg(address(second), false, POOL, 2000)
        );
        assertEq(run(encoded), 200);
        assertKept(token0, 10_200);

        encoded = bytes.concat(
            header(address(token0), 999, 201, 2),
            leg(address(first), true, POOL, 0),
            leg(address(second), false, POOL, 2000)
        );
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        run(encoded);
        assertKept(token0, 10_200);
    }

    /// @dev A token that delivers less than was sent underpays the next pool,
    /// which rejects it; the executor's own balance of that token is not used.
    function testTaxedIntermediateIsNotPaidFromInventory() public {
        token1.mint(address(executor), 1_000_000);
        token1.configure(10_000, false, false);
        vm.expectRevert();
        run(route(100));
        assertEq(token1.balanceOf(address(executor)), 1_000_000);
    }

    function testOldInventoryCannotSubsidizeProfit() public {
        token0.mint(address(executor), 10_000);
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        run(route(201));
        assertEq(token0.balanceOf(address(executor)), 10_000);
        assertPoolsUntouched();
    }

    function testRouteCannotReenter() public {
        bytes memory encoded = route(100);
        token1.reenter(address(executor), abi.encodeCall(executor.executeHot, (encoded)));
        assertEq(run(encoded), 200);
        require(token1.reentryBlocked(), "reentered");
    }

    /// @dev Every swap of a route is nested in the one after it, so a pool
    /// appears once: a V3 pool is locked while it swaps.
    function testSixteenSteps() public {
        uint256 amount = 1000;
        uint256 last;
        bytes memory encoded = header(address(token0), 1000, 100, 16);
        for (uint256 i; i < 16; ++i) {
            bool out = i % 2 == 0;
            last = amount;
            amount = out ? amount * 2 : amount * 6 / 10;
            encoded = bytes.concat(
                encoded, leg(address(v3Pool(out ? 2_000_000 : 600_000, 0)), out, POOL, i == 15 ? last : 0)
            );
        }
        // Each pool is asked for exactly what the next takes in, so the first
        // pool's input is what those exact outputs cost, rounded up per pool.
        uint256 owed = last;
        for (uint256 i = 15; i > 0; --i) {
            owed = (i - 1) % 2 == 0 ? (owed + 1) / 2 : (owed * 10 + 5) / 6;
        }
        assertEq(run(encoded), amount - owed);
    }
}
