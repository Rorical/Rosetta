// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

// Synthetic ABI fixtures: these do not model tick crossing or prove deployment compatibility.
library MockPayments {
    function send(address token, address recipient, uint256 amount) internal {
        (bool ok, bytes memory result) = token.call(abi.encodeWithSelector(0xa9059cbb, recipient, amount));
        require(ok && (result.length == 0 || abi.decode(result, (bool))), "token transfer");
    }
}

contract TestToken {
    uint8 public decimals;

    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    uint256 public tax;
    bool public returnFalse;
    bool public noReturn;
    address public reenterTarget;
    bytes public reenterData;
    bool public reentryBlocked;

    function setDecimals(uint8 value) external {
        decimals = value;
    }

    function mint(address recipient, uint256 amount) external {
        balanceOf[recipient] += amount;
    }

    function configure(uint256 tax_, bool returnFalse_, bool noReturn_) external {
        tax = tax_;
        returnFalse = returnFalse_;
        noReturn = noReturn_;
    }

    function reenter(address target, bytes calldata payload) external {
        reenterTarget = target;
        reenterData = payload;
    }

    function approve(address spender, uint256 amount) external returns (bool) {
        allowance[msg.sender][spender] = amount;
        return true;
    }

    function transfer(address recipient, uint256 amount) external returns (bool) {
        move(msg.sender, recipient, amount);
        if (noReturn) assembly { return(0, 0) }
        return !returnFalse;
    }

    function transferFrom(address sender, address recipient, uint256 amount) external returns (bool) {
        allowance[sender][msg.sender] -= amount;
        move(sender, recipient, amount);
        if (noReturn) assembly { return(0, 0) }
        return !returnFalse;
    }

    function move(address sender, address recipient, uint256 amount) private {
        balanceOf[sender] -= amount;
        balanceOf[recipient] += amount - amount * tax / 1_000_000;
        if (reenterTarget != address(0)) {
            (bool ok,) = reenterTarget.call(reenterData);
            reentryBlocked = !ok;
        }
    }
}

contract CallbackProxy {
    function forward(address target, bytes calldata payload) external {
        (bool ok, bytes memory reason) = target.call(payload);
        if (!ok) assembly { revert(add(reason, 32), mload(reason)) }
    }
}

contract TestV3Pool {
    address public token0;
    address public token1;
    uint256 public rate;
    uint8 public callbackKind;
    uint8 public mode;
    uint256 public fee = 5;
    CallbackProxy private proxy = new CallbackProxy();
    bytes private previousPayload;

    // mode: 1 partial, 2 duplicate, 3 wrong caller, 4 bad payload, 5 omitted callback,
    // 6 overcharge, 7 incorrect return, 8 underpay output/loan, 9 other-token flash fee,
    // 10 exhaust gas, 11 replay the first swap payload.
    constructor(address a, address b, uint256 rate_, uint8 callbackKind_) {
        token0 = a;
        token1 = b;
        rate = rate_;
        callbackKind = callbackKind_;
    }

    function setMode(uint8 value) external {
        mode = value;
    }

    function swap(address recipient, bool zeroForOne, int256 specified, uint160, bytes calldata payload)
        external
        returns (int256 delta0, int256 delta1)
    {
        if (mode == 10) {
            assembly { for {} 1 {} {} }
        }
        uint256 amount;
        uint256 out;
        if (specified > 0) {
            amount = uint256(specified);
            if (mode == 1) amount /= 2;
            out = amount * rate / 1_000_000;
        } else {
            // Exact output: the input that buys it, rounded up.
            out = uint256(-specified);
            amount = (out * 1_000_000 + rate - 1) / rate;
        }
        TestToken input = TestToken(zeroForOne ? token0 : token1);
        uint256 beforeBalance = input.balanceOf(address(this));
        MockPayments.send(zeroForOne ? token1 : token0, recipient, mode == 8 ? out - 1 : out);
        int256 owed = int256(mode == 6 ? amount + 1 : amount);
        (delta0, delta1) = zeroForOne ? (owed, -int256(out)) : (-int256(out), owed);
        string memory signature = callbackKind == 1
            ? "pancakeV3SwapCallback(int256,int256,bytes)"
            : callbackKind == 2
                ? "algebraSwapCallback(int256,int256,bytes)"
                : callbackKind == 3
                    ? "solidlyV3SwapCallback(int256,int256,bytes)"
                    : "uniswapV3SwapCallback(int256,int256,bytes)";
        bytes memory data = mode == 4 ? bytes("foreign") : payload;
        if (mode == 11) {
            if (previousPayload.length == 0) previousPayload = payload;
            else data = previousPayload;
        }
        bytes memory callData = abi.encodeWithSignature(signature, delta0, delta1, data);
        if (mode == 3) proxy.forward(msg.sender, callData);
        else if (mode != 5) invoke(msg.sender, callData);
        if (mode == 2) invoke(msg.sender, callData);
        require(input.balanceOf(address(this)) == beforeBalance + amount, "unpaid swap");
        if (mode == 7) if (zeroForOne) ++delta1;
        else ++delta0;
    }

    function flash(address recipient, uint256 amount0, uint256 amount1, bytes calldata payload) external {
        uint256 before0 = TestToken(token0).balanceOf(address(this));
        uint256 before1 = TestToken(token1).balanceOf(address(this));
        if (amount0 != 0) MockPayments.send(token0, recipient, mode == 8 ? amount0 - 1 : amount0);
        if (amount1 != 0) MockPayments.send(token1, recipient, mode == 8 ? amount1 - 1 : amount1);
        uint256 fee0 = amount0 == 0 ? (mode == 9 ? fee : 0) : fee;
        uint256 fee1 = amount1 == 0 ? (mode == 9 ? fee : 0) : fee;
        bytes memory callData = abi.encodeWithSignature(
            callbackKind == 1
                ? "pancakeV3FlashCallback(uint256,uint256,bytes)"
                : "uniswapV3FlashCallback(uint256,uint256,bytes)",
            fee0,
            fee1,
            mode == 4 ? bytes("foreign") : payload
        );
        if (mode == 3) proxy.forward(msg.sender, callData);
        else if (mode != 5) invoke(msg.sender, callData);
        if (mode == 2) invoke(msg.sender, callData);
        require(TestToken(token0).balanceOf(address(this)) >= before0 + fee0, "unpaid loan0");
        require(TestToken(token1).balanceOf(address(this)) >= before1 + fee1, "unpaid loan1");
    }

    function invoke(address target, bytes memory payload) private {
        (bool ok, bytes memory reason) = target.call(payload);
        if (!ok) assembly { revert(add(reason, 32), mload(reason)) }
    }
}

