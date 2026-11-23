// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";
import {TestV2Pool} from "./PoolMocks.sol";
import {PricedV3Pool, PricedVelodromePool, PricedV4Manager, ShortSlot0Pool} from "./PriceMocks.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {PackedPlan} from "../src/PackedPlan.sol";
import {PriceCheck} from "../src/PriceCheck.sol";
import {ExecutorAdmin} from "../src/ExecutorAdmin.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {HotV3Executor} from "../src/HotV3Executor.sol";

contract CheckedPlanHarness {
    function decode(bytes calldata encoded) external pure returns (T.Plan memory plan, uint256[] memory bounds) {
        (plan, bounds,) = PackedPlan.decode(encoded);
    }

    function mulDiv(uint256 a, uint256 b, uint256 d) external pure returns (uint256) {
        return PriceCheck.mulDiv(a, b, d);
    }

    function decodeRate(bytes calldata encoded) external pure returns (uint256 minRate) {
        (,, minRate) = PackedPlan.decode(encoded);
    }

    function check(T.Plan memory plan, uint256[] memory bounds) external view {
        PriceCheck.check(plan, bounds);
    }
}

contract ExecutorPriceCheckTest is ExecutorFixture {
    uint160 private constant ONE = 2 ** 96;
    // First-step pools: four V3-family kinds, Velodrome V2 and a V4 pool.
    uint8[6] private kinds = [T.V3, T.SLIPSTREAM, T.SOLIDLY_V3, T.CAMELOT_V3, T.VELODROME_V2, T.V4];
    bytes32[6] private firsts;
    TestV2Pool private v2;
    bytes32 private v2Key;
    PricedVelodromePool private velodrome;
    PricedV4Manager private manager;
    bytes32 private v4Slot;
    CheckedPlanHarness private parser = new CheckedPlanHarness();

    event GasMeasured(string scenario, uint256 gasUsed);

    function setUp() public override {
        super.setUp();
        v2 = new TestV2Pool(address(token0), address(token1));
        token0.mint(address(v2), 1_000_000);
        token1.mint(address(v2), 1_000_000);
        v2.sync();
        v2Key = register(T.V2, address(v2));
        uint8[4] memory callbacks = [0, 0, 3, 2];
        for (uint256 j; j < 4; ++j) {
            uint256 i = 3 - j;
            PricedV3Pool pool = new PricedV3Pool(address(token0), address(token1), 2_000_000, callbacks[i]);
            token0.mint(address(pool), 10_000_000);
            token1.mint(address(pool), 10_000_000);
            pool.setPrice(ONE);
            firsts[i] = register(kinds[i], address(pool));
        }
        velodrome = new PricedVelodromePool(address(token0), address(token1));
        token0.mint(address(velodrome), 10_000_000);
        token1.mint(address(velodrome), 10_000_000);
        velodrome.setReserves(1_000_000, 1_000_000);
        firsts[4] = register(T.VELODROME_V2, address(velodrome));
        manager = new PricedV4Manager();
        token0.mint(address(manager), 10_000_000);
        token1.mint(address(manager), 10_000_000);
        bytes32 id = keccak256(abi.encode(T.PoolKey(address(token0), address(token1), 3000, 60, address(0))));
        manager.setRate(id, 2_000_000);
        v4Slot = keccak256(abi.encode(id, uint256(6)));
        setV4Price(ONE);
        firsts[5] = register(T.V4, address(manager));
    }

    function testUnchangedStatePassesForEveryKindAndDirection() public {
        for (uint256 i; i < kinds.length; ++i) {
            for (uint256 d; d < 2; ++d) {
                T.Plan memory plan = cycle(firsts[i], d == 0);
                (bool ok, bytes memory result,) = run(PackedTestPlan.encodeChecked(plan, currentBounds(plan)));
                require(ok, "unchanged state rejected");
                (uint256 profit,) = abi.decode(result, (uint256, uint256));
                require(profit >= plan.minProfit, "profit");
            }
        }
    }

    function testAdverseMoveRevertsBeforeTheFirstSwap() public {
        for (uint256 i; i < kinds.length; ++i) {
            for (uint256 d; d < 2; ++d) {
                bool zeroForOne = d == 0;
                T.Plan memory plan = cycle(firsts[i], zeroForOne);
                uint160[] memory bounds = currentBounds(plan);
                // One unit of the bound's own scale against the swap direction.
                movePrice(i, zeroForOne, false);
                (bool ok, bytes memory result, uint256 used) = run(PackedTestPlan.encodeChecked(plan, bounds));
                require(!ok && keccak256(result) == keccak256(moved(0)), "adverse move accepted");
                require(used < 30_000, "reverted after swapping");
                restorePrice(i);
            }
        }
    }

    function testFavourableMovePasses() public {
        for (uint256 i; i < kinds.length; ++i) {
            for (uint256 d; d < 2; ++d) {
                bool zeroForOne = d == 0;
                T.Plan memory plan = cycle(firsts[i], zeroForOne);
                uint160[] memory bounds = currentBounds(plan);
                movePrice(i, zeroForOne, true);
                (bool ok,,) = run(PackedTestPlan.encodeChecked(plan, bounds));
                require(ok, "favourable move rejected");
                restorePrice(i);
            }
        }
    }

    function testV2ReserveRatioIsDirectionAware() public {
        T.Plan memory plan = cycle(firsts[0], true); // the V2 leg sells token1 for token0
        uint160[] memory bounds = currentBounds(plan);
        uint256 snapshot = vm.snapshotState();
        // Liquidity added in proportion leaves the ratio, and the bound, intact.
        token0.mint(address(v2), 1_000_000);
        token1.mint(address(v2), 1_000_000);
        v2.sync();
        (bool ok,,) = run(PackedTestPlan.encodeChecked(plan, bounds));
        require(ok, "proportional liquidity rejected");
        vm.revertToState(snapshot);
        // A rival selling token1 first raises our input reserve.
        token1.mint(address(v2), 1_000);
        v2.sync();
        bytes memory result;
        (ok, result,) = run(PackedTestPlan.encodeChecked(plan, bounds));
        require(!ok && keccak256(result) == keccak256(moved(1)), "rival sale accepted");
        // A trade the other way restores more output than we quoted.
        token0.mint(address(v2), 2_000);
        v2.sync();
        (ok,,) = run(PackedTestPlan.encodeChecked(plan, bounds));
        require(ok, "favourable reserves rejected");
    }

    function testUnreadablePoolRejected() public {
        address[2] memory targets = [address(v3Pool(2_000_000, 0)), address(new ShortSlot0Pool())];
        for (uint256 i; i < 2; ++i) {
            T.Plan memory plan = cycle(register(T.V3, targets[i]), true);
            uint160[] memory bounds = new uint160[](2);
            bounds[0] = ONE;
            (bool ok, bytes memory result,) = run(PackedTestPlan.encodeChecked(plan, bounds));
            require(
                !ok && keccak256(result) == keccak256(abi.encodeWithSelector(PriceCheck.UnreadablePool.selector, 0)),
                "unreadable pool accepted"
            );
        }
    }

    function testMalformedPriceBoundsRejected() public {
        T.Plan memory plan = cycle(firsts[0], true);
        plan.steps[1].sqrtPriceLimitX96 = 0;
        plan.steps[0].sqrtPriceLimitX96 = 4295128740;
        bytes memory encoded = PackedTestPlan.encodeChecked(plan, currentBounds(plan));
        for (uint256 length; length < encoded.length; ++length) {
            bytes memory shortened = new bytes(length);
            for (uint256 i; i < length; ++i) {
                shortened[i] = encoded[i];
            }
            vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
            parser.decode(shortened);
        }
        vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
        parser.decode(bytes.concat(encoded, hex"00"));
        // Step 0 is 80 bytes plus a limit and a bound; step 1 flags follow.
        uint256 firstFlags = 95 + 79;
        uint256 secondFlags = 95 + 120 + 79;
        bytes[5] memory changed;
        for (uint256 i; i < changed.length; ++i) {
            changed[i] = bytes.concat(encoded);
        }
        changed[0][firstFlags] = 0x04; // unknown flag
        changed[1][firstFlags] = 0x07;
        for (uint256 i = 95 + 100; i < 95 + 120; ++i) {
            changed[2][i] = 0; // zero bound
        }
        changed[3][0] = 0x04; // unknown version
        changed[4][secondFlags] = 0x06;
        for (uint256 i; i < changed.length; ++i) {
            vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
            parser.decode(changed[i]);
        }
        // Version 2 has no bound flag.
        bytes memory old = PackedTestPlan.encode(plan);
        old[firstFlags] = 0x03;
        vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
        parser.decode(bytes.concat(old, bytes20(uint160(1))));
    }

    // Velodrome reserves are full words: the comparison must not overflow.
    function testFuzzPairBoundMatchesFloorRatio(uint160 reserveOut, uint256 reserveIn, uint160 bound) public {
        if (reserveIn == 0) reserveIn = 1;
        if (bound == 0) bound = 1;
        velodrome.setReserves(reserveIn, reserveOut);
        T.Plan memory plan = cycle(firsts[4], true);
        uint256[] memory bounds = new uint256[](2);
        bounds[0] = bound;
        bool expected = (uint256(reserveOut) << 96) / reserveIn >= bound;
        try parser.check(plan, bounds) {
            require(expected, "moved pool accepted");
        } catch (bytes memory reason) {
            require(!expected && keccak256(reason) == keccak256(moved(0)), "unmoved pool rejected");
        }
    }

    function testVersionTwoStillDecodesAndExecutes() public {
        T.Plan memory plan = cycle(firsts[0], true);
        (T.Plan memory oldPlan, uint256[] memory none) = parser.decode(PackedTestPlan.encode(plan));
        (T.Plan memory newPlan, uint256[] memory zeros) =
            parser.decode(PackedTestPlan.encodeChecked(plan, new uint160[](2)));
        require(keccak256(abi.encode(oldPlan)) == keccak256(abi.encode(plan)), "v2 round trip");
        require(keccak256(abi.encode(newPlan)) == keccak256(abi.encode(plan)), "v3 round trip");
        require(none.length == 0 && zeros.length == 2 && zeros[0] == 0 && zeros[1] == 0, "bounds");
        uint160[] memory bounds = currentBounds(plan);
        (, uint256[] memory decoded) = parser.decode(PackedTestPlan.encodeChecked(plan, bounds));
        require(decoded[0] == bounds[0] && decoded[1] == bounds[1], "bound values");
        // Version 2 ignores pool state, as the deployed executor does.
        movePrice(0, true, false);
        (bool ok,,) = run(PackedTestPlan.encode(plan));
        require(ok, "v2 execution");
    }

    function testAccessCheckedBeforeAnyPoolRead() public {
        T.Plan memory plan = cycle(firsts[0], true);
        bytes memory encoded = PackedTestPlan.encodeChecked(plan, currentBounds(plan));
        vm.expectCall(plan.steps[0].target, abi.encodeWithSignature("slot0()"), 0);
        vm.prank(address(0x5678));
        vm.expectRevert(ExecutorAdmin.Unauthorized.selector);
        executor.executePacked(encoded);
    }

    function testGuardsRunAroundThePrecheck() public {
        T.Plan memory plan = cycle(firsts[0], true);
        uint160[] memory bounds = currentBounds(plan);
        // An expired plan fails on its deadline even after an adverse move.
        movePrice(0, true, false);
        plan.deadline = block.timestamp - 1;
        (bool ok, bytes memory result,) = run(PackedTestPlan.encodeChecked(plan, bounds));
        require(!ok && bytes4(result) == HotV3Executor.InvalidPlan.selector, "deadline order");
        restorePrice(0);
        // A passing precheck does not replace the closing profit check.
        plan = cycle(firsts[0], true);
        plan.minProfit = 1_000;
        (ok, result,) = run(PackedTestPlan.encodeChecked(plan, bounds));
        require(!ok && bytes4(result) == SwapExecutor.InsufficientProfit.selector, "profit guard");
        // Funding modes run unchanged after the precheck.
        plan = cycle(firsts[0], true);
        plan.funding = T.Funding.V3FlashSwap;
        (ok,,) = run(PackedTestPlan.encodeChecked(plan, bounds));
        require(ok, "flash swap with precheck");
    }

    // Synthetic two-leg route, one cold call per test. In the lost auction a
    // rival sold into the last (V2) pool: without bounds both swaps run and the
    // route fails on profit; with bounds it fails on reading the second pool.
    // Bounds are the setUp prices, so computing them touches no pool here.
    function testGasPackedSuccess() public {
        measure("packed_success", PackedTestPlan.encode(lossPlan()), true);
    }

    function testGasPrecheckSuccess() public {
        T.Plan memory plan = lossPlan();
        measure("precheck_success", PackedTestPlan.encodeChecked(plan, setUpBounds()), true);
    }

    function testGasPackedLostAuction() public {
        T.Plan memory plan = lossPlan();
        rivalSale();
        measure("packed_lost_auction", PackedTestPlan.encode(plan), false);
    }

    function testGasPrecheckLostAuction() public {
        T.Plan memory plan = lossPlan();
        rivalSale();
        measure("precheck_lost_auction", PackedTestPlan.encodeChecked(plan, setUpBounds()), false);
    }

    function testGasPrecheckFirstPoolMoved() public {
        T.Plan memory plan = lossPlan();
        movePrice(0, true, false);
        measure("precheck_first_pool_moved", PackedTestPlan.encodeChecked(plan, setUpBounds()), false);
    }

    function lossPlan() private view returns (T.Plan memory plan) {
        plan = cycle(firsts[0], true);
        plan.steps[1].minAmountOut = 1; // leave the failure to the profit check
    }

    function setUpBounds() private pure returns (uint160[] memory bounds) {
        bounds = new uint160[](2);
        (bounds[0], bounds[1]) = (ONE, ONE);
    }

    function rivalSale() private {
        token1.mint(address(v2), 1_000_000);
        v2.sync();
    }

    function measure(string memory scenario, bytes memory encoded, bool success) private {
        // The rival or a price change touched some of these earlier in this transaction.
        address[5] memory touched =
            [address(executor), address(token0), address(token1), address(v2), testPools[firsts[0]].target];
        for (uint256 i; i < touched.length; ++i) {
            vm.cool(touched[i]);
        }
        (bool ok,, uint256 used) = run(encoded);
        emit GasMeasured(scenario, used);
        require(ok == success, "unexpected outcome");
    }

    // Wallet route: the first pool from asset to the other token at rate 2,
    // then the V2 pool back. minProfit leaves room for 990 units of profit.
    // Ten basis points of slack: the cumulative check's minimum rate product.
    uint64 private constant MIN_RATE = 0.999e18;

    function testCumulativeUnchangedStatePassesForEveryKindAndDirection() public {
        for (uint256 i; i < kinds.length; ++i) {
            for (uint256 d; d < 2; ++d) {
                T.Plan memory plan = cycle(firsts[i], d == 0);
                (bool ok,,) = run(PackedTestPlan.encodeCumulative(plan, currentBounds(plan), MIN_RATE));
                require(ok, "unchanged state rejected");
            }
        }
    }

    // When several pools each move less than the route's slack, every
    // per-step bound passes and the transaction fails at the closing profit
    // check after most of its gas is spent. Two legs here each lose 6 bp of a
    // 10 bp slack: version 3 lets them through, version 4 stops before swapping.
    function testCumulativeRejectsDriftSpreadAcrossPools() public {
        T.Plan memory plan = twoPriced(true);
        uint160[] memory references = currentBounds(plan);
        uint160[] memory bounds = boundsAt(plan, references, 999499875); // sqrt(0.999) in 1e9
        shiftRate(plan, 0, 9994);
        shiftRate(plan, 1, 9994);
        (bool ok, bytes memory result,) = run(PackedTestPlan.encodeChecked(plan, bounds));
        require(
            keccak256(result) != keccak256(moved(0)) && keccak256(result) != keccak256(moved(1)),
            "per-step bounds caught spread drift"
        );
        uint256 used;
        (ok, result, used) = run(PackedTestPlan.encodeCumulative(plan, references, MIN_RATE));
        require(
            !ok && (keccak256(result) == keccak256(moved(0)) || keccak256(result) == keccak256(moved(1))),
            "spread drift accepted"
        );
        require(used < 40_000, "reverted after swapping");
    }

    // One leg 15 bp against the route, the other 10 bp in its favour: 5 bp net,
    // within the slack. Per-step bounds reject the first leg; the product passes.
    function testCumulativeLetsAFavourableMoveOffset() public {
        T.Plan memory plan = twoPriced(false);
        uint160[] memory references = currentBounds(plan);
        uint160[] memory bounds = boundsAt(plan, references, 999499875);
        shiftRate(plan, 0, 9985);
        shiftRate(plan, 1, 10010);
        (bool ok, bytes memory result,) = run(PackedTestPlan.encodeChecked(plan, bounds));
        require(!ok && keccak256(result) == keccak256(moved(0)), "per-step bound missed the adverse leg");
        (ok,,) = run(PackedTestPlan.encodeCumulative(plan, references, MIN_RATE));
        require(ok, "offsetting moves rejected");
    }

    function testCumulativeCatchesAPairLeg() public {
        T.Plan memory plan = cycle(firsts[0], true); // the V2 leg sells token1 for token0
        uint160[] memory references = currentBounds(plan);
        token1.mint(address(v2), 2_000); // a rival sale moves the V2 leg 0.2% against us
        v2.sync();
        (bool ok, bytes memory result,) = run(PackedTestPlan.encodeCumulative(plan, references, MIN_RATE));
        require(!ok && keccak256(result) == keccak256(moved(1)), "pair drift accepted");
    }

    function testMalformedMinimumRateRejected() public {
        T.Plan memory plan = cycle(firsts[0], true);
        uint160[] memory references = currentBounds(plan);
        uint64[3] memory invalid = [uint64(0), uint64(1e18), type(uint64).max];
        for (uint256 i; i < invalid.length; ++i) {
            vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
            parser.decode(PackedTestPlan.encodeCumulative(plan, references, invalid[i]));
        }
        bytes memory encoded = PackedTestPlan.encodeCumulative(plan, references, MIN_RATE);
        require(parser.decodeRate(encoded) == MIN_RATE, "rate round trip");
        bytes memory header = new bytes(100);
        for (uint256 i; i < header.length; ++i) {
            header[i] = encoded[i];
        }
        vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
        parser.decode(header);
    }

    function testFuzzMulDivMatchesExactArithmetic(uint256 a, uint256 b, uint256 d) public view {
        if (d == 0) d = 1;
        // Products that fit 256 bits match plain arithmetic.
        uint128 x = uint128(a);
        uint128 y = uint128(b);
        require(parser.mulDiv(x, y, d) == uint256(x) * y / d, "small product");
        // Full 512-bit products divide back exactly.
        require(parser.mulDiv(a, d, d) == a, "a * d / d");
        // A quotient beyond 256 bits saturates.
        if (a > 1 && d < a) require(parser.mulDiv(a, type(uint256).max, d) == type(uint256).max, "saturation");
    }

    function testGasCumulativePrecheckSuccess() public {
        T.Plan memory plan = cycle(firsts[0], true);
        measure(
            "cumulative_precheck_success", PackedTestPlan.encodeCumulative(plan, currentBounds(plan), MIN_RATE), true
        );
    }

    // Two priced V3-family legs: token0 -> token1 through the V3 pool and back
    // through the Slipstream pool, or the reverse.
    function twoPriced(bool zeroForOne) private view returns (T.Plan memory plan) {
        (address asset, address middle) =
            zeroForOne ? (address(token0), address(token1)) : (address(token1), address(token0));
        plan = walletPlan(firsts[0], firsts[1], asset, middle);
    }

    // Moves a priced leg's rate to basisPoints / 10000 of its quote.
    function shiftRate(T.Plan memory plan, uint256 index, uint256 basisPoints) private {
        address tokenIn = index == 0 ? plan.asset : plan.steps[index - 1].tokenOut;
        PricedV3Pool pool = PricedV3Pool(plan.steps[index].target);
        uint256 root = sqrt(basisPoints * 1e14); // sqrt(rate) in 1e9
        uint256 price = pool.price();
        pool.setPrice(uint160(tokenIn < plan.steps[index].tokenOut ? price * root / 1e9 : price * 1e9 / root));
    }

    // Per-step version 3 bounds at a rate of rootRate² (rootRate in 1e9).
    function boundsAt(T.Plan memory plan, uint160[] memory references, uint256 rootRate)
        private
        pure
        returns (uint160[] memory bounds)
    {
        bounds = new uint160[](references.length);
        address tokenIn = plan.asset;
        for (uint256 i; i < references.length; ++i) {
            bool zeroForOne = tokenIn < plan.steps[i].tokenOut;
            bounds[i] =
                uint160(zeroForOne ? uint256(references[i]) * rootRate / 1e9 : uint256(references[i]) * 1e9 / rootRate);
            tokenIn = plan.steps[i].tokenOut;
        }
    }

    function sqrt(uint256 x) private pure returns (uint256 y) {
        y = x;
        uint256 z = (x + 1) / 2;
        while (z < y) (y, z) = (z, (x / z + z) / 2);
    }

    function cycle(bytes32 first, bool zeroForOne) private view returns (T.Plan memory plan) {
        (address asset, address middle) =
            zeroForOne ? (address(token0), address(token1)) : (address(token1), address(token0));
        plan = walletPlan(first, v2Key, asset, middle);
    }

    function currentBounds(T.Plan memory plan) private view returns (uint160[] memory bounds) {
        bounds = new uint160[](plan.steps.length);
        address tokenIn = plan.asset;
        for (uint256 i; i < plan.steps.length; ++i) {
            T.Step memory current = plan.steps[i];
            bool zeroForOne = tokenIn < current.tokenOut;
            if (current.kind == T.V2 || current.kind == T.VELODROME_V2) {
                (uint256 reserve0, uint256 reserve1) = reserves(current.target, current.kind);
                (uint256 reserveIn, uint256 reserveOut) = zeroForOne ? (reserve0, reserve1) : (reserve1, reserve0);
                bounds[i] = uint160((reserveOut << 96) / reserveIn);
            } else if (current.kind == T.V4) {
                bounds[i] = uint160(uint256(PricedV4Manager(payable(current.target)).extsload(v4Slot)));
            } else {
                bounds[i] = PricedV3Pool(current.target).price();
            }
            tokenIn = current.tokenOut;
        }
    }

    function reserves(address target, uint8 kind) private view returns (uint256 reserve0, uint256 reserve1) {
        if (kind == T.V2) {
            (uint112 a, uint112 b,) = TestV2Pool(target).getReserves();
            return (a, b);
        }
        (reserve0, reserve1,) = PricedVelodromePool(target).getReserves();
    }

    // Moves first pool i by the smallest step its bound can see.
    function movePrice(uint256 i, bool zeroForOne, bool favourable) private {
        // Selling token0 gains when token0's price in token1 rises.
        bool up = zeroForOne == favourable;
        if (i < 4) {
            PricedV3Pool pool = PricedV3Pool(testPools[firsts[i]].target);
            pool.setPrice(up ? ONE + 1 : ONE - 1);
        } else if (i == 4) {
            // Price is reserve1 / reserve0. A tenth of a percent clears floor rounding.
            velodrome.setReserves(up ? 1_000_000 : 1_001_000, up ? 1_001_000 : 1_000_000);
        } else {
            setV4Price(up ? ONE + 1 : ONE - 1);
        }
    }

    function restorePrice(uint256 i) private {
        if (i < 4) PricedV3Pool(testPools[firsts[i]].target).setPrice(ONE);
        else if (i == 4) velodrome.setReserves(1_000_000, 1_000_000);
        else setV4Price(ONE);
    }

    // The upper bits hold tick and fees, which the precheck must mask off.
    function setV4Price(uint160 price) private {
        manager.setWord(v4Slot, bytes32((uint256(0xffffff) << 160) | price));
    }

    function moved(uint256 index) private pure returns (bytes memory) {
        return abi.encodeWithSelector(PriceCheck.PoolMoved.selector, index);
    }

    function run(bytes memory encoded) private returns (bool ok, bytes memory result, uint256 used) {
        vm.prank(OPERATOR);
        return measuredCall(address(executor), abi.encodeCall(executor.executePacked, (encoded)));
    }
}
