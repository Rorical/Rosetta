// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {V4NettingFixture} from "./V4NettingFixture.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {PackedPlan} from "../src/PackedPlan.sol";
import {ExecutorAdmin} from "../src/ExecutorAdmin.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";

contract PackedPlanHarness {
    function decode(bytes calldata encoded) external pure returns (T.Plan memory plan) {
        (plan,,) = PackedPlan.decode(encoded);
    }
}

contract ExecutorPackedTest is V4NettingFixture {
    PackedPlanHarness private parser = new PackedPlanHarness();
    event GasMeasured(string scenario, uint256 gasUsed);

    function testPackedExecutesAllFundingModesAndExplicitPriceLimits() public {
        for (uint256 i; i < 4; ++i) {
            T.Plan memory plan = i == 2 ? flashTailPlan() : cyclePlan();
            plan.funding = T.Funding(i);
            if (i == 1) {
                lend(plan, register(T.V3, address(v3Pool(1_000_000, 0))));
                plan.maxLoanFee = 5;
            }
            plan.steps[0].sqrtPriceLimitX96 =
                plan.asset < plan.steps[0].tokenOut ? 4295128740 : 1461446703485210103287273052203988822378723970341;
            bytes memory encoded = PackedTestPlan.encode(plan);
            require(keccak256(abi.encode(parser.decode(encoded))) == keccak256(abi.encode(plan)), "round trip");
            vm.prank(OPERATOR);
            (uint256 profit, uint256 fee) = executor.executePacked(encoded);
            assertEq(profit, i == 1 ? 195 : 200);
            assertEq(fee, i == 1 ? 5 : 0);
        }
    }

    function testEveryTruncatedPrefixAndTrailingDataRejected() public {
        bytes memory encoded = PackedTestPlan.encode(flashTailPlan());
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
    }

    function testReservedFlagsHeaderBoundsAndZeroExplicitLimitRejected() public {
        bytes memory encoded = PackedTestPlan.encode(cyclePlan());
        for (uint256 i; i < 5; ++i) {
            bytes memory changed = bytes.concat(encoded);
            if (i == 0) changed[0] = 0x01;
            if (i == 1) changed[1] = 0xff;
            if (i == 2) changed[2] = 0x00;
            if (i == 3) changed[2] = 0x11;
            if (i == 4) changed[95 + 79] = 0x02; // first step flags
            vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
            parser.decode(changed);
        }
        encoded[encoded.length - 1] = 0x01;
        vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
        parser.decode(bytes.concat(encoded, new bytes(20)));
    }

    function testLenderDirectionByteAndNegativeSpacingDecode() public {
        T.Plan memory plan = cyclePlan();
        plan.funding = T.Funding.V3Flash;
        lend(plan, register(T.V3, address(v3Pool(1_000_000, 0))));
        plan.maxLoanFee = 5;
        plan.steps[1].tickSpacing = -60;
        bytes memory encoded = PackedTestPlan.encode(plan);
        T.Plan memory decoded = parser.decode(encoded);
        require(decoded.steps[1].tickSpacing == -60, "spacing sign");
        require(decoded.lender == plan.lender && decoded.lenderToken0 == plan.lenderToken0, "lender");
        require(keccak256(abi.encode(decoded)) == keccak256(abi.encode(plan)), "round trip");
        encoded[115] = 0x02; // lenderToken0 must be 0 or 1
        vm.expectRevert(PackedPlan.InvalidPackedPlan.selector);
        parser.decode(encoded);
    }

    function testPackedRetainsAccessRouteAndProfitGuards() public {
        T.Plan memory plan = cyclePlan();
        bytes memory encoded = PackedTestPlan.encode(plan);
        vm.expectRevert(ExecutorAdmin.Unauthorized.selector);
        vm.prank(address(0x5678));
        executor.executePacked(encoded);
        plan.steps[0].tokenOut = plan.asset;
        encoded = PackedTestPlan.encode(plan);
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        vm.prank(OPERATOR);
        executor.executePacked(encoded);
        plan = cyclePlan();
        plan.minProfit = 201;
        encoded = PackedTestPlan.encode(plan);
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        vm.prank(OPERATOR);
        executor.executePacked(encoded);
    }

    function testFuzzPackedIntegerWidths(
        uint256 amount,
        uint256 minimum,
        uint160 limit,
        uint64 deadline,
        uint24 fee,
        int24 spacing,
        uint8 kind
    ) public view {
        T.Plan memory plan = cyclePlan();
        plan.amount = amount;
        plan.minProfit = minimum;
        plan.deadline = deadline;
        plan.steps[0].sqrtPriceLimitX96 = limit;
        plan.steps[1].fee = fee;
        plan.steps[1].tickSpacing = spacing;
        plan.steps[1].kind = kind;
        bytes memory encoded = PackedTestPlan.encode(plan);
        require(keccak256(abi.encode(parser.decode(encoded))) == keccak256(abi.encode(plan)), "integer truncation");
    }

    function testGasPackedV4Cycle() public {
        measure("packed_v4_cycle", cyclePlan());
    }

    function testGasPackedFlashSwapTail() public {
        measure("packed_v4_flash_swap_tail", flashTailPlan());
    }

    function measure(string memory scenario, T.Plan memory plan) private {
        bytes memory callData = abi.encodeCall(executor.executePacked, (PackedTestPlan.encode(plan)));
        vm.prank(OPERATOR);
        (bool ok, bytes memory result, uint256 used) = measuredCall(address(executor), callData);
        emit GasMeasured(scenario, used);
        require(ok, "packed execution failed");
        (uint256 profit,) = abi.decode(result, (uint256, uint256));
        assertEq(profit, 200);
    }
}
