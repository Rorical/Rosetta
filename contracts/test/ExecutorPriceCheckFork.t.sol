// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ArbitrageExecutor} from "../src/ArbitrageExecutor.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {PriceCheck} from "../src/PriceCheck.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";
import {CallMeter} from "./CallMeter.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";

interface ForkVm {
    function envOr(string calldata, string calldata) external view returns (string memory);
    function envUint(string calldata) external view returns (uint256);
    function createSelectFork(string calldata, uint256) external returns (uint256);
    function deal(address, uint256) external;
    function prank(address) external;
    function snapshotState() external returns (uint256);
    function revertToState(uint256) external returns (bool);
}

interface IForkToken {
    function deposit() external payable;
    function approve(address, uint256) external returns (bool);
    function transfer(address, uint256) external returns (bool);
    function balanceOf(address) external view returns (uint256);
}

interface IForkV3Pool {
    function swap(address, bool, int256, uint160, bytes calldata) external returns (int256, int256);
}

interface IForkPairPool {
    function getReserves() external view returns (uint256, uint256, uint256);
    function getAmountOut(uint256, address) external view returns (uint256);
    function swap(uint256, uint256, address, bytes calldata) external;
}

/// @dev Gas on live OP pools at a pinned recent block. Skipped unless
/// ROSETTA_OP_FORK_URL and ROSETTA_OP_FORK_BLOCK are set, so the offline suite
/// never forks. A victim trade misprices one pool; the route's quote, the
/// rival's optimally sized run of the same route and the bounds all happen in
/// setUp, so each test's single measured call starts with cold accounts, as a
/// new transaction would. Excludes intrinsic and L1 data gas.
abstract contract ForkRoute {
    ForkVm internal constant vm = ForkVm(address(uint160(uint256(keccak256("hevm cheat code")))));
    address internal constant WETH = 0x4200000000000000000000000000000000000006;
    address internal constant USDC = 0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85;
    address internal constant OP = 0x4200000000000000000000000000000000000042;
    address internal constant MANAGER = 0x9a13F98Cb987694C9F086b1F5eB990EeA8264Ec3;
    address internal constant UNI_WETH_USDC = 0x1fb3cf6e48F1E7B10213E7b6d87D4c073C7Fdb7b; // 0.05%
    address internal constant UNI_OP_USDC = 0xB533c12fB4e7b53b5524EAb9b47d93fF6C7A456F; // 0.3%
    address internal constant SLIP_WETH_USDC = 0x478946BcD4a5a22b316470F5486fAfb928C0bA25; // spacing 100
    address internal constant SLIP_WETH_OP = 0x4DC22588Ade05C40338a9D95A6da9dCeE68Bcd60; // spacing 200
    address internal constant VELO_WETH_USDC = 0xF4F2657AE744354bAcA871E56775e5083F7276Ab;
    address internal constant VELO_WETH_OP = 0xd25711EdfBf747efCE181442Cc1D8F5F8fc8a0D3;
    address internal constant UNIV2_WETH_USDC = 0x4C43646304492A925E335f2b6d840C1489f17815;
    address internal constant OPERATOR = address(0x1234);
    address internal constant RIVAL = address(0x5678);
    uint160 private constant MIN_PRICE = 4295128740;
    uint160 private constant MAX_PRICE = 1461446703485210103287273052203988822378723970341;

    bool private skip;
    ArbitrageExecutor private executor;
    bytes private plain;
    bytes private checked;
    bytes private cumulative;
    uint256 private quotedProfit;
    uint256 private rivalAmount;
    uint256 private slackPpm;
    int256[] private adversePpm;

    event GasMeasured(string scenario, uint256 gasUsed);
    event Quote(string route, uint256 amount, uint256 profit, uint256 rivalAmount, uint256 slackPpm);
    event PoolMove(string route, uint256 step, int256 adversePpm, bool caught);

    function name() internal pure virtual returns (string memory);
    function route() internal pure virtual returns (T.Plan memory plan);
    function victim() internal virtual;
    function lost() internal pure virtual returns (bool);

    function setUp() public {
        string memory url = vm.envOr("ROSETTA_OP_FORK_URL", string(""));
        if (bytes(url).length == 0) {
            skip = true;
            return;
        }
        vm.createSelectFork(url, vm.envUint("ROSETTA_OP_FORK_BLOCK"));
        executor = new ArbitrageExecutor(address(this), [OPERATOR, address(0)]);
        executor.setOperator(OPERATOR, true);
        executor.setOperator(RIVAL, true);
        executor.setPaused(false);
        vm.deal(address(this), 100 ether);
        IForkToken(WETH).deposit{value: 60 ether}();
        fund(OPERATOR);
        fund(RIVAL);
        victim();

        T.Plan memory plan = route();
        plan.deadline = block.timestamp + 60;
        quotedProfit = profitAt(plan, plan.amount);
        require(quotedProfit > 0, "victim left no arbitrage");
        // The rival runs the same cycle at its most profitable multiple of our size.
        uint256 best;
        for (uint256 k = 2; k <= 16; k += 2) {
            uint256 profit = profitAt(plan, plan.amount * k);
            if (profit > best) (best, rivalAmount) = (profit, plan.amount * k);
        }
        require(rivalAmount != 0, "no profitable rival size");
        // Half the quoted profit is the required minimum; the rest is slack.
        plan.minProfit = quotedProfit / 2;
        slackPpm = (quotedProfit - plan.minProfit) * 1e6 / (plan.amount + quotedProfit);
        uint256[] memory quoted = new uint256[](plan.steps.length);
        uint160[] memory bounds = new uint160[](plan.steps.length);
        uint256 required = plan.amount + plan.minProfit;
        uint256 output = plan.amount + quotedProfit;
        address tokenIn = plan.asset;
        for (uint256 i; i < plan.steps.length; ++i) {
            quoted[i] = PoolPrice.read(plan.steps[i], tokenIn);
            bounds[i] = PoolPrice.bound(plan.steps[i], tokenIn, quoted[i], required, output);
            tokenIn = plan.steps[i].tokenOut;
        }
        plain = PackedTestPlan.encode(plan);
        checked = PackedTestPlan.encodeChecked(plan, bounds);
        uint160[] memory references = new uint160[](plan.steps.length);
        for (uint256 i; i < plan.steps.length; ++i) {
            references[i] = uint160(quoted[i]);
        }
        cumulative = PackedTestPlan.encodeCumulative(plan, references, uint64(required * 1e18 / output));
        if (!lost()) return;
        T.Plan memory rival = route();
        rival.amount = rivalAmount;
        rival.minProfit = 1;
        rival.deadline = plan.deadline;
        vm.prank(RIVAL);
        executor.executePacked(PackedTestPlan.encode(rival));
        tokenIn = plan.asset;
        for (uint256 i; i < plan.steps.length; ++i) {
            adversePpm.push(PoolPrice.adversePpm(plan.steps[i], tokenIn, quoted[i]));
            tokenIn = plan.steps[i].tokenOut;
        }
    }

    function testPlainPacked() public {
        if (skip) return;
        (bool ok, bytes memory result, uint256 used) = run(plain);
        emit GasMeasured(string.concat(name(), lost() ? "_lost_plain" : "_success_plain"), used);
        if (lost()) {
            require(!ok, "route survived the rival");
            bytes4 reason = bytes4(result);
            require(
                reason == SwapExecutor.InsufficientProfit.selector || reason == SwapExecutor.Slippage.selector,
                "unexpected revert"
            );
        } else {
            require(ok, "quoted route failed");
        }
    }

    function testPriceChecked() public {
        if (skip) return;
        (bool ok, bytes memory result, uint256 used) = run(checked);
        emit GasMeasured(string.concat(name(), lost() ? "_lost_checked" : "_success_checked"), used);
        emit Quote(name(), route().amount, quotedProfit, rivalAmount, slackPpm);
        if (!lost()) {
            require(ok, "unchanged pools rejected");
            return;
        }
        require(!ok && bytes4(result) == PriceCheck.PoolMoved.selector, "rival move not caught");
        for (uint256 i; i < adversePpm.length; ++i) {
            emit PoolMove(name(), i, adversePpm[i], adversePpm[i] > int256(slackPpm));
        }
    }

    function testCumulativeChecked() public {
        if (skip) return;
        (bool ok, bytes memory result, uint256 used) = run(cumulative);
        emit GasMeasured(string.concat(name(), lost() ? "_lost_cumulative" : "_success_cumulative"), used);
        if (!lost()) {
            require(ok, "unchanged pools rejected");
            return;
        }
        require(!ok && bytes4(result) == PriceCheck.PoolMoved.selector, "rival move not caught");
    }

    function run(bytes memory encoded) private returns (bool, bytes memory, uint256) {
        vm.prank(OPERATOR);
        return CallMeter.call(address(executor), abi.encodeCall(executor.executePacked, (encoded)));
    }

    function profitAt(T.Plan memory plan, uint256 amount) private returns (uint256 profit) {
        uint256 snapshot = vm.snapshotState();
        T.Plan memory trial = abi.decode(abi.encode(plan), (T.Plan));
        trial.amount = amount;
        trial.minProfit = 1;
        vm.prank(OPERATOR);
        try executor.executePacked(PackedTestPlan.encode(trial)) returns (uint256 value, uint256) {
            profit = value;
        } catch {}
        vm.revertToState(snapshot);
    }

    function fund(address account) private {
        IForkToken(WETH).transfer(account, 20 ether);
        vm.prank(account);
        IForkToken(WETH).approve(address(executor), type(uint256).max);
    }

    function step(uint8 kind, address target, address tokenOut, uint24 fee, int24 spacing)
        internal
        pure
        returns (T.Step memory)
    {
        return T.Step(kind, target, tokenOut, fee, spacing, 1, 0);
    }

    function sellV3(address pool, address tokenIn, address tokenOut, uint256 amount) internal {
        bool zeroForOne = tokenIn < tokenOut;
        IForkV3Pool(pool)
            .swap(
                address(this),
                zeroForOne,
                int256(amount),
                zeroForOne ? MIN_PRICE + 1 : MAX_PRICE - 1,
                abi.encode(tokenIn)
            );
    }

    function uniswapV3SwapCallback(int256 amount0, int256 amount1, bytes calldata payload) external {
        IForkToken(abi.decode(payload, (address))).transfer(msg.sender, uint256(amount0 > 0 ? amount0 : amount1));
    }

    function sellVelodrome(address pool, address tokenIn, address tokenOut, uint256 amount) internal {
        uint256 out = IForkPairPool(pool).getAmountOut(amount, tokenIn);
        IForkToken(tokenIn).transfer(pool, amount);
        IForkPairPool(pool).swap(tokenIn < tokenOut ? 0 : out, tokenIn < tokenOut ? out : 0, address(this), "");
    }

    function sellUniswapV2(address pool, address tokenIn, address tokenOut, uint256 amount) internal {
        (uint256 reserve0, uint256 reserve1,) = IForkPairPool(pool).getReserves();
        (uint256 reserveIn, uint256 reserveOut) = tokenIn < tokenOut ? (reserve0, reserve1) : (reserve1, reserve0);
        uint256 out = amount * 997 * reserveOut / (reserveIn * 1000 + amount * 997);
        IForkToken(tokenIn).transfer(pool, amount);
        IForkPairPool(pool).swap(tokenIn < tokenOut ? 0 : out, tokenIn < tokenOut ? out : 0, address(this), "");
    }
}

