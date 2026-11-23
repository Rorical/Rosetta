// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ArbitrageExecutor} from "../src/ArbitrageExecutor.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";

interface MetricForkVm {
    function envOr(string calldata, string calldata) external view returns (string memory);
    function envUint(string calldata) external view returns (uint256);
    function createSelectFork(string calldata, uint256) external returns (uint256);
    function deal(address, uint256) external;
    function prank(address) external;
    function mockCall(address, bytes calldata, bytes calldata) external;
    function snapshotState() external returns (uint256);
    function revertToState(uint256) external returns (bool);
}

interface IMetricForkToken {
    function deposit() external payable;
    function approve(address, uint256) external returns (bool);
    function balanceOf(address) external view returns (uint256);
    function transfer(address, uint256) external returns (bool);
}

interface IMetricForkV3 {
    function slot0() external view returns (uint160, int24, uint16, uint16, uint16, uint8, bool);
}

interface IMetricForkPool {
    function swap(address, bool, int256, uint160, bytes calldata) external returns (int256, int256);
}

interface IMetricForkPair {
    function getReserves() external view returns (uint112, uint112, uint32);
    function swap(uint256, uint256, address, bytes calldata) external;
}

interface IMetricForkProvider {
    function getQuote() external returns (uint128, uint128, uint128);
}

