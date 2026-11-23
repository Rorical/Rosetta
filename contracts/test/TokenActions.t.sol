// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {TokenActions} from "../src/TokenActions.sol";
import {TestToken} from "./PoolMocks.sol";

contract TokenActionsHarness {
    receive() external payable {}

    function balance(address token) external view returns (uint256) {
        return TokenActions.balance(token, address(this));
    }

    function pay(address token, address recipient, uint256 amount) external {
        TokenActions.pay(token, recipient, amount);
    }

    function settle(address token, address first, address second, uint256 amount) external returns (uint256) {
        uint256 remaining = TokenActions.balance(token, address(this));
        remaining = TokenActions.payFromBalance(token, first, amount, remaining);
        return TokenActions.payFromBalance(token, second, amount, remaining);
    }
}

contract SettlementMutation {
    TestToken private immutable token;
    address private immutable recipient;
    uint256 private calls;

    constructor(TestToken token_, address recipient_) {
        token = token_;
        recipient = recipient_;
    }

    function onTransfer() external {
        if (++calls == 2) token.mint(recipient, 1);
    }
}

contract RefundingRecipient {
    receive() external payable {
        (bool ok,) = msg.sender.call{value: 1}("");
        require(ok, "refund failed");
    }
}

contract TokenActionsTest is ExecutorFixture {
    TokenActionsHarness private harness;

    function setUp() public override {
        super.setUp();
        harness = new TokenActionsHarness();
        token0.mint(address(harness), 1000);
    }

    function testFuzzBalanceRejectsMalformedLength(uint8 length) public {
        if (length == 32) return;
        vm.mockCall(address(token0), abi.encodeCall(token0.balanceOf, (address(harness))), new bytes(length));
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        harness.balance(address(token0));
    }

    function testFuzzBalancePreservesFullWord(uint256 amount) public {
        vm.mockCall(address(token0), abi.encodeCall(token0.balanceOf, (address(harness))), abi.encode(amount));
        assertEq(harness.balance(address(token0)), amount);
    }

    function testFuzzTransferRejectsNonBooleanWord(uint256 word) public {
        if (word == 1) return;
        vm.mockCall(address(token0), abi.encodeCall(token0.transfer, (OPERATOR, 100)), abi.encode(word));
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        harness.pay(address(token0), OPERATOR, 100);
        assertEq(token0.balanceOf(address(harness)), 1000);
    }

    function testFuzzTransferRejectsMalformedLength(uint8 length) public {
        if (length == 0 || length == 32) return;
        vm.mockCall(address(token0), abi.encodeCall(token0.transfer, (OPERATOR, 100)), new bytes(length));
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        harness.pay(address(token0), OPERATOR, 100);
    }

    function testEmptyRevertDoesNotLookLikeNoReturnToken() public {
        vm.mockCallRevert(address(token0), abi.encodeCall(token0.transfer, (OPERATOR, 100)), "");
        vm.expectRevert(TokenActions.TokenCallFailed.selector);
        harness.pay(address(token0), OPERATOR, 100);
    }

    function testKnownBalanceRejectsSecondPaymentSenderMutation() public {
        SettlementMutation hook = new SettlementMutation(token0, address(harness));
        token0.reenter(address(hook), abi.encodeCall(hook.onTransfer, ()));
        vm.expectRevert(TokenActions.UnexpectedTransfer.selector);
        harness.settle(address(token0), OPERATOR, address(this), 100);
        assertEq(token0.balanceOf(address(harness)), 1000);
        assertEq(token0.balanceOf(OPERATOR), 100_000);
    }

    function testKnownBalanceRejectsSecondPaymentRecipientMutation() public {
        SettlementMutation hook = new SettlementMutation(token0, address(this));
        token0.reenter(address(hook), abi.encodeCall(hook.onTransfer, ()));
        vm.expectRevert(TokenActions.UnexpectedTransfer.selector);
        harness.settle(address(token0), OPERATOR, address(this), 100);
        assertEq(token0.balanceOf(address(this)), 0);
    }

    function testKnownNativeBalanceRejectsRefundingRecipient() public {
        RefundingRecipient recipient = new RefundingRecipient();
        vm.deal(address(harness), 1000);
        vm.expectRevert(TokenActions.UnexpectedTransfer.selector);
        harness.settle(address(0), OPERATOR, address(recipient), 100);
        assertEq(address(harness).balance, 1000);
    }
}
