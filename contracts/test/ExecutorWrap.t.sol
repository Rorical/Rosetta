// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {TestToken, TestV3Pool} from "./PoolMocks.sol";
import {TestV4Manager} from "./V4Mock.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";
import {SwapExecutor} from "../src/SwapExecutor.sol";

/// @dev Pays ether on from its own address, as a contract other than the
/// wrapped token would.
contract EtherRelay {
    function forward(address recipient) external payable {
        (bool ok,) = recipient.call{value: msg.value}("");
        require(ok, "relay payment");
    }
}

/// @dev WETH9's deposit and withdraw on the test token.
contract TestWrapped is TestToken {
    EtherRelay public immutable relay = new EtherRelay();
    // 1: a foreign contract pays the withdrawer one wei first; 2: withdraw
    // sends one wei less; 3: deposit credits one unit less.
    uint8 public mode;
    bool public foreignRejected;

    function setMode(uint8 value) external {
        mode = value;
    }

    function deposit() external payable {
        balanceOf[msg.sender] += mode == 3 ? msg.value - 1 : msg.value;
    }

    function withdraw(uint256 amount) external {
        balanceOf[msg.sender] -= amount;
        if (mode == 1) {
            (bool paid,) = address(relay).call{value: 1}(abi.encodeCall(EtherRelay.forward, (msg.sender)));
            foreignRejected = !paid;
        }
        (bool ok,) = msg.sender.call{value: mode == 2 ? amount - 1 : amount}("");
        require(ok, "native payment");
    }
}