/// @dev The Metric leg against the deployed WETH/USDC pool on Arbitrum One
/// (ROSETTA_ARB_FORK_URL, ROSETTA_ARB_FORK_BLOCK at or after 523360000). The
/// pool's quote is shifted by 30 bps so that a cycle through the Uniswap V3
/// 0.05% pool closes with a profit; the swap, its extensions and the callback
/// are the deployed code.
contract ExecutorMetricForkTest {
    MetricForkVm private constant vm = MetricForkVm(address(uint160(uint256(keccak256("hevm cheat code")))));
    address private constant WETH = 0x82aF49447D8a07e3bd95BD0d56f35241523fBab1;
    address private constant USDC = 0xaf88d065e77c8cC2239327C5EDb3A432268e5831;
    address private constant METRIC = 0x2B4A9DE7BdAffeA997f5E12d0eBbffDDf6AeA3D3;
    address private constant PROVIDER = 0xDE1b439BFA4A342A3d7474f1C267A11D2cbDD2C7;
    address private constant V3 = 0xC6962004f452bE9203591991D15f6b388e09E8D0;
    address private constant OPERATOR = address(0x1234);

    ArbitrageExecutor private executor;
    bool private skip;

    function setUp() public {
        string memory url = vm.envOr("ROSETTA_ARB_FORK_URL", string(""));
        if (bytes(url).length == 0) {
            skip = true;
            return;
        }
        vm.createSelectFork(url, vm.envUint("ROSETTA_ARB_FORK_BLOCK"));
        executor = new ArbitrageExecutor(address(this), [OPERATOR, address(0)]);
        executor.setOperator(OPERATOR, true);
        executor.setPaused(false);
        vm.deal(OPERATOR, 10 ether);
        vm.prank(OPERATOR);
        IMetricForkToken(WETH).deposit{value: 5 ether}();
        vm.prank(OPERATOR);
        IMetricForkToken(WETH).approve(address(executor), type(uint256).max);
    }

    function testSellsToTheMetricPoolAndBuysBackOnV3() public {
        if (skip) return;
        shiftQuote(10030);
        run(METRIC, V3);
    }

    function testSellsOnV3AndBuysFromTheMetricPool() public {
        if (skip) return;
        shiftQuote(9970);
        run(V3, METRIC);
    }

    event GasMeasured(string scenario, uint256 gasUsed);

    /// @dev The chained hot route: the Metric pool is swapped first for 400
    /// USDC in and pays the executor its WETH; its callback asks the V3 pool
    /// for exactly 400 USDC, sent straight to the Metric pool, and the
    /// executor pays the V3 pool in WETH. No USDC reaches the executor.
    function testHotRouteBuysFromTheMetricPool() public {
        if (skip) return;
        shiftQuote(9970);
        bytes memory route = abi.encodePacked(
            WETH,
            uint128(v3InputFor(400e6)),
            uint128(1),
            uint8(2),
            abi.encodePacked(uint8(1), uint88(0), V3),
            abi.encodePacked(uint8(4), uint88(400e6), METRIC)
        );
        uint256 kept = IMetricForkToken(WETH).balanceOf(address(executor));
        vm.prank(OPERATOR);
        uint256 start = gasleft();
        uint256 profit = executor.executeHot(route);
        emit GasMeasured("v3_metric_hot", start - gasleft());
        // 400 USDC is about 0.2 WETH; 30 bps of it less both pools' fees.
        require(profit > 0.0002 ether && profit < 0.0008 ether, "profit outside the shifted quote's range");
        require(IMetricForkToken(WETH).balanceOf(address(executor)) == kept + profit, "profit not kept");
        require(IMetricForkToken(USDC).balanceOf(address(executor)) == 0, "USDC reached the executor");
    }

    /// @dev The same route with the oracle quote moved against it after the
    /// bot priced it: the Metric pool returns less WETH than the principal,
    /// and the route fails in the pool's callback, before the V3 swap.
    function testHotRouteFailsInTheMetricCallbackWhenTheQuoteMoved() public {
        if (skip) return;
        shiftQuote(10030);
        bytes memory route = abi.encodePacked(
            WETH,
            uint128(v3InputFor(400e6)),
            uint128(1),
            uint8(2),
            abi.encodePacked(uint8(1), uint88(0), V3),
            abi.encodePacked(uint8(4), uint88(400e6), METRIC)
        );
        vm.prank(OPERATOR);
        uint256 start = gasleft();
        (bool ok, bytes memory data) = address(executor).call(abi.encodeCall(executor.executeHot, (route)));
        emit GasMeasured("v3_metric_hot_quote_moved", start - gasleft());
        require(
            !ok && bytes4(data) == bytes4(keccak256("InsufficientProfit()")),
            "the moved quote did not fail the return check"
        );
        require(start - gasleft() < 180000, "the failure ran past the Metric pool's callback");
    }

    address private constant PAIR = 0xF64Dfe17C8b87F012FCf50FbDA1D62bfA148366a; // Camelot V2 WETH/USDC

    /// @dev The chained hot route with a V2 pair after the pool: the V3 pool
    /// is swapped for 0.05 WETH in and sends its USDC straight to the pair,
    /// the pair sends the quoted WETH to the executor, and the executor pays
    /// the V3 pool. A victim's WETH sale into the pair opens the gap.
    function testHotRouteSellsOnV3AndBuysFromAPair() public {
        if (skip) return;
        vm.prank(OPERATOR);
        IMetricForkToken(WETH).transfer(address(this), 1 ether);
        IMetricForkToken(WETH).transfer(PAIR, 0.5 ether);
        (uint256 weth, uint256 usdc,) = IMetricForkPair(PAIR).getReserves();
        IMetricForkPair(PAIR).swap(0, usdc * 4 / 100, address(this), "");
        uint256 amount = 0.05 ether;
        // What the V3 pool pays for the amount: the bot's quote.
        uint256 snapshot = vm.snapshotState();
        uint256 held = IMetricForkToken(USDC).balanceOf(address(this));
        IMetricForkPool(V3).swap(address(this), true, int256(amount), 4295128740, "");
        uint256 middle = IMetricForkToken(USDC).balanceOf(address(this)) - held;
        vm.revertToState(snapshot);
        (weth, usdc,) = IMetricForkPair(PAIR).getReserves();
        uint256 out = middle * 997 * weth / (usdc * 1000 + middle * 997);
        require(out > amount, "the victim left no gap");
        bytes memory route = abi.encodePacked(
            WETH,
            uint128(amount),
            uint128(1),
            uint8(2),
            abi.encodePacked(uint8(1), uint88(amount), V3),
            abi.encodePacked(uint8(2), uint88(out), PAIR)
        );
        uint256 kept = IMetricForkToken(WETH).balanceOf(address(executor));
        vm.prank(OPERATOR);
        uint256 start = gasleft();
        uint256 profit = executor.executeHot(route);
        emit GasMeasured("v3_pair_hot", start - gasleft());
        require(profit == out - amount, "profit is not the quoted output less the input");
        require(IMetricForkToken(WETH).balanceOf(address(executor)) == kept + profit, "profit not kept");
        require(IMetricForkToken(USDC).balanceOf(address(executor)) == 0, "USDC reached the executor");
    }

    /// @dev The WETH the V3 pool takes for usdc out: the bot's quoted input,
    /// which it sends as the route's principal.
    function v3InputFor(uint256 usdc) private returns (uint256 input) {
        uint256 snapshot = vm.snapshotState();
        vm.prank(OPERATOR);
        IMetricForkToken(WETH).transfer(address(this), 1 ether);
        uint256 held = IMetricForkToken(WETH).balanceOf(address(this));
        IMetricForkPool(V3).swap(address(this), true, -int256(usdc), 4295128740, "");
        input = held - IMetricForkToken(WETH).balanceOf(address(this));
        vm.revertToState(snapshot);
    }

    function uniswapV3SwapCallback(int256 amount0, int256 amount1, bytes calldata) external {
        IMetricForkToken(WETH).transfer(msg.sender, uint256(amount0 > 0 ? amount0 : amount1));
    }

    function run(address first, address second) private {
        T.Plan memory plan;
        plan.asset = WETH;
        plan.amount = 0.2 ether;
        plan.steps = new T.Step[](2);
        plan.steps[0] = T.Step(first == METRIC ? T.METRIC_OMM : T.V3, first, USDC, 0, 0, 1, 0);
        plan.steps[1] = T.Step(second == METRIC ? T.METRIC_OMM : T.V3, second, WETH, 0, 0, 1, 0);
        plan.minProfit = 1;
        plan.deadline = block.timestamp + 60;
        uint256 kept = IMetricForkToken(WETH).balanceOf(address(executor));
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        require(profit > 0.0002 ether && profit < 0.0008 ether, "profit outside the shifted quote's range");
        require(IMetricForkToken(WETH).balanceOf(address(executor)) == kept + profit, "profit not kept");
        require(IMetricForkToken(USDC).balanceOf(address(executor)) == 0, "USDC left behind");
    }

    /// @dev The provider reads its oracle only inside a swap (InvalidInSwap
    /// otherwise), so the quote is rebuilt from the V3 pool's price: USDC per
    /// WETH in whole tokens as Q64.64, with a 4 bps spread around it.
    function shiftQuote(uint256 bps) private {
        (uint160 sqrtPrice,,,,,,) = IMetricForkV3(V3).slot0();
        uint256 mid = (uint256(sqrtPrice) * uint256(sqrtPrice) * 1e12 >> 128) * bps / 10000;
        vm.mockCall(
            PROVIDER,
            abi.encodeWithSelector(IMetricForkProvider.getQuote.selector),
            abi.encode(uint128(mid * 9998 / 10000), uint128(mid * 10002 / 10000), uint128(mid))
        );
    }
}
