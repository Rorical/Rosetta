// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";
import {TestToken, MockPayments} from "./PoolMocks.sol";

interface IUnlockReceiver {
    function unlockCallback(bytes calldata) external returns (bytes memory);
}

// Models V4's signed caller deltas, sync-before-pay and unlock settlement boundary.
contract TestV4Manager {
    uint256 public rate;
    uint8 public mode;
    address private locker;
    address private input;
    address private output;
    uint256 private debt;
    uint256 private credit;
    uint256 private syncedBalance;
    bool private synced;

    constructor(uint256 rate_) {
        rate = rate_;
    }
    receive() external payable {}

    function setMode(uint8 value) external {
        mode = value;
    }

    function unlock(bytes calldata payload) external returns (bytes memory result) {
        require(locker == address(0), "locked");
        locker = msg.sender;
        if (mode != 1) result = IUnlockReceiver(msg.sender).unlockCallback(mode == 2 ? bytes("foreign") : payload);
        if (mode == 3) IUnlockReceiver(msg.sender).unlockCallback(payload);
        require(debt == 0 && credit == 0, "unsettled");
        locker = address(0);
    }

    function swap(T.PoolKey calldata key, T.SwapParams calldata params, bytes calldata hooks)
        external
        returns (int256 delta)
    {
        require(msg.sender == locker && key.hooks == address(0) && hooks.length == 0, "permission");
        require(params.amountSpecified < 0, "exact input must be negative");
        debt = uint256(-params.amountSpecified);
        if (mode == 4) --debt;
        credit = debt * rate / 1_000_000;
        input = params.zeroForOne ? key.currency0 : key.currency1;
        output = params.zeroForOne ? key.currency1 : key.currency0;
        require(debt <= uint256(uint128(type(int128).max)) && credit <= uint256(uint128(type(int128).max)), "bounds");
        (int128 amount0, int128 amount1) = params.zeroForOne
            ? (-int128(uint128(debt)), int128(uint128(credit)))
            : (int128(uint128(credit)), -int128(uint128(debt)));
        delta = (int256(amount0) << 128) | int256(uint256(uint128(amount1)));
    }

    function sync(address token) external {
        require(msg.sender == locker && token == input, "wrong currency");
        synced = true;
        syncedBalance = token == address(0) ? 0 : TestToken(token).balanceOf(address(this));
    }

    function settle() external payable returns (uint256 paid) {
        require(msg.sender == locker && synced, "must sync first");
        if (input == address(0)) {
            paid = msg.value;
        } else {
            require(msg.value == 0, "unexpected value");
            paid = TestToken(input).balanceOf(address(this)) - syncedBalance;
        }
        require(paid == debt, "wrong repayment");
        debt = 0;
        synced = false;
        if (mode == 5) --paid;
    }

    function take(address token, address recipient, uint256 amount) external {
        require(msg.sender == locker && token == output && amount == credit, "wrong take");
        credit = 0;
        if (mode == 6) --amount;
        if (token == address(0)) {
            (bool ok,) = recipient.call{value: amount}("");
            require(ok, "native payment");
        } else {
            MockPayments.send(token, recipient, amount);
        }
    }
}
