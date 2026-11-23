// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

/// @dev Transaction-local callback authorization (EIP-1153). Swap arguments
/// travel in the authenticated payload instead of occupying transient slots.
library ExecutionState {
    uint256 private constant LOCK = 0;
    // The one address that may send ether now: a V4 manager inside its unlock
    // or a wrapped native token inside its withdrawal.
    uint256 private constant MANAGER = 1;
    uint256 private constant NONCE = 2;
    uint256 private constant SWAP = 3; // Packed target/kind, then payload hash.
    uint256 private constant LOAN = 5; // Five words, slots 5..9.
    uint256 private constant FLASH_SWAP_RESULT = 10;

    struct Swap {
        address target; // Zero means no callback is pending.
        uint8 kind;
        bytes32 dataHash;
    }

    struct Loan {
        address lender;
        bool token0;
        bytes32 dataHash;
        uint256 baseline;
        uint256 status; // 1 awaiting callback, 2 inside callback, 3 repaid.
        uint256 fee;
    }

    function executing() internal view returns (bool value) {
        assembly ("memory-safe") { value := tload(LOCK) }
    }

    function setExecuting(bool value) internal {
        assembly ("memory-safe") { tstore(LOCK, value) }
    }

    function manager() internal view returns (address value) {
        assembly ("memory-safe") { value := tload(MANAGER) }
    }

    function setManager(address value) internal {
        assembly ("memory-safe") { tstore(MANAGER, value) }
    }

    function nextNonce() internal returns (uint256 value) {
        // Never reset within a transaction: sequential routes must not share payloads.
        assembly ("memory-safe") {
            value := add(tload(NONCE), 1)
            tstore(NONCE, value)
        }
    }

    function swap() internal view returns (Swap memory value) {
        assembly ("memory-safe") {
            let control := tload(SWAP)
            mstore(value, and(control, 0xffffffffffffffffffffffffffffffffffffffff))
            mstore(add(value, 32), and(shr(160, control), 0xff))
            mstore(add(value, 64), tload(add(SWAP, 1)))
        }
    }

    function startSwap(address target, uint8 kind, bytes32 dataHash) internal {
        assembly ("memory-safe") {
            tstore(SWAP, or(and(target, 0xffffffffffffffffffffffffffffffffffffffff), shl(160, and(kind, 0xff))))
            tstore(add(SWAP, 1), dataHash)
        }
    }

    function swapPending() internal view returns (bool value) {
        assembly ("memory-safe") { value := iszero(iszero(tload(SWAP))) }
    }

    function consumeSwap() internal {
        assembly ("memory-safe") { tstore(SWAP, 0) }
    }

    function loan() internal view returns (Loan memory value) {
        assembly ("memory-safe") {
            let identity := tload(LOAN)
            mstore(value, and(identity, 0xffffffffffffffffffffffffffffffffffffffff))
            mstore(add(value, 32), and(shr(160, identity), 1))
            mstore(add(value, 64), tload(add(LOAN, 1)))
            mstore(add(value, 96), tload(add(LOAN, 2)))
            mstore(add(value, 128), tload(add(LOAN, 3)))
            mstore(add(value, 160), tload(add(LOAN, 4)))
        }
    }

    function setLoan(Loan memory value) internal {
        assembly ("memory-safe") {
            let lender := and(mload(value), 0xffffffffffffffffffffffffffffffffffffffff)
            tstore(LOAN, or(lender, shl(160, and(mload(add(value, 32)), 1))))
            tstore(add(LOAN, 1), mload(add(value, 64)))
            tstore(add(LOAN, 2), mload(add(value, 96)))
            tstore(add(LOAN, 3), mload(add(value, 128)))
            tstore(add(LOAN, 4), mload(add(value, 160)))
        }
    }

    function finishLoan(uint256 fee) internal {
        assembly ("memory-safe") {
            tstore(add(LOAN, 4), fee)
            tstore(add(LOAN, 3), 3)
        }
    }

    function loanResult() internal view returns (uint256 status, uint256 fee) {
        assembly ("memory-safe") {
            status := tload(add(LOAN, 3))
            fee := tload(add(LOAN, 4))
        }
    }

    function setLoanStatus(uint256 value) internal {
        assembly ("memory-safe") { tstore(add(LOAN, 3), value) }
    }

    function flashSwapResult() internal view returns (uint256 value) {
        assembly ("memory-safe") { value := tload(FLASH_SWAP_RESULT) }
    }

    function setFlashSwapResult(uint256 value) internal {
        assembly ("memory-safe") { tstore(FLASH_SWAP_RESULT, value) }
    }
}