/// @dev Reads and bounds pool prices the way the Go planner does.
library PoolPrice {
    uint256 private constant Q96 = 2 ** 96;

    function ratioKind(uint8 kind) private pure returns (bool) {
        return kind == T.V2 || kind == T.VELODROME_V2;
    }

    /// @return value sqrtPriceX96, or reserveOut / reserveIn in Q96 for pair pools.
    function read(T.Step memory step, address tokenIn) internal view returns (uint256 value) {
        bool zeroForOne = tokenIn < step.tokenOut;
        if (ratioKind(step.kind)) {
            (bool ok, bytes memory data) = step.target.staticcall(abi.encodeWithSignature("getReserves()"));
            require(ok && data.length >= 64, "reserves");
            (uint256 reserve0, uint256 reserve1) = abi.decode(data, (uint256, uint256));
            return zeroForOne ? reserve1 * Q96 / reserve0 : reserve0 * Q96 / reserve1;
        }
        bytes memory call = abi.encodeWithSignature("slot0()");
        if (step.kind == T.V4) {
            (address currency0, address currency1) = zeroForOne ? (tokenIn, step.tokenOut) : (step.tokenOut, tokenIn);
            bytes32 id = keccak256(abi.encode(currency0, currency1, step.fee, step.tickSpacing, address(0)));
            call = abi.encodeWithSignature("extsload(bytes32)", keccak256(abi.encode(id, uint256(6))));
        }
        (bool success, bytes memory word) = step.target.staticcall(call);
        require(success && word.length >= 32, "price");
        return uint256(bytes32(word)) & type(uint160).max;
    }

    /// @dev The least favourable value at which this pool alone still leaves
    /// required out of the quoted output: the rate may fall by required/output.
    function bound(T.Step memory step, address tokenIn, uint256 value, uint256 required, uint256 output)
        internal
        pure
        returns (uint160)
    {
        if (ratioKind(step.kind)) return uint160(value * required / output);
        if (tokenIn < step.tokenOut) return uint160(value * sqrt(required * Q96 * Q96 / output) / Q96);
        return uint160(value * (sqrt(output * Q96 * Q96 / required) + 1) / Q96 + 1);
    }

    /// @return ppm Fall of this leg's spot rate since the quote; negative when favourable.
    function adversePpm(T.Step memory step, address tokenIn, uint256 quoted) internal view returns (int256 ppm) {
        uint256 current = read(step, tokenIn);
        uint256 rate; // current rate / quoted rate, 1e12 scale
        if (ratioKind(step.kind)) {
            rate = current * 1e12 / quoted;
        } else {
            uint256 ratio = tokenIn < step.tokenOut ? current * 1e6 / quoted : quoted * 1e6 / current;
            rate = ratio * ratio;
        }
        return (1e12 - int256(rate)) / 1e6;
    }

    function sqrt(uint256 x) private pure returns (uint256 z) {
        if (x == 0) return 0;
        z = x;
        uint256 y = x / 2 + 1;
        while (y < z) {
            z = y;
            y = (x / y + y) / 2;
        }
    }
}

