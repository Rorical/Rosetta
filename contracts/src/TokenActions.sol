// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

library TokenActions {
    error TokenCallFailed();
    error UnexpectedTransfer();

    function balance(address token, address account) internal view returns (uint256) {
        if (token == address(0)) return account.balance;
        bool ok;
        uint256 result;
        // Fixed-size output avoids allocating/copying arbitrary token return data.
        assembly ("memory-safe") {
            let buffer := mload(0x40)
            mstore(buffer, shl(224, 0x70a08231))
            mstore(add(buffer, 4), account)
            ok := staticcall(gas(), token, buffer, 36, buffer, 32)
            ok := and(ok, eq(returndatasize(), 32))
            result := mload(buffer)
        }
        if (!ok) revert TokenCallFailed();
        return result;
    }

    function pay(address token, address recipient, uint256 amount) internal {
        if (amount == 0) return;
        payFromBalance(token, recipient, amount, balance(token, address(this)));
    }

    /// @dev Pays a counterparty that verifies its own receipt: a V2/V3-style
    /// pool's balance check, a V3 lender's repayment check or the V4 manager's
    /// settle. The plan's inventory check covers our side: a token that takes
    /// more than it moves leaves an intermediate or asset balance short.
    function payPool(address token, address recipient, uint256 amount) internal {
        if (amount == 0) return;
        callToken(token, address(0), recipient, amount, false);
    }

    /// @dev beforeSender must be freshly observed, with no intervening mutable
    /// external call. Returns the verified balance for the next settlement payment.
    function payFromBalance(address token, address recipient, uint256 amount, uint256 beforeSender)
        internal
        returns (uint256 afterSender)
    {
        if (amount == 0) return beforeSender;
        uint256 beforeRecipient = balance(token, recipient);
        if (token == address(0)) {
            (bool ok,) = recipient.call{value: amount}("");
            if (!ok) revert TokenCallFailed();
        } else {
            callToken(token, address(0), recipient, amount, false);
        }
        // Taxed/rebasing tokens cannot consume old inventory or underpay a lender.
        afterSender = balance(token, address(this));
        if (afterSender + amount != beforeSender || balance(token, recipient) != beforeRecipient + amount) {
            revert UnexpectedTransfer();
        }
    }

    /// @dev The caller has already observed beforeBalance; only read-only route
    /// validation may occur between that observation and pulling the principal.
    function pull(address token, address sender, uint256 amount, uint256 beforeBalance) internal {
        callToken(token, sender, address(this), amount, true);
        if (balance(token, address(this)) != beforeBalance + amount) revert UnexpectedTransfer();
    }

    function callToken(address token, address sender, address recipient, uint256 amount, bool pulling) private {
        if (token.code.length == 0) revert TokenCallFailed();
        bool ok;
        assembly ("memory-safe") {
            let buffer := mload(0x40)
            let addressMask := 0xffffffffffffffffffffffffffffffffffffffff
            let length := 68
            switch pulling
            case 0 {
                mstore(buffer, shl(224, 0xa9059cbb))
                mstore(add(buffer, 4), and(recipient, addressMask))
                mstore(add(buffer, 36), amount)
            }
            default {
                mstore(buffer, shl(224, 0x23b872dd))
                mstore(add(buffer, 4), and(sender, addressMask))
                mstore(add(buffer, 36), and(recipient, addressMask))
                mstore(add(buffer, 68), amount)
                length := 100
            }
            ok := call(gas(), token, 0, buffer, length, buffer, 32)
            switch returndatasize()
            case 0 {}
            case 32 { ok := and(ok, eq(mload(buffer), 1)) }
            default { ok := 0 }
        }
        if (!ok) revert TokenCallFailed();
    }
}
