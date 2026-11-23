// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

library CallMeter {
    // Keep encoding and returndata allocation outside the measured CALL under
    // both compiler pipelines. A high-level gasleft/call pair can move that work.
    function call(address target, bytes memory payload) internal returns (bool ok, bytes memory result, uint256 used) {
        assembly ("memory-safe") {
            let beforeGas := gas()
            ok := call(gas(), target, 0, add(payload, 32), mload(payload), 0, 0)
            used := sub(beforeGas, gas())
            let size := returndatasize()
            result := mload(0x40)
            mstore(result, size)
            returndatacopy(add(result, 32), 0, size)
            mstore(0x40, add(add(result, 32), and(add(size, 31), not(31))))
        }
    }
}
