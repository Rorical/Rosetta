// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ArbitrageExecutor} from "../src/ArbitrageExecutor.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {TestToken, TestV3Pool} from "./PoolMocks.sol";
import {CallMeter} from "./CallMeter.sol";

interface Vm {
    function chainId(uint256) external;
    function deal(address, uint256) external;
    function prank(address) external;
    function expectRevert() external;
    function expectRevert(bytes4) external;
    function expectRevert(bytes calldata) external;
    function warp(uint256) external;
    function etch(address, bytes calldata) external;
    function mockCall(address, bytes calldata, bytes calldata) external;
    function mockCallRevert(address, bytes calldata, bytes calldata) external;
    function clearMockedCalls() external;
    function expectCall(address, bytes calldata, uint64) external;
    function snapshotState() external returns (uint256);
    function revertToState(uint256) external returns (bool);
    function cool(address) external;
    function load(address, bytes32) external view returns (bytes32);
}

abstract contract ExecutorFixture {
    Vm internal constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));
    ArbitrageExecutor internal executor;
    TestToken internal token0;
    TestToken internal token1;
    address internal constant OPERATOR = address(0x1234);

    // The executor keeps no pool registry. Tests keep pool parameters here and
    // refer to them by handle when building route steps.
    struct TestPool {
        uint8 kind;
        address target;
        address token0;
        address token1;
        uint24 fee;
        int24 tickSpacing;
    }

    mapping(bytes32 => TestPool) internal testPools;

    receive() external payable {}

    function setUp() public virtual {
        vm.chainId(42161);
        TestToken a = new TestToken();
        TestToken b = new TestToken();
        (token0, token1) = address(a) < address(b) ? (a, b) : (b, a);
        executor = new ArbitrageExecutor(address(this), hotOperators());
        executor.setOperator(OPERATOR, true);
        executor.setPaused(false);
        token0.mint(OPERATOR, 100_000);
        token1.mint(OPERATOR, 100_000);
        vm.prank(OPERATOR);
        token0.approve(address(executor), type(uint256).max);
        vm.prank(OPERATOR);
        token1.approve(address(executor), type(uint256).max);
    }

    /// @dev executeHot's senders: the operator, and token1 so that a token
    /// hook can try to reenter.
    function hotOperators() internal view returns (address[2] memory hot) {
        hot[0] = OPERATOR;
        hot[1] = address(token1);
    }

    function register(uint8 kind, address target) internal returns (bytes32) {
        return registerPool(kind, target, address(token0), address(token1), 3000, 60);
    }

    function registerPool(uint8 kind, address target, address a, address b, uint24 fee, int24 spacing)
        internal
        returns (bytes32 handle)
    {
        handle = keccak256(abi.encode(kind, target, a, b, fee, spacing));
        testPools[handle] = TestPool(kind, target, a, b, fee, spacing);
    }

    function step(bytes32 handle, address tokenIn, uint256 minOut, uint160 limit)
        internal
        view
        returns (T.Step memory)
    {
        TestPool memory pool = testPools[handle];
        require(pool.target != address(0), "unknown test pool");
        require(tokenIn == pool.token0 || tokenIn == pool.token1, "token not in test pool");
        address tokenOut = tokenIn == pool.token0 ? pool.token1 : pool.token0;
        return T.Step(pool.kind, pool.target, tokenOut, pool.fee, pool.tickSpacing, minOut, limit);
    }

    function lend(T.Plan memory plan, bytes32 handle) internal view {
        TestPool memory pool = testPools[handle];
        require(pool.target != address(0), "unknown test pool");
        plan.lender = pool.target;
        plan.lenderToken0 = plan.asset == pool.token0;
    }

    function v3Pool(uint256 rate, uint8 callbackKind) internal returns (TestV3Pool pool) {
        pool = new TestV3Pool(address(token0), address(token1), rate, callbackKind);
        token0.mint(address(pool), 10_000_000);
        token1.mint(address(pool), 10_000_000);
    }

    function walletPlan(bytes32 first, bytes32 second, address asset, address middle)
        internal
        view
        returns (T.Plan memory plan)
    {
        plan.asset = asset;
        plan.amount = 1000;
        plan.steps = new T.Step[](2);
        plan.steps[0] = step(first, asset, 1900, 0);
        plan.steps[1] = step(second, middle, 1100, 0);
        plan.minProfit = 100;
        plan.deadline = block.timestamp + 60;
    }

    function assertEq(uint256 actual, uint256 expected) internal pure {
        require(actual == expected, "unequal");
    }

    /// @dev Route profit stays in the executor until the owner sweeps it.
    function assertKept(TestToken token, uint256 expected) internal view {
        assertEq(token.balanceOf(address(this)), 0);
        assertEq(token.balanceOf(address(executor)), expected);
    }

    function measuredCall(address target, bytes memory payload)
        internal
        returns (bool ok, bytes memory result, uint256 used)
    {
        return CallMeter.call(target, payload);
    }
}
