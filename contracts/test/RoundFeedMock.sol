// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

// Local AggregatorV3-shaped price/status fixture, not a production oracle.
contract TestRoundFeed {
    uint8 public immutable decimals;
    uint80 private roundId;
    int256 private answer;
    uint256 private startedAt;
    uint256 private updatedAt;
    uint80 private answeredInRound;

    constructor(uint8 decimals_, int256 answer_, uint256 started_, uint256 updated_) {
        decimals = decimals_;
        setRound(1, answer_, started_, updated_, 1);
    }

    function setRound(uint80 id, int256 value, uint256 started, uint256 updated, uint80 answered) public {
        roundId = id;
        answer = value;
        startedAt = started;
        updatedAt = updated;
        answeredInRound = answered;
    }

    function latestRoundData() external view returns (uint80, int256, uint256, uint256, uint80) {
        return (roundId, answer, startedAt, updatedAt, answeredInRound);
    }
}