// R2: V3 → Velodrome. A victim sells WETH into Velodrome WETH/USDC; we buy it
// back there on the last leg.
abstract contract RouteTwo is ForkRoute {
    function name() internal pure override returns (string memory) {
        return "r2_v3_velodrome";
    }

    function route() internal pure override returns (T.Plan memory plan) {
        plan.asset = WETH;
        plan.amount = 0.01 ether;
        plan.steps = new T.Step[](2);
        plan.steps[0] = step(T.V3, UNI_WETH_USDC, USDC, 500, 10);
        plan.steps[1] = step(T.VELODROME_V2, VELO_WETH_USDC, WETH, 0, 0);
    }

    function victim() internal override {
        sellVelodrome(VELO_WETH_USDC, WETH, USDC, 0.05 ether);
    }
}

// R3: Uniswap V2 → V3 → Slipstream. A victim sells USDC into the V2 pair; we
// sell WETH there on the first leg.
abstract contract RouteThree is ForkRoute {
    function name() internal pure override returns (string memory) {
        return "r3_v2_v3_slipstream";
    }

    function route() internal pure override returns (T.Plan memory plan) {
        plan.asset = WETH;
        plan.amount = 0.002 ether;
        plan.steps = new T.Step[](3);
        plan.steps[0] = step(T.V2, UNIV2_WETH_USDC, USDC, 3000, 0);
        plan.steps[1] = step(T.V3, UNI_OP_USDC, OP, 3000, 60);
        plan.steps[2] = step(T.SLIPSTREAM, SLIP_WETH_OP, WETH, 0, 200);
    }

    function victim() internal override {
        sellV3(UNI_WETH_USDC, WETH, USDC, 0.01 ether);
        sellUniswapV2(UNIV2_WETH_USDC, USDC, WETH, IForkToken(USDC).balanceOf(address(this)));
    }
}

