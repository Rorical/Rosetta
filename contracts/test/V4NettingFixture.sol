// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {NettingV4Manager} from "./V4NettingMock.sol";

abstract contract V4NettingFixture is ExecutorFixture {
    NettingV4Manager internal manager;
    bytes internal encodedCycle;
    bytes internal encodedFlashTail;
    bytes internal encodedSeparatedCycle;

    function setUp() public virtual override {
        super.setUp();
        manager = new NettingV4Manager();
        token0.mint(address(manager), 10_000_000);
        token1.mint(address(manager), 10_000_000);
        vm.deal(address(manager), 10_000_000);
        bytes32 first = pool(address(token0), address(token1), 3000, 2_000_000);
        bytes32 second = pool(address(token0), address(token1), 500, 600_000);
        T.Plan memory cycle = walletPlan(first, second, address(token0), address(token1));
        bytes32 v3 = register(T.V3, address(v3Pool(2_000_000, 0)));
        bytes32 middle = pool(address(0), address(token1), 3000, 1_000_000);
        bytes32 last = pool(address(0), address(token0), 3000, 600_000);
        T.Plan memory tail = walletPlan(v3, v3, address(token0), address(token1));
        tail.funding = T.Funding.V3FlashSwap;
        tail.steps = new T.Step[](3);
        tail.steps[0] = step(v3, address(token0), 1900, 0);
        tail.steps[1] = step(middle, address(token1), 1900, 0);
        tail.steps[2] = step(last, address(0), 1100, 0);
        NettingV4Manager other = new NettingV4Manager();
        token0.mint(address(other), 10_000_000);
        token1.mint(address(other), 10_000_000);
        bytes32 id = keccak256(abi.encode(T.PoolKey(address(token0), address(token1), 500, 60, address(0))));
        other.setRate(id, 600_000);
        T.Plan memory separated = abi.decode(abi.encode(cycle), (T.Plan));
        separated.steps[1].target = address(other);
        encodedCycle = abi.encode(cycle);
        encodedFlashTail = abi.encode(tail);
        encodedSeparatedCycle = abi.encode(separated);
    }

    function pool(address a, address b, uint24 fee, uint256 rate) internal returns (bytes32) {
        if (a > b) (a, b) = (b, a);
        bytes32 id = keccak256(abi.encode(T.PoolKey(a, b, fee, 60, address(0))));
        manager.setRate(id, rate);
        return registerPool(T.V4, address(manager), a, b, fee, 60);
    }

    function cyclePlan() internal view returns (T.Plan memory) {
        return abi.decode(encodedCycle, (T.Plan));
    }

    function flashTailPlan() internal view returns (T.Plan memory) {
        return abi.decode(encodedFlashTail, (T.Plan));
    }
}
