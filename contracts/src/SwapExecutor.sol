// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T, IV2Pool, IV3Pool, IVelodromePool, IMetricPool} from "./ExecutorTypes.sol";
import {ExecutorAdmin} from "./ExecutorAdmin.sol";
import {TokenActions} from "./TokenActions.sol";
import {ExecutionState as S} from "./ExecutionState.sol";

abstract contract SwapExecutor is ExecutorAdmin {
    error InvalidRoute();
    error InvalidCallback();
    error InvalidSwap();
    error Slippage();
    error InsufficientProfit();

    uint8 internal constant FLASH_SWAP_FLAG = 128;

    struct SwapPayment {
        address tokenIn;
        uint256 amount;
        uint256 minimum;
        uint256 profitFloor;
        bool zeroForOne;
        uint256 nonce;
    }

    uint160 internal constant MIN_PRICE = 4295128740;
    uint160 internal constant MAX_PRICE = 1461446703485210103287273052203988822378723970341;

    constructor(address owner_) ExecutorAdmin(owner_) {}

    function prepareRoute(T.Plan memory plan, address forbiddenPool)
        internal
        view
        returns (T.RouteStep[] memory route)
    {
        route = new T.RouteStep[](plan.steps.length);
        address token = plan.asset;
        // Validate every leg before funding or touching pool state.
        for (uint256 i; i < plan.steps.length; ++i) {
            T.Step memory step = plan.steps[i];
            uint8 kind = step.kind;
            address target = step.target;
            address tokenOut = step.tokenOut;
            if (
                kind < T.V2 || kind > T.WRAP || target == address(0) || target == address(this)
                    || target == forbiddenPool || tokenOut == token
            ) revert InvalidRoute();
            // Only the V4 manager settles native currency, and only the wrapped
            // native token converts it: such a leg has native currency on one
            // side and its target, the token, on the other. Elsewhere
            // address(0) would make TokenActions move ether to the target.
            bool native = token == address(0) || tokenOut == address(0);
            if (kind == T.WRAP ? (!native || (token != target && tokenOut != target)) : (kind != T.V4 && native)) {
                revert InvalidRoute();
            }
            if (plan.funding == T.Funding.V3FlashSwap) {
                if (i == 0 && kind != T.V3 && kind != T.SLIPSTREAM && kind != T.SOLIDLY_V3) revert InvalidRoute();
                if (i != 0 && target == route[0].target) revert InvalidRoute();
            } else if (plan.funding == T.Funding.V4Flash) {
                if (kind != T.V4 || (i != 0 && target != route[0].target)) revert InvalidRoute();
            }
            uint160 limit;
            if (kind == T.V2 || kind == T.VELODROME_V2 || kind >= T.METRIC_OMM) {
                if (step.sqrtPriceLimitX96 != 0) revert InvalidRoute();
            } else {
                limit = priceLimit(token < tokenOut, step.sqrtPriceLimitX96);
            }
            route[i] = T.RouteStep(target, token, tokenOut, step.minAmountOut, limit, step.fee, step.tickSpacing, kind);
            token = tokenOut;
        }
        if (token != plan.asset) revert InvalidRoute();
    }

    function runRoute(T.RouteStep[] memory route, uint256 amount, uint256 firstStep, uint256 requiredOutput)
        internal
        returns (uint256)
    {
        for (uint256 i = firstStep; i < route.length;) {
            if (route[i].kind == T.V4) {
                (amount, i) = runV4(route, i, amount, requiredOutput);
            } else {
                amount = runStep(route[i], amount, i + 1 == route.length ? requiredOutput : 0);
                ++i;
            }
        }
        return amount;
    }

    function runStep(T.RouteStep memory step, uint256 amount, uint256 profitFloor) private returns (uint256) {
        // A token that delivers less than the pool sent is caught by the next
        // payment or by the plan's inventory check (inventoryBefore).
        if (step.kind == T.WRAP) wrap(step, amount);
        else if (step.kind == T.VELODROME_V2) amount = swapVelodrome(step, amount, profitFloor);
        else if (step.kind == T.V2) amount = swapV2(step, amount, profitFloor);
        else if (step.kind == T.METRIC_OMM) amount = swapMetric(step, amount, profitFloor);
        else amount = swapV3(step, amount, profitFloor);
        checkOutput(amount, step.minimum, profitFloor);
        return amount;
    }

    /// @dev The executor's balance of each intermediate token before the route.
    /// Profit stays as inventory, so a route must leave every token it passes
    /// through as it found it; per-leg balance reads would cost about a quarter
    /// of a two-hop route's own gas.
    function inventoryBefore(T.RouteStep[] memory route, address asset)
        internal
        view
        returns (uint256[] memory balances)
    {
        balances = new uint256[](route.length);
        for (uint256 i; i + 1 < route.length; ++i) {
            address token = route[i].tokenOut;
            if (token != asset) balances[i] = TokenActions.balance(token, address(this));
        }
    }

    function checkInventory(T.RouteStep[] memory route, address asset, uint256[] memory balances) internal view {
        for (uint256 i; i + 1 < route.length; ++i) {
            address token = route[i].tokenOut;
            if (token != asset && TokenActions.balance(token, address(this)) != balances[i]) revert InvalidSwap();
        }
    }

    function checkOutput(uint256 amount, uint256 minimum, uint256 profitFloor) internal pure {
        if (amount == 0 || amount < minimum) revert Slippage();
        if (amount < profitFloor) revert InsufficientProfit();
    }

    /// @dev Deposits or withdraws amount at the wrapped native token; the
    /// output is the input. A target that credits or sends less, or has no
    /// code, leaves the asset or an intermediate balance short at the plan's
    /// closing checks.
    function wrap(T.RouteStep memory step, uint256 amount) private {
        address target = step.target;
        bool deposit = step.tokenIn == address(0);
        // receive() accepts ether only from the recorded sender, so the token
        // is recorded for exactly the duration of its withdrawal.
        if (!deposit) S.setManager(target);
        bool ok;
        assembly ("memory-safe") {
            // withdraw(uint256), replaced by deposit() carrying the amount.
            mstore(0, shl(224, 0x2e1a7d4d))
            mstore(4, amount)
            let value := 0
            let size := 36
            if deposit {
                mstore(0, shl(224, 0xd0e30db0))
                value := amount
                size := 4
            }
            ok := call(gas(), target, value, 0, size, 0, 0)
        }
        if (!ok) revert InvalidSwap();
        S.setManager(address(0));
    }

    function swapV2(T.RouteStep memory step, uint256 amount, uint256 profitFloor) private returns (uint256 out) {
        // A successful V2 reserve update must fit uint112; the product then fits uint256.
        if (amount > type(uint112).max) revert InvalidSwap();
        bool zeroForOne = step.tokenIn < step.tokenOut;
        (uint112 reserve0, uint112 reserve1,) = IV2Pool(step.target).getReserves();
        (uint256 reserveIn, uint256 reserveOut) = zeroForOne ? (reserve0, reserve1) : (reserve1, reserve0);
        uint256 amountWithFee = amount * (1_000_000 - step.fee);
        if (reserveIn == 0 || reserveOut == 0) revert InvalidSwap();
        out = amountWithFee * reserveOut / (reserveIn * 1_000_000 + amountWithFee);
        checkOutput(out, step.minimum, profitFloor);
        TokenActions.payPool(step.tokenIn, step.target, amount);
        IV2Pool(step.target).swap(zeroForOne ? 0 : out, zeroForOne ? out : 0, address(this), "");
    }

    function swapVelodrome(T.RouteStep memory step, uint256 amount, uint256 profitFloor) private returns (uint256 out) {
        // The pool applies its stable/volatile invariant and current factory fee.
        out = IVelodromePool(step.target).getAmountOut(amount, step.tokenIn);
        checkOutput(out, step.minimum, profitFloor);
        TokenActions.payPool(step.tokenIn, step.target, amount);
        bool zeroForOne = step.tokenIn < step.tokenOut;
        IV2Pool(step.target).swap(zeroForOne ? 0 : out, zeroForOne ? out : 0, address(this), "");
    }

    function swapV3(T.RouteStep memory step, uint256 amount, uint256 profitFloor) private returns (uint256 out) {
        if (amount > uint256(type(int256).max)) revert InvalidSwap();
        bool zeroForOne = step.tokenIn < step.tokenOut;
        bytes memory payload =
            abi.encode(SwapPayment(step.tokenIn, amount, step.minimum, profitFloor, zeroForOne, S.nextNonce()));
        S.startSwap(step.target, step.kind, keccak256(payload));
        (int256 delta0, int256 delta1) =
            IV3Pool(step.target).swap(address(this), zeroForOne, int256(amount), step.limit, payload);
        if (S.swapPending()) revert InvalidCallback();
        (int256 input, int256 output) = zeroForOne ? (delta0, delta1) : (delta1, delta0);
        if (input != int256(amount) || output >= 0) revert InvalidSwap();
        out = uint256(-output);
    }

    /// @dev A Metric pool's limit is a fee-inclusive Q64.64 price: a floor when
    /// selling token0, where zero is no floor, and a cap when buying it, where
    /// zero fills nothing. The leg is bounded by its minimum output instead.
    function swapMetric(T.RouteStep memory step, uint256 amount, uint256 profitFloor) private returns (uint256 out) {
        if (amount > uint128(type(int128).max)) revert InvalidSwap();
        bool zeroForOne = step.tokenIn < step.tokenOut;
        bytes memory payload =
            abi.encode(SwapPayment(step.tokenIn, amount, step.minimum, profitFloor, zeroForOne, S.nextNonce()));
        S.startSwap(step.target, step.kind, keccak256(payload));
        (int128 delta0, int128 delta1) = IMetricPool(step.target)
            .swap(address(this), zeroForOne, int128(uint128(amount)), zeroForOne ? 0 : type(uint128).max, payload, "");
        if (S.swapPending()) revert InvalidCallback();
        (int256 input, int256 output) = zeroForOne ? (delta0, delta1) : (delta1, delta0);
        if (input != int256(amount) || output >= 0) revert InvalidSwap();
        out = uint256(-output);
    }

    function metricOmmSwapCallback(int256 amount0, int256 amount1, bytes calldata payload) external {
        if (hotSwapCallback(amount0, amount1)) return;
        S.Swap memory current = S.swap();
        if (current.kind != T.METRIC_OMM) revert InvalidCallback();
        paySwap(current, amount0, amount1, payload);
    }

    /// @dev HotV3Executor's callback; false when no hot route expects one.
    function hotSwapCallback(int256 amount0, int256 amount1) internal virtual returns (bool);

    function uniswapV3SwapCallback(int256 amount0, int256 amount1, bytes calldata payload) external {
        if (hotSwapCallback(amount0, amount1)) return;
        S.Swap memory current = S.swap();
        if (current.kind != T.V3 && current.kind != T.SLIPSTREAM && current.kind != (T.V3 | FLASH_SWAP_FLAG)) {
            revert InvalidCallback();
        }
        paySwap(current, amount0, amount1, payload);
    }

    function solidlyV3SwapCallback(int256 amount0, int256 amount1, bytes calldata payload) external {
        S.Swap memory current = S.swap();
        if (current.kind != T.SOLIDLY_V3 && current.kind != (T.V3 | FLASH_SWAP_FLAG)) revert InvalidCallback();
        paySwap(current, amount0, amount1, payload);
    }

    function pancakeV3SwapCallback(int256 amount0, int256 amount1, bytes calldata payload) external {
        if (hotSwapCallback(amount0, amount1)) return;
        S.Swap memory current = S.swap();
        if (current.kind != T.V3 && current.kind != (T.V3 | FLASH_SWAP_FLAG)) revert InvalidCallback();
        paySwap(current, amount0, amount1, payload);
    }

    function algebraSwapCallback(int256 amount0, int256 amount1, bytes calldata payload) external {
        if (hotSwapCallback(amount0, amount1)) return;
        S.Swap memory current = S.swap();
        if (current.kind != T.CAMELOT_V3) revert InvalidCallback();
        paySwap(current, amount0, amount1, payload);
    }

    function paySwap(S.Swap memory current, int256 amount0, int256 amount1, bytes calldata payload) private {
        if (
            !S.executing() || current.target == address(0) || msg.sender != current.target
                || keccak256(payload) != current.dataHash
        ) {
            revert InvalidCallback();
        }
        if (current.kind == (T.V3 | FLASH_SWAP_FLAG)) {
            S.consumeSwap();
            completeFlashSwap(amount0, amount1, payload);
            return;
        }
        SwapPayment memory payment = abi.decode(payload, (SwapPayment));
        (int256 input, int256 output) = payment.zeroForOne ? (amount0, amount1) : (amount1, amount0);
        if (input <= 0 || uint256(input) != payment.amount || output >= 0) revert InvalidSwap();
        checkOutput(uint256(-output), payment.minimum, payment.profitFloor);
        // Clear before calling the token, including tokens with transfer hooks.
        S.consumeSwap();
        TokenActions.payPool(payment.tokenIn, msg.sender, uint256(input));
    }

    function runV4(T.RouteStep[] memory route, uint256 first, uint256 amount, uint256 requiredOutput)
        internal
        virtual
        returns (uint256 output, uint256 next);

    function completeFlashSwap(int256 amount0, int256 amount1, bytes calldata payload) internal virtual;

    function priceLimit(bool zeroForOne, uint160 requested) internal pure returns (uint160) {
        if (requested == 0) return zeroForOne ? MIN_PRICE : MAX_PRICE;
        if (requested < MIN_PRICE || requested > MAX_PRICE) revert InvalidSwap();
        return requested;
    }
}
