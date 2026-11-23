// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {FlashSwapExecutor} from "./FlashSwapExecutor.sol";
import {TokenActions} from "./TokenActions.sol";
import {ExecutionState as S} from "./ExecutionState.sol";
import {PriceCheck} from "./PriceCheck.sol";

/// @dev A closed route in which every pool pays the next pool directly, without
/// the checks a conditional transaction enforces. The bot sends it only with
/// conditions on this contract's slot 0 (pause flag and config version,
/// expected unpaused), on the route's quiet pools and on timestampMax, so a
/// pause, an owner change, a moved quiet pool or an expired route is rejected
/// at its execution position without charge. A plain transaction
/// skips those conditions and its senders are fixed at deployment, so nothing
/// stops one: a sender can only run routes that leave the executor a profit.
///
/// A condition holds only state unchanged for 2 s, so the pool that just
/// moved (the trigger) carries a price bound instead: a rival that restored it
/// through other pools fails here after one read, not at the profit check.
///
/// The route is asset (20 bytes), amount (16), minProfit (16), steps (1, bit 7
/// set to claim the opportunity), then one 32-byte word per step: flags (1:
/// bit 0 zeroForOne, bits 1-2 the kind: 0 a V3-family pool, 1 a V2 pair, 2 a
/// Metric pool), an 11-byte amount and the pool (20); then per bound the step
/// index (1, bit 7 set for Algebra's globalState) and the sqrt price bound
/// (20), one-sided in the step's direction as PriceCheck.check: zeroForOne
/// requires price >= bound.
///
/// The last pool that calls back (the outermost) is swapped first for its
/// exact input, its amount; it sends its output to the executor, or to the V2
/// pair after it. Any pairs after it then swap in order, each for its amount
/// as output. The outermost pool's callback is settled by asking the pool
/// before it for exactly what is owed, sent straight to the pool that is owed
/// it, and so on back to the first pool, which the executor pays in the asset
/// out of the route's own output. A V2 pair before the outermost pool is
/// first supplied its amount as input. Intermediate tokens never reach the
/// executor: it sends only the asset, and the asset's actual profit is checked
/// at the end. The amount is the quoted input and no cap: a first pool
/// that asks for more is paid, and the profit check alone decides.
abstract contract HotV3Executor is FlashSwapExecutor {
    error Unavailable();
    error InvalidPlan();

    event Executed(address indexed operator, address indexed asset, uint256 amount, uint256 loanFee, uint256 profit);

    uint256 private constant HEADER = 53;
    uint256 private constant STEP = 32;
    uint256 private constant BOUND = 21;
    uint256 private constant POOL = (1 << 160) - 1;
    // Transient slots past ExecutionState's.
    uint256 private constant EXPECTED = 32; // pool | (step + 1) << 160
    uint256 private constant LAYOUT = 33; // asset | steps << 160 | outermost << 168
    uint256 private constant RETURN = 35; // the least the last pool must pay out: amount plus minProfit
    uint256 private constant ROUTE = 40; // per step: its calldata word

    /// @dev Raised by each successful route that claims its opportunity. The
    /// variants of one opportunity raced on several lanes all hold this value
    /// as a condition, so the first to succeed drops the others without
    /// charge. Storage slot 2; it starts at 1 so a claim never writes a
    /// zero slot.
    uint256 public claimEpoch = 1;

    /// @dev The senders of executeHot, fixed at deployment: reading them costs
    /// no storage access. Unused places hold the zero address. Changing them
    /// takes a new deployment; the operators mapping still gates every other
    /// entry point.
    address private immutable HOT0;
    address private immutable HOT1;

    constructor(address owner_, address[2] memory hot) FlashSwapExecutor(owner_) {
        (HOT0, HOT1) = (hot[0], hot[1]);
    }

    function executeHot(bytes calldata route) external returns (uint256 profit) {
        address sender = msg.sender;
        if (sender != HOT0 && sender != HOT1) revert Unauthorized();
        if (S.executing()) revert Busy();
        if (block.chainid != chainId) revert Unavailable();
        (address asset, uint256 amount, uint256 minProfit, uint256 steps, uint256 outermost) = loadRoute(route);
        checkBounds(route, steps);
        S.setExecuting(true);
        uint256 baseline = TokenActions.balance(asset, address(this));
        uint256 word = step(outermost);
        callPool(outermost, word, outermost + 1 == steps ? address(this) : address(uint160(step(outermost + 1))), 0);
        uint256 endBalance = TokenActions.balance(asset, address(this));
        if (endBalance < baseline + minProfit) revert InsufficientProfit();
        profit = endBalance - baseline;
        if (uint8(route[52]) >= 0x80) ++claimEpoch;
        S.setExecuting(false);
        emit Executed(msg.sender, asset, amount, 0, profit);
    }

    /// @dev Pools and directions are the operator's; a wrong one reverts in
    /// the pool (wrong direction, unpaid input, lock) or fails the profit check.
    function loadRoute(bytes calldata route)
        private
        returns (address asset, uint256 amount, uint256 minProfit, uint256 steps, uint256 outermost)
    {
        if (route.length < HEADER) revert InvalidPlan();
        assembly ("memory-safe") {
            asset := shr(96, calldataload(route.offset))
            amount := shr(128, calldataload(add(route.offset, 20)))
            minProfit := shr(128, calldataload(add(route.offset, 36)))
            steps := and(byte(0, calldataload(add(route.offset, 52))), 0x7f)
        }
        uint256 bounds = HEADER + steps * STEP;
        if (
            steps < 2 || steps > 16 || route.length < bounds || (route.length - bounds) % BOUND != 0 || amount == 0
                || minProfit == 0
        ) revert InvalidPlan();
        outermost = steps;
        assembly ("memory-safe") {
            for { let i := 0 } lt(i, steps) { i := add(i, 1) } {
                let word := calldataload(add(add(route.offset, HEADER), mul(i, STEP)))
                tstore(add(ROUTE, i), word)
                if iszero(eq(and(shr(249, word), 3), 1)) { outermost := i }
            }
            tstore(RETURN, add(amount, minProfit))
            tstore(LAYOUT, or(asset, or(shl(160, steps), shl(168, outermost))))
        }
        // A route of pairs alone has no pool to deliver before it is paid.
        if (outermost == steps) revert InvalidPlan();
    }

    /// @dev Reads each bounded pool before any balance or swap, so a lost race
    /// costs little more than the access checks.
    function checkBounds(bytes calldata route, uint256 steps) private view {
        for (uint256 at = HEADER + steps * STEP; at < route.length; at += BOUND) {
            uint256 entry;
            assembly ("memory-safe") { entry := calldataload(add(route.offset, at)) }
            uint256 index = (entry >> 248) & 0x7f;
            if (index >= steps) revert InvalidPlan();
            uint256 bound = (entry >> 88) & type(uint160).max;
            uint256 word = step(index);
            // slot0() for Uniswap, Pancake and Slipstream; Algebra's globalState().
            uint256 selector = entry >> 255 == 1 ? 0xe76c01e4 : 0x3850c7bd;
            uint256 price;
            bool ok;
            assembly ("memory-safe") {
                mstore(0, shl(224, selector))
                ok := staticcall(gas(), and(word, POOL), 0, 4, 0, 32)
                ok := and(ok, iszero(lt(returndatasize(), 32)))
                price := and(mload(0), POOL)
            }
            if (!ok) revert PriceCheck.UnreadablePool(index);
            if ((word >> 248) & 1 == 1 ? price < bound : price > bound) revert PriceCheck.PoolMoved(index);
        }
    }

    function step(uint256 i) private view returns (uint256 word) {
        assembly ("memory-safe") { word := tload(add(ROUTE, i)) }
    }

    /// @dev Swaps on step i's pool, which calls back for its input: for its
    /// amount as exact input when output is zero, otherwise for exactly output,
    /// which the pool after it is owed. The pool sends what it pays out to
    /// recipient.
    function callPool(uint256 i, uint256 word, address recipient, uint256 output) private {
        uint256 amount = (word >> 160) & ((1 << 88) - 1);
        assembly ("memory-safe") {
            let pool := and(word, POOL)
            let zeroForOne := and(shr(248, word), 1)
            if output { amount := sub(0, output) }
            tstore(EXPECTED, or(pool, shl(160, add(i, 1))))
            let m := mload(0x40)
            let size := 196
            let metric := eq(and(shr(249, word), 3), 2)
            // swap(address,bool,int256,uint160,bytes), or Metric's
            // swap(address,bool,int128,uint128,bytes,bytes).
            mstore(m, shl(224, add(mul(metric, 0xe24f0884), mul(iszero(metric), 0x128acb08))))
            mstore(add(m, 4), recipient)
            mstore(add(m, 36), zeroForOne)
            mstore(add(m, 68), amount)
            switch metric
            case 1 {
                // A fee-inclusive Q64.64 limit: none selling token0, the
                // widest cap buying it.
                mstore(add(m, 100), mul(iszero(zeroForOne), sub(shl(128, 1), 1)))
                mstore(add(m, 132), 192)
                mstore(add(m, 164), 224)
                mstore(add(m, 196), 0)
                mstore(add(m, 228), 0)
                size := 260
            }
            default {
                // MIN_SQRT_RATIO + 1 or MAX_SQRT_RATIO - 1.
                switch zeroForOne
                case 1 { mstore(add(m, 100), 4295128740) }
                default { mstore(add(m, 100), 1461446703485210103287273052203988822378723970341) }
                mstore(add(m, 132), 160)
                mstore(add(m, 164), 0)
            }
            if iszero(call(gas(), pool, 0, m, size, 0, 0)) {
                returndatacopy(m, 0, returndatasize())
                revert(m, returndatasize())
            }
            // The pool must have asked for its input.
            if tload(EXPECTED) { revert(0, 0) }
        }
    }

    function hotSwapCallback(int256 amount0, int256 amount1) internal override returns (bool) {
        uint256 expected;
        uint256 layout;
        assembly ("memory-safe") {
            expected := tload(EXPECTED)
            layout := tload(LAYOUT)
        }
        if (expected == 0) return false;
        if (msg.sender != address(uint160(expected))) revert InvalidCallback();
        assembly ("memory-safe") { tstore(EXPECTED, 0) }
        uint256 i = (expected >> 160) - 1;
        int256 owed = amount0 > 0 ? amount0 : amount1;
        if (owed <= 0) revert InvalidSwap();
        uint256 steps = (layout >> 160) & 0xff;
        if (i == layout >> 168) {
            if (i + 1 == steps) {
                // The outermost pool, as the last step, has just paid the
                // executor the route's whole return, before any other pool
                // swapped. The pools before it were quoted to take the
                // amount, so a return below the amount plus the minimum
                // profit is taken to end at the profit check: it fails here
                // instead, without the gas of the pools before it. A Metric pool's
                // oracle quote can change with no pool moved, which no
                // condition or price bound catches.
                // What the pool paid is the delta that is not owed; both
                // deltas positive pass here and fail the profit check.
                assembly ("memory-safe") {
                    if lt(sub(owed, add(amount0, amount1)), tload(RETURN)) {
                        mstore(0, shl(224, 0x4e47f8ea)) // InsufficientProfit()
                        revert(0, 4)
                    }
                }
            }
            // The outermost pool has paid the pair after it: the pairs swap in
            // order and the last sends the asset to the executor.
            for (uint256 j = i + 1; j < steps; ++j) {
                swapPair(step(j), 0, j + 1 == steps ? address(this) : address(uint160(step(j + 1))));
            }
        }
        supply(i, uint256(owed), address(uint160(layout)));
        return true;
    }

    /// @dev Delivers amount of step i's input to its pool: the asset from the
    /// executor for the first step, otherwise the output of the step before.
    function supply(uint256 i, uint256 amount, address asset) private {
        uint256 word = step(i);
        if (i == 0) {
            TokenActions.payPool(asset, address(uint160(word)), amount);
            return;
        }
        uint256 previous = step(i - 1);
        if ((previous >> 249) & 3 == 1) {
            supply(i - 1, (previous >> 160) & ((1 << 88) - 1), asset);
            swapPair(previous, amount, address(uint160(word)));
        } else {
            callPool(i - 1, previous, address(uint160(word)), amount);
        }
    }

    /// @dev A pair that already holds its input sends out to recipient; zero
    /// out means the step's own amount.
    function swapPair(uint256 word, uint256 out, address recipient) private {
        if (out == 0) out = (word >> 160) & ((1 << 88) - 1);
        bool zeroForOne = (word >> 248) & 1 == 1;
        IV2Pair(address(uint160(word))).swap(zeroForOne ? 0 : out, zeroForOne ? out : 0, recipient, "");
    }
}

interface IV2Pair {
    function swap(uint256, uint256, address, bytes calldata) external;
}