contract ExecutorWrapTest is ExecutorFixture {
    TestWrapped private weth;
    TestToken private usd;
    TestV3Pool private pool;
    TestV4Manager private manager;
    bytes32 private v3;
    bytes32 private v4;
    bytes32 private wrapper;

    function setUp() public override {
        super.setUp();
        weth = new TestWrapped();
        usd = token0;
        vm.deal(address(weth), 1_000_000);
        weth.mint(OPERATOR, 100_000);
        vm.prank(OPERATOR);
        weth.approve(address(executor), type(uint256).max);
        wrapper = registerPool(T.WRAP, address(weth), address(0), address(weth), 0, 0);
    }

    /// @dev rate is the output per million of input, for both directions.
    function markets(uint256 v3Rate, uint256 v4Rate) private {
        (address a, address b) =
            address(weth) < address(usd) ? (address(weth), address(usd)) : (address(usd), address(weth));
        pool = new TestV3Pool(a, b, v3Rate, 0);
        weth.mint(address(pool), 10_000_000);
        usd.mint(address(pool), 10_000_000);
        manager = new TestV4Manager(v4Rate);
        usd.mint(address(manager), 1_000_000);
        vm.deal(address(manager), 1_000_000);
        v3 = registerPool(T.V3, address(pool), a, b, 500, 10);
        v4 = registerPool(T.V4, address(manager), address(0), address(usd), 500, 10);
    }

    /// @dev WETH -> V3 -> USD -> V4 -> ETH -> deposit -> WETH: 1000 in, 1200 out.
    function wrapPlan() private returns (T.Plan memory plan) {
        markets(2_000_000, 600_000);
        plan.asset = address(weth);
        plan.amount = 1000;
        plan.steps = new T.Step[](3);
        plan.steps[0] = step(v3, address(weth), 2000, 0);
        plan.steps[1] = step(v4, address(usd), 1200, 0);
        plan.steps[2] = step(wrapper, address(0), 1200, 0);
        plan.minProfit = 100;
        plan.deadline = block.timestamp + 60;
    }

    /// @dev WETH -> withdraw -> ETH -> V4 -> USD -> V3 -> WETH: 1000 in, 1200 out.
    function unwrapPlan() private returns (T.Plan memory plan) {
        markets(600_000, 2_000_000);
        plan.asset = address(weth);
        plan.amount = 1000;
        plan.steps = new T.Step[](3);
        plan.steps[0] = step(wrapper, address(weth), 1000, 0);
        plan.steps[1] = step(v4, address(0), 2000, 0);
        plan.steps[2] = step(v3, address(usd), 1200, 0);
        plan.minProfit = 100;
        plan.deadline = block.timestamp + 60;
    }

    function assertSettled(uint256 profit) private view {
        assertEq(profit, 200);
        assertEq(weth.balanceOf(OPERATOR), 100_000);
        assertEq(weth.balanceOf(address(executor)), 200);
        assertEq(usd.balanceOf(address(executor)), 0);
        assertEq(address(executor).balance, 0);
    }

    function testNativeOutputIsWrappedToCloseTheRoute() public {
        T.Plan memory plan = wrapPlan();
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertSettled(profit);
        assertEq(address(weth).balance, 1_000_000 + 1200);
        assertEq(address(manager).balance, 1_000_000 - 1200);
    }

    function testWrappedAssetIsUnwrappedForANativeLeg() public {
        T.Plan memory plan = unwrapPlan();
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertSettled(profit);
        assertEq(address(weth).balance, 1_000_000 - 1000);
        assertEq(address(manager).balance, 1_000_000 + 1000);
    }

    function testPackedPlanCarriesWrapSteps() public {
        T.Plan memory plan = wrapPlan();
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.executePacked(PackedTestPlan.encode(plan));
        assertSettled(profit);
        plan = unwrapPlan();
        plan.funding = T.Funding.Inventory;
        weth.mint(address(executor), 1000);
        vm.prank(OPERATOR);
        (profit,) = executor.executePacked(PackedTestPlan.encode(plan));
        assertEq(profit, 200);
        assertEq(weth.balanceOf(address(executor)), 200 + 1000 + 200);
        assertEq(address(executor).balance, 0);
    }

    function testFlashSwapTailWrapsNativeOutput() public {
        T.Plan memory plan = wrapPlan();
        plan.funding = T.Funding.V3FlashSwap;
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertSettled(profit);
    }

    function testFlashLoanRouteUnwrapsAndWraps() public {
        T.Plan memory plan = unwrapPlan();
        TestV3Pool lender = new TestV3Pool(pool.token0(), pool.token1(), 1_000_000, 0);
        weth.mint(address(lender), 10_000_000);
        plan.funding = T.Funding.V3Flash;
        plan.maxLoanFee = 5;
        plan.lender = address(lender);
        plan.lenderToken0 = lender.token0() == address(weth);
        vm.prank(OPERATOR);
        (uint256 profit, uint256 fee) = executor.execute(plan);
        assertEq(fee, 5);
        assertEq(profit, 195);
        assertEq(address(executor).balance, 0);
    }

    function testWrapStepMustPairNativeWithItsTarget() public {
        // The target is not the wrapped side.
        T.Plan memory plan = wrapPlan();
        plan.steps[2].target = address(token1);
        expectInvalidRoute(plan);
        plan = unwrapPlan();
        plan.steps[0].target = address(token1);
        expectInvalidRoute(plan);
        // Neither side is native currency.
        plan = wrapPlan();
        plan.steps[0].kind = T.WRAP;
        plan.steps[0].target = address(weth);
        expectInvalidRoute(plan);
        plan.steps[0].target = address(usd);
        expectInvalidRoute(plan);
        // A wrap takes no price limit.
        plan = wrapPlan();
        plan.steps[2].sqrtPriceLimitX96 = 4295128740;
        expectInvalidRoute(plan);
        // Native currency stays out of every other kind, and kind 10 is unknown.
        plan = wrapPlan();
        plan.steps[2].kind = T.V3;
        expectInvalidRoute(plan);
        plan.steps[2].kind = T.WRAP + 1;
        expectInvalidRoute(plan);
        assertEq(weth.balanceOf(OPERATOR), 100_000);
    }

    function expectInvalidRoute(T.Plan memory plan) private {
        vm.expectRevert(SwapExecutor.InvalidRoute.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }

    function testWrapCannotStartAFlashSwapOrJoinAV4Flash() public {
        T.Plan memory plan = unwrapPlan();
        plan.funding = T.Funding.V3FlashSwap;
        expectInvalidRoute(plan);
        plan.funding = T.Funding.V4Flash;
        expectInvalidRoute(plan);
    }

    function testUnsolicitedEtherIsRejected() public {
        vm.deal(address(this), 1000);
        (bool ok,) = address(executor).call{value: 100}("");
        require(!ok, "accepted foreign native transfer");
        // The wrapped token is accepted only while its withdrawal runs.
        T.Plan memory plan = unwrapPlan();
        vm.prank(OPERATOR);
        executor.execute(plan);
        vm.prank(address(weth));
        (ok,) = address(executor).call{value: 100}("");
        require(!ok, "accepted the wrapped token outside a withdrawal");
        vm.deal(address(manager), 1000);
        vm.prank(address(manager));
        (ok,) = address(executor).call{value: 100}("");
        require(!ok, "accepted the manager outside an unlock");
    }

    function testEtherFromAnotherContractDuringWithdrawIsRejected() public {
        T.Plan memory plan = unwrapPlan();
        weth.setMode(1);
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        require(weth.foreignRejected(), "accepted a foreign payer during the withdrawal");
        assertSettled(profit);
    }

    function testShortWithdrawAndShortDepositRevert() public {
        T.Plan memory plan = unwrapPlan();
        weth.setMode(2);
        vm.expectRevert();
        vm.prank(OPERATOR);
        executor.execute(plan);
        // The executor's own ether must not cover the missing wei.
        vm.deal(address(executor), 10);
        vm.expectRevert(SwapExecutor.InvalidSwap.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);

        plan = wrapPlan();
        weth.setMode(3);
        // The route's last output is still 1200; the asset balance is one short.
        plan.minProfit = 200;
        vm.expectRevert(SwapExecutor.InsufficientProfit.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
        assertEq(weth.balanceOf(OPERATOR), 100_000);
    }

    function testRevertingWrapCallReverts() public {
        T.Plan memory plan = unwrapPlan();
        // More than the executor holds: the token's withdraw reverts.
        plan.funding = T.Funding.Inventory;
        vm.expectRevert(SwapExecutor.InvalidSwap.selector);
        vm.prank(OPERATOR);
        executor.execute(plan);
    }
}