// R4: Slipstream → V4 → V4 (one unlock, native ETH between) → Velodrome. A
// victim sells WETH into Velodrome WETH/OP; we buy it there on the last leg.
abstract contract RouteFour is ForkRoute {
    function name() internal pure override returns (string memory) {
        return "r4_slipstream_v4_v4_velodrome";
    }

    function route() internal pure override returns (T.Plan memory plan) {
        plan.asset = WETH;
        plan.amount = 0.05 ether;
        plan.steps = new T.Step[](4);
        plan.steps[0] = step(T.SLIPSTREAM, SLIP_WETH_USDC, USDC, 0, 100);
        plan.steps[1] = step(T.V4, MANAGER, address(0), 3000, 60);
        plan.steps[2] = step(T.V4, MANAGER, OP, 3000, 60);
        plan.steps[3] = step(T.VELODROME_V2, VELO_WETH_OP, WETH, 0, 0);
    }

    function victim() internal override {
        sellVelodrome(VELO_WETH_OP, WETH, OP, 0.5 ether);
    }
}

contract RouteTwoSuccessForkTest is RouteTwo {
    function lost() internal pure override returns (bool) {
        return false;
    }
}

contract RouteTwoLostForkTest is RouteTwo {
    function lost() internal pure override returns (bool) {
        return true;
    }
}

contract RouteThreeSuccessForkTest is RouteThree {
    function lost() internal pure override returns (bool) {
        return false;
    }
}

contract RouteThreeLostForkTest is RouteThree {
    function lost() internal pure override returns (bool) {
        return true;
    }
}

contract RouteFourSuccessForkTest is RouteFour {
    function lost() internal pure override returns (bool) {
        return false;
    }
}

contract RouteFourLostForkTest is RouteFour {
    function lost() internal pure override returns (bool) {
        return true;
    }
}
