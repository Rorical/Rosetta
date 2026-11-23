// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {TokenActions} from "./TokenActions.sol";
import {ExecutionState as S} from "./ExecutionState.sol";

/// @dev Routes carry their own pool parameters. The executor keeps no pool or
/// token registry. Each route must return its principal and minimum profit or
/// revert; the profit stays here as inventory until the owner sweeps it.
abstract contract ExecutorAdmin {
    error Unauthorized();
    error Busy();
    error InvalidConfiguration();

    address public immutable owner;
    uint256 public immutable chainId;
    // Packed into one slot: execution reads the pause flag with one SLOAD.
    bool public paused = true;
    // Moves on every owner change so off-chain caches know when to re-verify.
    uint64 public configVersion = 1;
    mapping(address => bool) public operators;

    event OperatorConfigured(address indexed operator, bool allowed);
    event Paused(bool paused);

    constructor(address owner_) {
        if (block.chainid == 0 || owner_ == address(0) || owner_ == address(this)) {
            revert InvalidConfiguration();
        }
        owner = owner_;
        chainId = block.chainid;
        operators[owner_] = true;
    }

    modifier onlyIdleOwner() {
        if (msg.sender != owner) revert Unauthorized();
        if (S.executing()) revert Busy();
        _;
    }

    function setOperator(address operator, bool allowed) external onlyIdleOwner {
        if (operator == address(0) || operator == address(this)) revert InvalidConfiguration();
        operators[operator] = allowed;
        ++configVersion;
        emit OperatorConfigured(operator, allowed);
    }

    function setPaused(bool value) external onlyIdleOwner {
        paused = value;
        ++configVersion;
        emit Paused(value);
    }

    function sweep(address token, uint256 amount) external onlyIdleOwner {
        TokenActions.pay(token, owner, amount);
    }
}
