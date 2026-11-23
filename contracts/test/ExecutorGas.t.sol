// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {ArbitrageExecutor} from "../src/ArbitrageExecutor.sol";
import {HotV3Executor} from "../src/HotV3Executor.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";

/// @dev Identical call boundaries for before/after comparisons. Excludes transaction
/// intrinsic/data fees and refunds; includes CALL overhead. Pools are synthetic.
contract ExecutorGasTest is ExecutorFixture {
    bytes private encodedPlan;
    bytes32 private lenderKey;

    event GasMeasured(string scenario, uint256 gasUsed);

    function setUp() public override {
        super.setUp();
        bytes32 first = register(T.V3, address(v3Pool(2_000_000, 0)));
        bytes32 second = register(T.V3, address(v3Pool(600_000, 0)));
        lenderKey = register(T.V3, address(v3Pool(1_000_000, 0)));
        encodedPlan = abi.encode(walletPlan(first, second, address(token0), address(token1)));
    }

    function savedPlan() private view returns (T.Plan memory) {
        return abi.decode(encodedPlan, (T.Plan));
    }

    function measure(string memory scenario, T.Plan memory plan, bytes4 expectedError, uint256 ceiling) private {
        bytes memory payload = abi.encodeCall(executor.execute, (plan));
        vm.prank(OPERATOR);
        (bool ok, bytes memory result, uint256 used) = measuredCall(address(executor), payload);
        emit GasMeasured(scenario, used);
        require(used <= ceiling, "execution gas regression");
        uint256 kept;
        if (expectedError == bytes4(0)) {
            require(ok, "execution failed");
            (kept,) = abi.decode(result, (uint256, uint256));
            require(kept >= plan.minProfit, "profit");
        } else {
            require(!ok && bytes4(result) == expectedError, "wrong revert");
            assertEq(token0.balanceOf(OPERATOR), 100_000);
        }
        assertKept(token0, kept);
    }

    function testGasWalletSuccess() public {
        measure("wallet_success", savedPlan(), 0, 295_000);
    }

    function testGasInventorySuccess() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.Inventory;
        token0.mint(address(executor), 5_000);
        bytes memory payload = abi.encodeCall(executor.execute, (plan));
        vm.prank(OPERATOR);
        (bool ok,, uint256 used) = measuredCall(address(executor), payload);
        emit GasMeasured("inventory_success", used);
        require(ok && used <= 275_000, "execution gas regression");
        assertEq(token0.balanceOf(address(executor)), 5_000 + 200);
    }

    function testGasFlashSuccess() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3Flash;
        lend(plan, lenderKey);
        plan.maxLoanFee = 5;
        measure("flash_success", plan, 0, 335_000);
    }

    function testGasFlashSwapSuccess() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3FlashSwap;
        measure("flash_swap_success", plan, 0, 275_000);
    }

    function testGasPackedFlashSwapSuccess() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3FlashSwap;
        bytes memory payload = abi.encodeCall(executor.executePacked, (PackedTestPlan.encode(plan)));
        vm.prank(OPERATOR);
        (bool ok, bytes memory result, uint256 used) = measuredCall(address(executor), payload);
        emit GasMeasured("packed_flash_swap_success", used);
        require(ok, "packed flash swap failed");
        (uint256 profit, uint256 fee) = abi.decode(result, (uint256, uint256));
        assertEq(profit, 200);
        assertEq(fee, 0);
    }

    function testGasFlashSwapFirstSlippage() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3FlashSwap;
        plan.steps[0].minAmountOut = 2001;
        measure("flash_swap_first_slippage", plan, SwapExecutor.Slippage.selector, 140_000);
    }

    function testGasFlashSwapInsufficientProfit() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3FlashSwap;
        plan.minProfit = 201;
        measure("flash_swap_insufficient_profit", plan, SwapExecutor.InsufficientProfit.selector, 195_000);
    }

    function testGasStandaloneFlashFirstSlippage() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3Flash;
        lend(plan, lenderKey);
        plan.maxLoanFee = 5;
        plan.steps[0].minAmountOut = 2001;
        measure("standalone_flash_first_slippage", plan, SwapExecutor.Slippage.selector, 210_000);
    }

    function testGasStandaloneFlashInsufficientProfit() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3Flash;
        lend(plan, lenderKey);
        plan.maxLoanFee = 5;
        plan.minProfit = 196;
        measure("standalone_flash_insufficient_profit", plan, SwapExecutor.InsufficientProfit.selector, 280_000);
    }

    function testGasWalletSixteenSteps() public {
        T.Plan memory plan = savedPlan();
        T.Step memory first = plan.steps[0];
        T.Step memory second = plan.steps[1];
        plan.steps = new T.Step[](16);
        for (uint256 i; i < 16; ++i) {
            plan.steps[i] = i % 2 == 0 ? first : second;
        }
        measure("wallet_sixteen_steps", plan, 0, 1_000_000);
    }

    function testGasFirstSlippage() public {
        T.Plan memory plan = savedPlan();
        plan.steps[0].minAmountOut = 2001;
        measure("first_slippage", plan, SwapExecutor.Slippage.selector, 175_000);
    }

    function testGasLastSlippage() public {
        T.Plan memory plan = savedPlan();
        plan.steps[1].minAmountOut = 1201;
        measure("last_slippage", plan, SwapExecutor.Slippage.selector, 245_000);
    }

    function testGasLastStepInvalid() public {
        T.Plan memory plan = savedPlan();
        plan.steps[1].kind = 0;
        measure("last_step_invalid", plan, SwapExecutor.InvalidRoute.selector, 65_000);
    }

    function testGasDisconnectedRoute() public {
        T.Plan memory plan = savedPlan();
        plan.steps[1].tokenOut = address(0x9999);
        measure("disconnected_route", plan, SwapExecutor.InvalidRoute.selector, 68_000);
    }

    function testGasInsufficientProfit() public {
        T.Plan memory plan = savedPlan();
        plan.minProfit = 201;
        measure("insufficient_profit", plan, SwapExecutor.InsufficientProfit.selector, 245_000);
    }

    function testGasFlashFeeExceeded() public {
        T.Plan memory plan = savedPlan();
        plan.funding = T.Funding.V3Flash;
        lend(plan, lenderKey);
        plan.maxLoanFee = 4;
        measure("flash_fee_exceeded", plan, ArbitrageExecutor.InvalidLoan.selector, 160_000);
    }

    function testGasExpired() public {
        T.Plan memory plan = savedPlan();
        plan.deadline = block.timestamp - 1;
        measure("expired", plan, HotV3Executor.InvalidPlan.selector, 9_000);
    }
}
