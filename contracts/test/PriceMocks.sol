// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {TestV3Pool, MockPayments} from "./PoolMocks.sol";
import {NettingV4Manager} from "./V4NettingMock.sol";

// Synthetic pools whose swap rate is fixed and whose readable price is set by
// the test: a precheck reads the price, the swap ignores it.

/// @dev Uniswap/Pancake/Slipstream/Solidly slot0 and Algebra globalState all
/// return the price in their first word.
contract PricedV3Pool is TestV3Pool {
    uint160 public price;

    constructor(address a, address b, uint256 rate_, uint8 callbackKind_) TestV3Pool(a, b, rate_, callbackKind_) {}

    function setPrice(uint160 value) external {
        price = value;
    }

    function slot0() external view returns (uint160, int24, uint16, uint16, uint16, uint8, bool) {
        return (price, -1, 0, 0, 0, 0, true);
    }

    function globalState() external view returns (uint160, int24, uint16, uint16, uint16, uint8, uint8, bool) {
        return (price, -1, 0, 0, 0, 0, 0, true);
    }
}

/// @dev Velodrome V2 returns three uint256 words from getReserves and quotes
/// through getAmountOut; the precheck reads only the reserves.
contract PricedVelodromePool {
    address public token0;
    address public token1;
    uint256 public rate = 2_000_000;
    uint256 private reserve0;
    uint256 private reserve1;

    constructor(address a, address b) {
        token0 = a;
        token1 = b;
    }

    function setReserves(uint256 value0, uint256 value1) external {
        reserve0 = value0;
        reserve1 = value1;
    }

    function getReserves() external view returns (uint256, uint256, uint256) {
        return (reserve0, reserve1, block.timestamp);
    }

    function getAmountOut(uint256 amount, address token) external view returns (uint256) {
        require(token == token0 || token == token1, "token");
        return amount * rate / 1_000_000;
    }

    function swap(uint256 out0, uint256 out1, address to, bytes calldata) external {
        if (out0 != 0) MockPayments.send(token0, to, out0);
        if (out1 != 0) MockPayments.send(token1, to, out1);
    }
}

/// @dev Serves extsload for the slots a test writes, as PoolManager exposes
/// its pool state to StateLibrary readers.
contract PricedV4Manager is NettingV4Manager {
    mapping(bytes32 => bytes32) private words;

    function setWord(bytes32 slot, bytes32 value) external {
        words[slot] = value;
    }

    function extsload(bytes32 slot) external view returns (bytes32) {
        return words[slot];
    }
}

/// @dev A pool that answers the price read with too little data.
contract ShortSlot0Pool {
    function slot0() external pure {
        assembly { return(0, 16) }
    }
}
