// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T, IV4Manager} from "./ExecutorTypes.sol";
import {SwapExecutor} from "./SwapExecutor.sol";
import {TokenActions} from "./TokenActions.sol";
import {ExecutionState as S} from "./ExecutionState.sol";

/// @dev Consecutive swaps on one manager share an unlock and settle only net deltas.
abstract contract V4SwapExecutor is SwapExecutor {
    uint8 private constant SINGLE_KIND = T.V4 | 64;

    struct Leg {
        T.PoolKey key;
        bool zeroForOne;
        uint160 limit;
        uint256 minimum;
    }

    struct Batch {
        Leg[] legs;
        uint256 amount;
        uint256 profitFloor;
        uint256 nonce;
    }

    struct Single {
        Leg leg;
        uint256 amount;
        uint256 profitFloor;
        uint256 nonce;
    }

    constructor(address owner_) SwapExecutor(owner_) {}

    /// @dev Ether arrives only from the V4 manager being unlocked or from the
    /// wrapped native token of a WRAP leg during its withdrawal (SwapExecutor.wrap).
    receive() external payable {
        if (!S.executing() || msg.sender != S.manager() || S.manager() == address(0)) revert InvalidCallback();
    }

    function runV4(T.RouteStep[] memory route, uint256 first, uint256 amount, uint256 requiredOutput)
        internal
        override
        returns (uint256 output, uint256 next)
    {
        address manager = route[first].target;
        next = first + 1;
        while (next < route.length && route[next].kind == T.V4 && route[next].target == manager) ++next;
        uint256 profitFloor = next == route.length ? requiredOutput : 0;
        if (next == first + 1) return (executeSingle(route[first], amount, profitFloor), next);
        Batch memory batch = Batch(new Leg[](next - first), amount, profitFloor, S.nextNonce());
        for (uint256 i = first; i < next; ++i) {
            batch.legs[i - first] = makeLeg(route[i]);
        }
        output = executeV4Batch(manager, batch);
    }

    function makeLeg(T.RouteStep memory step) private pure returns (Leg memory) {
        bool zeroForOne = step.tokenIn < step.tokenOut;
        return Leg(
            T.PoolKey(
                zeroForOne ? step.tokenIn : step.tokenOut,
                zeroForOne ? step.tokenOut : step.tokenIn,
                step.fee,
                step.tickSpacing,
                address(0)
            ),
            zeroForOne,
            step.limit,
            step.minimum
        );
    }

    function executeSingle(T.RouteStep memory step, uint256 amount, uint256 profitFloor)
        private
        returns (uint256 output)
    {
        // settle checks what the manager received; the plan's inventory check
        // covers what we took.
        output = requestUnlock(
            step.target, SINGLE_KIND, abi.encode(Single(makeLeg(step), amount, profitFloor, S.nextNonce()))
        );
        checkOutput(output, step.minimum, profitFloor);
    }

    function requestUnlock(address manager, uint8 kind, bytes memory payload) private returns (uint256) {
        S.startSwap(manager, kind, keccak256(payload));
        S.setManager(manager);
        bytes memory result = IV4Manager(manager).unlock(payload);
        S.setManager(address(0));
        if (S.swapPending()) revert InvalidCallback();
        return abi.decode(result, (uint256));
    }

    function executeV4Batch(address manager, Batch memory batch) private returns (uint256 output) {
        // Currencies that never leave the manager are covered by the plan's
        // inventory check, settled ones by settle's paid amount.
        output = requestUnlock(manager, T.V4, abi.encode(batch));
        checkOutput(output, batch.legs[batch.legs.length - 1].minimum, batch.profitFloor);
    }

    function unlockCallback(bytes calldata payload) external returns (bytes memory) {
        S.Swap memory current = S.swap();
        if (
            !S.executing() || current.target == address(0) || (current.kind != T.V4 && current.kind != SINGLE_KIND)
                || msg.sender != current.target || keccak256(payload) != current.dataHash
        ) revert InvalidCallback();
        S.consumeSwap();
        IV4Manager manager = IV4Manager(msg.sender);
        if (current.kind == SINGLE_KIND) {
            Single memory single = abi.decode(payload, (Single));
            uint256 received = swapLeg(manager, single.leg, single.amount, single.profitFloor);
            settle(manager, inputCurrency(single.leg), single.amount);
            manager.take(outputCurrency(single.leg), address(this), received);
            return abi.encode(received);
        }
        Batch memory batch = abi.decode(payload, (Batch));
        uint256 output = batch.amount;
        for (uint256 i; i < batch.legs.length; ++i) {
            output = swapLeg(manager, batch.legs[i], output, i + 1 == batch.legs.length ? batch.profitFloor : 0);
        }
        address tokenIn = inputCurrency(batch.legs[0]);
        address tokenOut = outputCurrency(batch.legs[batch.legs.length - 1]);
        if (tokenIn != tokenOut) {
            settle(manager, tokenIn, batch.amount);
            manager.take(tokenOut, address(this), output);
        } else if (output > batch.amount) {
            manager.take(tokenOut, address(this), output - batch.amount);
        } else if (output < batch.amount) {
            settle(manager, tokenIn, batch.amount - output);
        }
        return abi.encode(output);
    }

    function swapLeg(IV4Manager manager, Leg memory leg, uint256 amount, uint256 profitFloor)
        private
        returns (uint256 output)
    {
        if (amount > uint256(uint128(type(int128).max))) revert InvalidSwap();
        int256 delta = callSwap(manager, leg, amount);
        (int256 input, int256 received) = leg.zeroForOne
            ? (int256(int128(delta >> 128)), int256(int128(delta)))
            : (int256(int128(delta)), int256(int128(delta >> 128)));
        if (input != -int256(amount) || received <= 0) revert InvalidSwap();
        output = uint256(received);
        checkOutput(output, leg.minimum, profitFloor);
    }

    function callSwap(IV4Manager manager, Leg memory leg, uint256 amount) private returns (int256 delta) {
        bool complete;
        // Static PoolKey/SwapParams plus empty hookData: 324-byte calldata.
        // The buffer is temporary, and only the one-word BalanceDelta is copied.
        assembly ("memory-safe") {
            let buffer := mload(0x40)
            let key := mload(leg)
            let addressMask := 0xffffffffffffffffffffffffffffffffffffffff
            mstore(buffer, shl(224, 0xf3cd914c))
            mstore(add(buffer, 4), and(mload(key), addressMask))
            mstore(add(buffer, 36), and(mload(add(key, 32)), addressMask))
            mstore(add(buffer, 68), and(mload(add(key, 64)), 0xffffff))
            mstore(add(buffer, 100), signextend(2, mload(add(key, 96))))
            mstore(add(buffer, 132), 0)
            mstore(add(buffer, 164), iszero(iszero(mload(add(leg, 32)))))
            mstore(add(buffer, 196), sub(0, amount))
            mstore(add(buffer, 228), and(mload(add(leg, 64)), addressMask))
            mstore(add(buffer, 260), 288)
            mstore(add(buffer, 292), 0)
            if iszero(call(gas(), manager, 0, buffer, 324, buffer, 32)) {
                returndatacopy(buffer, 0, returndatasize())
                revert(buffer, returndatasize())
            }
            complete := iszero(lt(returndatasize(), 32))
            delta := mload(buffer)
        }
        if (!complete) revert InvalidSwap();
    }

    function settle(IV4Manager manager, address currency, uint256 amount) private {
        // Sync native too: a prior ERC20 sync must not affect native settlement.
        manager.sync(currency);
        uint256 paid;
        if (currency == address(0)) {
            paid = manager.settle{value: amount}();
        } else {
            TokenActions.payPool(currency, address(manager), amount);
            paid = manager.settle();
        }
        if (paid != amount) revert InvalidSwap();
    }

    function inputCurrency(Leg memory leg) private pure returns (address) {
        return leg.zeroForOne ? leg.key.currency0 : leg.key.currency1;
    }

    function outputCurrency(Leg memory leg) private pure returns (address) {
        return leg.zeroForOne ? leg.key.currency1 : leg.key.currency0;
    }
}
