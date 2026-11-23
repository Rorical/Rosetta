// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ArbitrageExecutor} from "../src/ArbitrageExecutor.sol";
import {ExecutorTypes as T, IV4Manager} from "../src/ExecutorTypes.sol";

interface WrapForkVm {
    function envOr(string calldata, string calldata) external view returns (string memory);
    function envUint(string calldata) external view returns (uint256);
    function createSelectFork(string calldata, uint256) external returns (uint256);
    function deal(address, uint256) external;
    function prank(address) external;
}

interface IWrapForkToken {
    function deposit() external payable;
    function approve(address, uint256) external returns (bool);
    function balanceOf(address) external view returns (uint256);
}

interface IWrapForkManager {
    function extsload(bytes32) external view returns (bytes32);
}

/// @dev The WRAP leg against Arbitrum One's WETH and the deployed Uniswap V4
/// PoolManager (ROSETTA_ARB_FORK_URL, ROSETTA_ARB_FORK_BLOCK): WETH -> Uniswap
/// V3 WETH/USDT -> USDT -> V4 native ETH/USDT -> ETH -> deposit -> WETH. A
/// victim's ETH sale into the V4 pool opens the gap, so the route closes with
/// a profit at any block; the swaps, the native take and the deposit are the
/// deployed code.
contract ExecutorWrapForkTest {
    WrapForkVm private constant vm = WrapForkVm(address(uint160(uint256(keccak256("hevm cheat code")))));
    address private constant WETH = 0x82aF49447D8a07e3bd95BD0d56f35241523fBab1;
    address private constant USDT = 0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9;
    address private constant V3 = 0x641C00A822e8b671738d32a431a4Fb6074E5c79d;
    address private constant MANAGER = 0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32;
    address private constant OPERATOR = address(0x1234);
    uint256 private constant POOLS_SLOT = 6;

    ArbitrageExecutor private executor;
    bool private skip;
    uint24 private fee;
    int24 private spacing;

    receive() external payable {}

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
        IWrapForkToken(WETH).deposit{value: 5 ether}();
        vm.prank(OPERATOR);
        IWrapForkToken(WETH).approve(address(executor), type(uint256).max);
        // The catalog's V4 tiers, most liquid pair first.
        uint24[3] memory fees = [uint24(500), 3000, 100];
        int24[3] memory spacings = [int24(10), 60, 1];
        for (uint256 i; i < 3 && fee == 0; ++i) {
            if (liquidity(fees[i], spacings[i]) != 0) (fee, spacing) = (fees[i], spacings[i]);
        }
        require(fee != 0, "no native ETH/USDT V4 pool with liquidity");
    }

    function poolSlot(uint24 fee_, int24 spacing_) private pure returns (bytes32) {
        bytes32 id = keccak256(abi.encode(T.PoolKey(address(0), USDT, fee_, spacing_, address(0))));
        return keccak256(abi.encode(id, POOLS_SLOT));
    }

    function liquidity(uint24 fee_, int24 spacing_) private view returns (uint128) {
        // Pool.State: slot0, two fee growth words, then liquidity.
        return uint128(uint256(IWrapForkManager(MANAGER).extsload(bytes32(uint256(poolSlot(fee_, spacing_)) + 3))));
    }

    /// @dev Sells ETH into the V4 pool until its price is 2% lower.
    function sellNative() private {
        vm.deal(address(this), 1000 ether);
        IV4Manager(MANAGER).unlock("");
    }

    function unlockCallback(bytes calldata) external returns (bytes memory) {
        require(msg.sender == MANAGER, "foreign unlock");
        uint160 price = uint160(uint256(IWrapForkManager(MANAGER).extsload(poolSlot(fee, spacing))));
        int256 delta = IV4Manager(MANAGER)
            .swap(
                T.PoolKey(address(0), USDT, fee, spacing, address(0)),
                T.SwapParams(true, -int256(1000 ether), price - price / 100),
                ""
            );
        IV4Manager(MANAGER).sync(address(0));
        IV4Manager(MANAGER).settle{value: uint256(-int256(int128(delta >> 128)))}();
        IV4Manager(MANAGER).take(USDT, address(this), uint256(int256(int128(delta))));
        return "";
    }

    function testClosesANativeV4RouteByWrapping() public {
        if (skip) return;
        sellNative();
        uint256 amount = 0.01 ether;
        T.Plan memory plan;
        plan.asset = WETH;
        plan.amount = amount;
        plan.steps = new T.Step[](3);
        plan.steps[0] = T.Step(T.V3, V3, USDT, 0, 0, 1, 0);
        plan.steps[1] = T.Step(T.V4, MANAGER, address(0), fee, spacing, 1, 0);
        plan.steps[2] = T.Step(T.WRAP, WETH, WETH, 0, 0, 1, 0);
        plan.minProfit = 1;
        plan.deadline = block.timestamp + 60;
        uint256 kept = IWrapForkToken(WETH).balanceOf(address(executor));
        uint256 wallet = IWrapForkToken(WETH).balanceOf(OPERATOR);
        // The test's deployment address can hold dust on the forked chain.
        uint256 dust = IWrapForkToken(USDT).balanceOf(address(executor));
        uint256 nativeDust = address(executor).balance;
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        // The V4 price is 2% below the V3 price before fees of at most 0.35%.
        require(profit > amount / 100 && profit < amount * 3 / 100, "profit outside the opened gap");
        require(IWrapForkToken(WETH).balanceOf(address(executor)) == kept + profit, "output minus input not kept");
        require(IWrapForkToken(WETH).balanceOf(OPERATOR) == wallet, "principal not returned");
        require(address(executor).balance == nativeDust, "ether left in the executor");
        require(IWrapForkToken(USDT).balanceOf(address(executor)) == dust, "USDT left in the executor");
    }
}
