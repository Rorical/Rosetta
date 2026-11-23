// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {V4NettingFixture} from "./V4NettingFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";

/// @dev Same mock and call boundaries before/after netting; no intrinsic/L1 fees or refunds.
contract ExecutorV4GasTest is V4NettingFixture {
    event GasMeasured(string scenario, uint256 gasUsed);

    function measure(string memory scenario, T.Plan memory plan, bytes4 error_) private {
        bytes memory payload = abi.encodeCall(executor.execute, (plan));
        vm.prank(OPERATOR);
        (bool ok, bytes memory result, uint256 used) = measuredCall(address(executor), payload);
        emit GasMeasured(scenario, used);
        uint256 kept;
        if (error_ == bytes4(0)) {
            require(ok, "execution failed");
            (kept,) = abi.decode(result, (uint256, uint256));
            require(kept >= plan.minProfit, "profit");
        } else {
            require(!ok && bytes4(result) == error_, "wrong revert");
        }
        // Every measured route starts and ends in token0.
        assertKept(token0, kept);
        assertEq(token1.balanceOf(address(executor)), 0);
        assertEq(address(executor).balance, 0);
    }

    function testGasV4Cycle() public {
        measure("v4_cycle", cyclePlan(), 0);
    }

    function testGasV4FlashSwapTail() public {
        measure("v4_flash_swap_tail", flashTailPlan(), 0);
    }

    function testGasV4SeparateManagers() public {
        measure("v4_separate_managers", abi.decode(encodedSeparatedCycle, (T.Plan)), 0);
    }

    function testGasV4FirstSlippage() public {
        T.Plan memory plan = cyclePlan();
        plan.steps[0].minAmountOut = 2001;
        measure("v4_first_slippage", plan, SwapExecutor.Slippage.selector);
    }

    function testGasV4InsufficientProfit() public {
        T.Plan memory plan = cyclePlan();
        plan.minProfit = 201;
        measure("v4_insufficient_profit", plan, SwapExecutor.InsufficientProfit.selector);
    }

    function testGasV4SixteenSteps() public {
        T.Plan memory plan = cyclePlan();
        T.Step memory first = plan.steps[0];
        T.Step memory second = plan.steps[1];
        plan.steps = new T.Step[](16);
        for (uint256 i; i < 16; ++i) {
            plan.steps[i] = i % 2 == 0 ? first : second;
        }
        measure("v4_sixteen_steps", plan, 0);
    }
}