contract TestV2Pool {
    address public token0;
    address public token1;
    uint112 private reserve0;
    uint112 private reserve1;

    constructor(address a, address b) {
        token0 = a;
        token1 = b;
    }

    function sync() public {
        uint256 balance0 = TestToken(token0).balanceOf(address(this));
        uint256 balance1 = TestToken(token1).balanceOf(address(this));
        require(balance0 <= type(uint112).max && balance1 <= type(uint112).max, "reserve overflow");
        reserve0 = uint112(balance0);
        reserve1 = uint112(balance1);
    }

    function getReserves() external view returns (uint112, uint112, uint32) {
        return (reserve0, reserve1, 0);
    }

    function swap(uint256 out0, uint256 out1, address recipient, bytes calldata payload) external {
        require(payload.length == 0 && (out0 != 0 || out1 != 0), "invalid output");
        require(out0 < reserve0 && out1 < reserve1, "liquidity");
        if (out0 != 0) MockPayments.send(token0, recipient, out0);
        if (out1 != 0) MockPayments.send(token1, recipient, out1);
        uint256 balance0 = TestToken(token0).balanceOf(address(this));
        uint256 balance1 = TestToken(token1).balanceOf(address(this));
        uint256 in0 = balance0 > reserve0 - out0 ? balance0 - (reserve0 - out0) : 0;
        uint256 in1 = balance1 > reserve1 - out1 ? balance1 - (reserve1 - out1) : 0;
        require(
            (balance0 * 1000 - in0 * 3) * (balance1 * 1000 - in1 * 3) >= uint256(reserve0) * reserve1 * 1_000_000,
            "invariant"
        );
        sync();
    }
}

/// MetricOmmPool's swap surface: int128 amounts, a Q64.64 fee-inclusive price
/// limit (a floor selling token0, a cap buying it) and metricOmmSwapCallback.
contract TestMetricPool {
    address public token0;
    address public token1;
    uint256 public rate;
    // mode: 1 partial fill, 3 callback through another contract.
    uint8 public mode;
    uint128 public lastLimit;
    CallbackProxy private proxy = new CallbackProxy();

    constructor(address a, address b, uint256 rate_) {
        token0 = a;
        token1 = b;
        rate = rate_;
    }

    function setMode(uint8 value) external {
        mode = value;
    }

    function swap(
        address recipient,
        bool zeroForOne,
        int128 specified,
        uint128 limit,
        bytes calldata payload,
        bytes calldata
    ) external returns (int128 delta0, int128 delta1) {
        require(specified > 0, "exact input");
        // The real pool fills nothing when a buy's cap is zero.
        require(zeroForOne || limit != 0, "buy capped at zero");
        lastLimit = limit;
        uint256 amount = uint256(uint128(specified));
        if (mode == 1) amount /= 2;
        uint256 out = amount * rate / 1_000_000;
        TestToken input = TestToken(zeroForOne ? token0 : token1);
        uint256 beforeBalance = input.balanceOf(address(this));
        MockPayments.send(zeroForOne ? token1 : token0, recipient, out);
        (delta0, delta1) = zeroForOne
            ? (int128(uint128(amount)), -int128(uint128(out)))
            : (-int128(uint128(out)), int128(uint128(amount)));
        bytes memory callData = abi.encodeWithSignature(
            "metricOmmSwapCallback(int256,int256,bytes)", int256(delta0), int256(delta1), payload
        );
        if (mode == 3) {
            proxy.forward(msg.sender, callData);
        } else {
            (bool ok, bytes memory reason) = msg.sender.call(callData);
            if (!ok) assembly { revert(add(reason, 32), mload(reason)) }
        }
        require(input.balanceOf(address(this)) >= beforeBalance + amount, "unpaid swap");
    }
}
