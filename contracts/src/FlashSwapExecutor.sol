// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T, IV3Pool} from "./ExecutorTypes.sol";
import {V4SwapExecutor} from "./V4SwapExecutor.sol";
import {ExecutionState as S} from "./ExecutionState.sol";
import {TokenActions} from "./TokenActions.sol";

/// @dev The first V3 swap delivers output before requesting its input payment.
/// Its authenticated callback runs the tail and pays only from the completed cycle.
abstract contract FlashSwapExecutor is V4SwapExecutor {
    struct Continuation {
        T.RouteStep[] route;
        uint256 amount;
        uint256 minProfit;
        uint256 nonce;
    }

    constructor(address owner_) V4SwapExecutor(owner_) {}

    function runFlashSwap(T.RouteStep[] memory route, uint256 amount, uint256 minProfit) internal {
        if (amount > uint256(type(int256).max)) revert InvalidSwap();
        T.RouteStep memory first = route[0];
        bool zeroForOne = first.tokenIn < first.tokenOut;
        bytes memory payload = abi.encode(Continuation(route, amount, minProfit, S.nextNonce()));
        S.setFlashSwapResult(0);
        S.startSwap(first.target, T.V3 | FLASH_SWAP_FLAG, keccak256(payload));
        (int256 delta0, int256 delta1) =
            IV3Pool(first.target).swap(address(this), zeroForOne, int256(amount), first.limit, payload);
        uint256 delivered = S.flashSwapResult();
        if (S.swapPending() || delivered == 0) revert InvalidCallback();
        (int256 input, int256 output) = zeroForOne ? (delta0, delta1) : (delta1, delta0);
        if (input != int256(amount) || output >= 0 || uint256(-output) != delivered) revert InvalidSwap();
        S.setFlashSwapResult(0);
    }

    function completeFlashSwap(int256 amount0, int256 amount1, bytes calldata payload) internal override {
        Continuation memory current = abi.decode(payload, (Continuation));
        T.RouteStep memory first = current.route[0];
        uint256 amount = current.amount;
        bool zeroForOne = first.tokenIn < first.tokenOut;
        (int256 input, int256 output) = zeroForOne ? (amount0, amount1) : (amount1, amount0);
        if (input != int256(amount) || output >= 0) revert InvalidSwap();
        uint256 delivered = uint256(-output);
        checkOutput(delivered, first.minimum, 0);
        // The last leg's output must cover the repayment and minimum profit;
        // the plan's asset and inventory checks verify what actually arrived.
        runRoute(current.route, delivered, 1, amount + current.minProfit);
        TokenActions.payPool(first.tokenIn, first.target, amount);
        S.setFlashSwapResult(delivered);
    }
}
