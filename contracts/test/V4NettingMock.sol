// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {TestToken, MockPayments} from "./PoolMocks.sol";
import {IUnlockReceiver} from "./V4Mock.sol";

/// @dev Synthetic prices with per-currency transient deltas and an unlock-wide
/// zero-delta check. Unlike the single-swap mock, intermediate credit can be spent.
contract NettingV4Manager {
    mapping(bytes32 => uint256) public rates;
    uint8 public mode;
    address public disturbedToken;
    bytes private previousPayload;

    receive() external payable {}

    function setRate(bytes32 poolId, uint256 rate) external {
        rates[poolId] = rate;
    }

    function setMode(uint8 value) external {
        mode = value;
    }

    function setDisturbedToken(address token) external {
        disturbedToken = token;
    }

    function unlock(bytes calldata payload) external returns (bytes memory result) {
        address locker;
        assembly ("memory-safe") { locker := tload(0) }
        require(locker == address(0), "locked");
        assembly ("memory-safe") { tstore(0, caller()) }
        if (mode != 1) {
            bytes memory actual = mode == 2 ? bytes("foreign") : payload;
            if (mode == 8) {
                if (previousPayload.length == 0) previousPayload = payload;
                actual = previousPayload;
            }
            result = IUnlockReceiver(msg.sender).unlockCallback(actual);
        }
        if (mode == 3) IUnlockReceiver(msg.sender).unlockCallback(payload);
        uint256 unsettled;
        assembly ("memory-safe") { unsettled := tload(1) }
        require(unsettled == 0, "unsettled currencies");
        if (mode == 7) result = abi.encode(abi.decode(result, (uint256)) + 1);
        assembly ("memory-safe") { tstore(0, 0) }
    }

    function swap(T.PoolKey calldata key, T.SwapParams calldata params, bytes calldata hooks)
        external
        returns (int256 delta)
    {
        checkLocker();
        require(key.hooks == address(0) && hooks.length == 0 && params.amountSpecified < 0, "swap parameters");
        uint256 input = uint256(-params.amountSpecified);
        if (mode == 4) --input;
        uint256 output = input * rates[keccak256(abi.encode(key))] / 1_000_000;
        require(input <= uint256(uint128(type(int128).max)) && output <= uint256(uint128(type(int128).max)), "bounds");
        (int128 amount0, int128 amount1) = params.zeroForOne
            ? (-int128(uint128(input)), int128(uint128(output)))
            : (int128(uint128(output)), -int128(uint128(input)));
        account(key.currency0, amount0);
        account(key.currency1, amount1);
        if (disturbedToken != address(0)) TestToken(disturbedToken).mint(msg.sender, 1);
        delta = (int256(amount0) << 128) | int256(uint256(uint128(amount1)));
    }

    function sync(address currency) external {
        checkLocker();
        uint256 syncedBalance = currency == address(0) ? 0 : TestToken(currency).balanceOf(address(this));
        assembly ("memory-safe") {
            tstore(2, currency)
            tstore(3, syncedBalance)
            tstore(4, 1)
        }
    }

    function settle() external payable returns (uint256 paid) {
        checkLocker();
        address currency;
        uint256 syncedBalance;
        bool synced;
        assembly ("memory-safe") {
            currency := tload(2)
            syncedBalance := tload(3)
            synced := tload(4)
            tstore(4, 0)
        }
        require(synced, "sync first");
        if (currency == address(0)) {
            paid = msg.value;
        } else {
            require(msg.value == 0, "unexpected value");
            paid = TestToken(currency).balanceOf(address(this)) - syncedBalance;
        }
        account(currency, int256(paid));
        if (mode == 5) --paid;
    }

    function take(address currency, address recipient, uint256 amount) external {
        checkLocker();
        account(currency, -int256(amount));
        if (mode == 6) --amount;
        if (currency == address(0)) {
            (bool ok,) = recipient.call{value: amount}("");
            require(ok, "native payment");
        } else {
            MockPayments.send(currency, recipient, amount);
        }
    }

    function checkLocker() private view {
        address locker;
        assembly ("memory-safe") { locker := tload(0) }
        require(msg.sender == locker, "wrong locker");
    }

    function account(address currency, int256 delta) private {
        bytes32 slot = keccak256(abi.encode("netting.currency", currency));
        int256 beforeDelta;
        uint256 count;
        assembly ("memory-safe") {
            beforeDelta := tload(slot)
            count := tload(1)
        }
        int256 afterDelta = beforeDelta + delta;
        if (beforeDelta == 0 && afterDelta != 0) ++count;
        if (beforeDelta != 0 && afterDelta == 0) --count;
        assembly ("memory-safe") {
            tstore(slot, afterDelta)
            tstore(1, count)
        }
    }
}
