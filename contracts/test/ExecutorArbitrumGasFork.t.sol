// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ArbitrageExecutor} from "../src/ArbitrageExecutor.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {PriceCheck} from "../src/PriceCheck.sol";
import {CallMeter} from "./CallMeter.sol";
import {HotTestRoute} from "./HotTestRoute.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";

interface ArbForkVm {
    function envOr(string calldata, string calldata) external view returns (string memory);
    function envUint(string calldata) external view returns (uint256);
    function createSelectFork(string calldata, uint256) external returns (uint256);
    function deal(address, uint256) external;
    function prank(address) external;
    function snapshotState() external returns (uint256);
    function revertToState(uint256) external returns (bool);
}

interface IArbForkToken {
    function deposit() external payable;
    function transfer(address, uint256) external returns (bool);
    function balanceOf(address) external view returns (uint256);
}

interface IArbForkV3Pool {
    function swap(address, bool, int256, uint160, bytes calldata) external returns (int256, int256);
    function slot0() external view returns (uint160, int24, uint16, uint16, uint16, uint8, bool);
}

/// @dev The floor for a V3-only cycle: a flash swap on the first pool whose
/// callback runs the others and pays it, with no authorization, guard or
/// balance check. Measures how much of the executor's gas is its own.
contract MinimalV3Cycle {
    struct Leg {
        address pool;
        bool zeroForOne;
        address tokenIn;
    }

    uint160 private constant MIN_PRICE = 4295128740;
    uint160 private constant MAX_PRICE = 1461446703485210103287273052203988822378723970341;

    function run(Leg[] calldata legs, uint256 amount) external {
        Leg calldata first = legs[0];
        IArbForkV3Pool(first.pool)
            .swap(
                address(this), first.zeroForOne, int256(amount), limit(first.zeroForOne), abi.encode(legs, uint256(0))
            );
    }

    function uniswapV3SwapCallback(int256 amount0, int256 amount1, bytes calldata data) external {
        (Leg[] memory legs, uint256 index) = abi.decode(data, (Leg[], uint256));
        if (index == 0) {
            uint256 amount = uint256(-(legs[0].zeroForOne ? amount1 : amount0));
            for (uint256 i = 1; i < legs.length; ++i) {
                (int256 delta0, int256 delta1) = IArbForkV3Pool(legs[i].pool)
                    .swap(
                        address(this),
                        legs[i].zeroForOne,
                        int256(amount),
                        limit(legs[i].zeroForOne),
                        abi.encode(legs, i)
                    );
                amount = uint256(-(legs[i].zeroForOne ? delta1 : delta0));
            }
        }
        IArbForkToken(legs[index].tokenIn).transfer(msg.sender, uint256(amount0 > 0 ? amount0 : amount1));
    }

    function limit(bool zeroForOne) private pure returns (uint160) {
        return zeroForOne ? MIN_PRICE + 1 : MAX_PRICE - 1;
    }
}

