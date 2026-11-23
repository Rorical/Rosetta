// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

library ExecutorTypes {
    // Values match domain.PoolKind. Pancake uses V3 with its own callback.
    uint8 internal constant V2 = 1;
    uint8 internal constant V3 = 2;
    uint8 internal constant V4 = 3;
    uint8 internal constant CAMELOT_V3 = 4;
    uint8 internal constant SLIPSTREAM = 5;
    uint8 internal constant VELODROME_V2 = 6;
    // Uniswap V3 fork: same swap selector, solidlyV3SwapCallback.
    uint8 internal constant SOLIDLY_V3 = 7;
    // Oracle-priced bins: its own swap signature and metricOmmSwapCallback.
    uint8 internal constant METRIC_OMM = 8;
    // The wrapped native token itself: deposit when the input is native
    // currency (address(0)), withdraw when the output is. Always 1:1.
    uint8 internal constant WRAP = 9;

    enum Funding {
        Wallet,
        V3Flash,
        V3FlashSwap,
        V4Flash,
        // The principal is the executor's own balance of the asset.
        Inventory
    }

    // Pool parameters travel in calldata; there is no on-chain pool registry.
    // Per-leg balance deltas, transient callback authentication and the closing
    // profit check bound a wrong or hostile target to a reverted transaction.
    // tokenIn is the preceding leg's tokenOut (the asset for the first leg).
    struct Step {
        uint8 kind;
        address target; // pool, the V4 manager, or the wrapped native token
        address tokenOut;
        uint24 fee; // V2 input fee in pips and the V4 key fee; other kinds ignore it
        int24 tickSpacing; // V4 key only
        uint256 minAmountOut;
        uint160 sqrtPriceLimitX96;
    }

    struct RouteStep {
        address target;
        address tokenIn;
        address tokenOut;
        uint256 minimum;
        uint160 limit;
        uint24 fee;
        int24 tickSpacing;
        uint8 kind;
    }

    struct Plan {
        Funding funding;
        address lender; // standalone V3 flash only
        bool lenderToken0; // the lender sends the asset as token0
        address asset;
        uint256 amount;
        uint256 maxLoanFee;
        Step[] steps;
        uint256 minProfit;
        uint256 deadline;
    }

    struct PoolKey {
        address currency0;
        address currency1;
        uint24 fee;
        int24 tickSpacing;
        address hooks;
    }

    struct SwapParams {
        bool zeroForOne;
        int256 amountSpecified;
        uint160 sqrtPriceLimitX96;
    }
}

interface IV2Pool {
    function getReserves() external view returns (uint112, uint112, uint32);
    function swap(uint256, uint256, address, bytes calldata) external;
}

interface IV3Pool {
    function swap(address, bool, int256, uint160, bytes calldata) external returns (int256, int256);
    function flash(address, uint256, uint256, bytes calldata) external;
}

interface IMetricPool {
    function swap(address, bool, int128, uint128, bytes calldata, bytes calldata) external returns (int128, int128);
}

interface IV4Manager {
    function unlock(bytes calldata) external returns (bytes memory);
    function swap(ExecutorTypes.PoolKey calldata, ExecutorTypes.SwapParams calldata, bytes calldata)
        external
        returns (int256);
    function sync(address) external;
    function settle() external payable returns (uint256);
    function take(address, address, uint256) external;
}

interface IVelodromePool {
    function getAmountOut(uint256 amountIn, address tokenIn) external view returns (uint256);
}
