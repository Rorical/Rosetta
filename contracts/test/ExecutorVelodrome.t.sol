// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {TestToken, MockPayments} from "./PoolMocks.sol";

// ABI fixture deliberately has no V2 getReserves: stable/volatile pricing and
// live factory fees must be delegated to the admitted pool's native quoter.
contract VelodromeQuoteFixture {
    address public token0;
    address public token1;
    uint256 public rate = 2_000_000;

    constructor(address a, address b) {
        token0 = a;
        token1 = b;
    }

    function setRate(uint256 value) external {
        rate = value;
    }

    function getAmountOut(uint256 amount, address token) external view returns (uint256) {
        require(token == token0 || token == token1);
        return amount * rate / 1_000_000;
    }

    function swap(uint256 a, uint256 b, address to, bytes calldata) external {
        require(TestToken(token0).balanceOf(address(this)) >= 1000, "input missing");
        if (a != 0) MockPayments.send(token0, to, a);
        if (b != 0) MockPayments.send(token1, to, b);
    }
}

contract ExecutorVelodromeTest is ExecutorFixture {
    function testNativeQuoteAndFeeChange() public {
        VelodromeQuoteFixture first = new VelodromeQuoteFixture(address(token0), address(token1));
        token1.mint(address(first), 1_000_000);
        bytes32 a = register(6, address(first));
        bytes32 b = register(T.V3, address(v3Pool(600000, 0)));
        T.Plan memory plan = walletPlan(a, b, address(token0), address(token1));
        vm.prank(OPERATOR);
        (uint256 profit,) = executor.execute(plan);
        assertEq(profit, 200);
        first.setRate(1_000_000);
        vm.expectRevert();
        vm.prank(OPERATOR);
        executor.execute(plan);
    }
}