/// @dev Executor gas on live Arbitrum One Uniswap V3 pools, funded by a flash
/// swap as the bot sends it. Skipped unless ROSETTA_ARB_FORK_URL and
/// ROSETTA_ARB_FORK_BLOCK are set. A victim trade misprices one pool in setUp;
/// each test then measures one cold executePacked or executeHot call,
/// excluding intrinsic and L1 data gas. "lost" runs the rival's same route
/// first, so ours fails its cumulative price check, as a race lost to a higher
/// tip would. The hot route bounds only the victim's pool, as the bot does
/// for a pool its conditions cannot hold; sent conditionally, a rival on the
/// other pools would be rejected free.
abstract contract ArbForkRoute {
    ArbForkVm internal constant vm = ArbForkVm(address(uint160(uint256(keccak256("hevm cheat code")))));
    address internal constant WETH = 0x82aF49447D8a07e3bd95BD0d56f35241523fBab1;
    address internal constant USDC = 0xaf88d065e77c8cC2239327C5EDb3A432268e5831;
    address internal constant USDT = 0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9;
    address internal constant UNI_WETH_USDC_500 = 0xC6962004f452bE9203591991D15f6b388e09E8D0;
    address internal constant UNI_WETH_USDC_3000 = 0xc473e2aEE3441BF9240Be85eb122aBB059A3B57c;
    address internal constant UNI_WETH_USDT_500 = 0x641C00A822e8b671738d32a431a4Fb6074E5c79d;
    address internal constant UNI_USDC_USDT_100 = 0xbE3aD6a5669Dc0B8b12FeBC03608860C31E2eef6;
    address internal constant OPERATOR = address(0x1234);
    address internal constant RIVAL = address(0x5678);
    uint160 private constant MIN_PRICE = 4295128740;
    uint160 private constant MAX_PRICE = 1461446703485210103287273052203988822378723970341;

    bool private skip;
    ArbitrageExecutor private executor;
    bytes private plain;
    bytes private cumulative;
    bytes private hot;

    event GasMeasured(string scenario, uint256 gasUsed);
    event Quote(string route, uint256 amount, uint256 profit);

    function name() internal pure virtual returns (string memory);
    function route() internal pure virtual returns (T.Plan memory plan);
    function victim(uint256 amount) internal virtual;
    function lost() internal pure virtual returns (bool);
    function trigger() internal pure virtual returns (uint8);

    function setUp() public {
        string memory url = vm.envOr("ROSETTA_ARB_FORK_URL", string(""));
        if (bytes(url).length == 0) {
            skip = true;
            return;
        }
        vm.createSelectFork(url, vm.envUint("ROSETTA_ARB_FORK_BLOCK"));
        executor = new ArbitrageExecutor(address(this), [OPERATOR, address(0)]);
        executor.setOperator(OPERATOR, true);
        executor.setOperator(RIVAL, true);
        executor.setPaused(false);
        vm.deal(address(this), 200 ether);
        IArbForkToken(WETH).deposit{value: 150 ether}();
        T.Plan memory plan = route();
        plan.deadline = block.timestamp + 60;
        // The smallest doubling of the victim's trade that leaves a profitable gap.
        uint256 quoted;
        for (uint256 size = 2 ether; size <= 128 ether && quoted == 0; size *= 2) {
            uint256 before = vm.snapshotState();
            victim(size);
            quoted = profitAt(plan, plan.amount);
            if (quoted == 0) vm.revertToState(before);
        }
        require(quoted > 0, "victim left no arbitrage");
        emit Quote(name(), plan.amount, quoted);
        plan.minProfit = quoted / 2;
        uint256 required = plan.amount + plan.minProfit;
        uint256 output = plan.amount + quoted;
        uint160[] memory references = new uint160[](plan.steps.length);
        for (uint256 i; i < plan.steps.length; ++i) {
            (references[i],,,,,,) = IArbForkV3Pool(plan.steps[i].target).slot0();
        }
        plain = PackedTestPlan.encode(plan);
        cumulative = PackedTestPlan.encodeCumulative(plan, references, uint64(required * 1e18 / output));
        hot = HotTestRoute.bound(HotTestRoute.encode(plan, lastInput(plan)), trigger(), false, references[trigger()]);
        if (!lost()) return;
        T.Plan memory rival = route();
        // The rival runs the same route at its most profitable doubling of our size.
        uint256 best;
        for (uint256 amount = plan.amount * 2; amount <= plan.amount * 2048; amount *= 2) {
            uint256 profit = profitAt(plan, amount);
            if (profit > best) (best, rival.amount) = (profit, amount);
        }
        rival.minProfit = 1;
        rival.deadline = plan.deadline;
        vm.prank(RIVAL);
        executor.executePacked(PackedTestPlan.encode(rival));
    }

    function testPlainPacked() public {
        if (skip) return;
        (bool ok,, uint256 used) = run(plain);
        emit GasMeasured(string.concat(name(), lost() ? "_lost_plain" : "_success_plain"), used);
        require(ok != lost(), lost() ? "route survived the rival" : "quoted route failed");
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

    function testHotRoute() public {
        if (skip) return;
        vm.prank(OPERATOR);
        (bool ok, bytes memory result, uint256 used) =
            CallMeter.call(address(executor), abi.encodeCall(executor.executeHot, (hot)));
        emit GasMeasured(string.concat(name(), lost() ? "_lost_hot" : "_success_hot"), used);
        if (!lost()) {
            require(ok, "quoted route failed");
            return;
        }
        require(!ok && bytes4(result) == PriceCheck.PoolMoved.selector, "rival move not caught");
    }

    function testMinimalReference() public {
        if (skip || lost()) return;
        T.Plan memory plan = route();
        MinimalV3Cycle floor = new MinimalV3Cycle();
        MinimalV3Cycle.Leg[] memory legs = new MinimalV3Cycle.Leg[](plan.steps.length);
        address tokenIn = plan.asset;
        for (uint256 i; i < legs.length; ++i) {
            legs[i] = MinimalV3Cycle.Leg(plan.steps[i].target, tokenIn < plan.steps[i].tokenOut, tokenIn);
            tokenIn = plan.steps[i].tokenOut;
        }
        (bool ok,, uint256 used) = CallMeter.call(address(floor), abi.encodeCall(floor.run, (legs, plan.amount)));
        emit GasMeasured(string.concat(name(), "_success_minimal"), used);
        require(ok, "reference route failed");
    }

    function run(bytes memory encoded) private returns (bool, bytes memory, uint256) {
        vm.prank(OPERATOR);
        return CallMeter.call(address(executor), abi.encodeCall(executor.executePacked, (encoded)));
    }

    /// @dev What the route's last pool takes in: the quote the bot sends as
    /// the hot route's exact input.
    function lastInput(T.Plan memory plan) private returns (uint256 amount) {
        uint256 snapshot = vm.snapshotState();
        amount = plan.amount;
        address tokenIn = plan.asset;
        for (uint256 i; i + 1 < plan.steps.length; ++i) {
            address tokenOut = plan.steps[i].tokenOut;
            uint256 held = IArbForkToken(tokenOut).balanceOf(address(this));
            sellV3(plan.steps[i].target, tokenIn, tokenOut, amount);
            amount = IArbForkToken(tokenOut).balanceOf(address(this)) - held;
            tokenIn = tokenOut;
        }
        vm.revertToState(snapshot);
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

    function step(address target, address tokenOut) internal pure returns (T.Step memory) {
        return T.Step(T.V3, target, tokenOut, 0, 0, 1, 0);
    }

    function flashPlan(uint256 amount, uint256 steps) internal pure returns (T.Plan memory plan) {
        plan.funding = T.Funding.V3FlashSwap;
        plan.asset = WETH;
        plan.amount = amount;
        plan.steps = new T.Step[](steps);
    }

    function sellV3(address pool, address tokenIn, address tokenOut, uint256 amount) internal {
        bool zeroForOne = tokenIn < tokenOut;
        IArbForkV3Pool(pool)
            .swap(
                address(this),
                zeroForOne,
                int256(amount),
                zeroForOne ? MIN_PRICE + 1 : MAX_PRICE - 1,
                abi.encode(tokenIn)
            );
    }

    function uniswapV3SwapCallback(int256 amount0, int256 amount1, bytes calldata payload) external {
        IArbForkToken(abi.decode(payload, (address))).transfer(msg.sender, uint256(amount0 > 0 ? amount0 : amount1));
    }
}

// A1: two Uniswap V3 WETH/USDC pools. A victim sells WETH into the 0.3% pool;
// we sell WETH on the 0.05% pool and buy it back there.
abstract contract ArbRouteTwo is ArbForkRoute {
    function name() internal pure override returns (string memory) {
        return "a1_v3_v3";
    }

    function route() internal pure override returns (T.Plan memory plan) {
        plan = flashPlan(0.05 ether, 2);
        plan.steps[0] = step(UNI_WETH_USDC_500, USDC);
        plan.steps[1] = step(UNI_WETH_USDC_3000, WETH);
    }

    function victim(uint256 amount) internal override {
        sellV3(UNI_WETH_USDC_3000, WETH, USDC, amount);
    }

    function trigger() internal pure override returns (uint8) {
        return 1;
    }
}

// A2: WETH -> USDT -> USDC -> WETH on Uniswap V3. A victim sells WETH into
// the WETH/USDC 0.05% pool, our last leg.
abstract contract ArbRouteThree is ArbForkRoute {
    function name() internal pure override returns (string memory) {
        return "a2_v3_v3_v3";
    }

    function route() internal pure override returns (T.Plan memory plan) {
        plan = flashPlan(0.05 ether, 3);
        plan.steps[0] = step(UNI_WETH_USDT_500, USDT);
        plan.steps[1] = step(UNI_USDC_USDT_100, USDC);
        plan.steps[2] = step(UNI_WETH_USDC_500, WETH);
    }

    function victim(uint256 amount) internal override {
        sellV3(UNI_WETH_USDC_500, WETH, USDC, amount);
    }

    function trigger() internal pure override returns (uint8) {
        return 2;
    }
}

contract ArbRouteTwoSuccessGasForkTest is ArbRouteTwo {
    function lost() internal pure override returns (bool) {
        return false;
    }
}

contract ArbRouteTwoLostGasForkTest is ArbRouteTwo {
    function lost() internal pure override returns (bool) {
        return true;
    }
}

contract ArbRouteThreeSuccessGasForkTest is ArbRouteThree {
    function lost() internal pure override returns (bool) {
        return false;
    }
}

contract ArbRouteThreeLostGasForkTest is ArbRouteThree {
    function lost() internal pure override returns (bool) {
        return true;
    }
}
