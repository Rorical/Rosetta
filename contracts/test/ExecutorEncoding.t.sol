// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorFixture} from "./ExecutorFixture.sol";
import {PackedTestPlan} from "./PackedTestPlan.sol";
import {PackedPlan} from "../src/PackedPlan.sol";
import {ExecutorTypes as T} from "../src/ExecutorTypes.sol";

contract ExecutorEncodingTest is ExecutorFixture {
    // Shared with internal/exec/testdata/plan.json and packed_plan.json;
    // synthetic addresses, not deployments.
    function testGoCalldataVectorMatchesSolidityABI() public view {
        T.Step[] memory steps = new T.Step[](2);
        steps[0] = T.Step(
            T.V3,
            address(0x1111111111111111111111111111111111111111),
            address(0x5555555555555555555555555555555555555555),
            500,
            10,
            1900,
            0
        );
        steps[1] = T.Step(
            T.V4,
            address(0x2222222222222222222222222222222222222222),
            address(0x4444444444444444444444444444444444444444),
            3000,
            60,
            1100,
            4295128740
        );
        T.Plan memory plan = T.Plan(
            T.Funding.V3Flash,
            address(0x6666666666666666666666666666666666666666),
            true,
            address(0x4444444444444444444444444444444444444444),
            1000,
            5,
            steps,
            100,
            2_000_000_000
        );
        bytes memory packed = PackedTestPlan.encode(plan);
        require(packed.length == 328, "packed length");
        require(
            keccak256(abi.encodeCall(executor.executePacked, (packed)))
                == 0x78c5f58b6c798dee87034e37f2ce52a53f0f25d904b906600b048beb92a115cd,
            "packed Go/Solidity ABI differs"
        );
        require(
            keccak256(abi.encodeCall(executor.execute, (plan)))
                == 0xa8ba45eb666719e387a475b30b9e33f789661136fd8c0f5b2c7c4e4760b9dc56,
            "Go/Solidity ABI differs"
        );
        // Version 3: each step's bound follows its optional price limit.
        uint160[] memory bounds = new uint160[](2);
        (bounds[0], bounds[1]) = (2 ** 96, 4295128741);
        bytes memory checked = PackedTestPlan.encodeChecked(plan, bounds);
        require(checked.length == 368, "checked length");
        require(
            keccak256(abi.encodeCall(executor.executePacked, (checked)))
                == 0x14cbfed9cae1acade61bece63c49dd34ec767766fc5497b49fdafea27ab84d29,
            "checked Go/Solidity ABI differs"
        );
        // Version 4: the same prices as quote references and a 0.999 minimum rate.
        bytes memory cumulative = PackedTestPlan.encodeCumulative(plan, bounds, 0.999e18);
        require(cumulative.length == 376, "cumulative length");
        require(
            keccak256(abi.encodeCall(executor.executePacked, (cumulative)))
                == 0x044cfc49988b1d9f072da379747b4116f0d2b2fa7f9c64844ef67417032662b7,
            "cumulative Go/Solidity ABI differs"
        );
    }

    // Shared with internal/exec/metric_plan_test.go: a wallet-funded route
    // whose first leg is a MetricOmmPool. The leg carries kind 8, no price
    // limit and its own minimum; the packed plan decodes back to the same.
    function testGoMetricOmmVectorMatchesSolidityABI() public view {
        T.Step[] memory steps = new T.Step[](2);
        steps[0] = T.Step(
            T.METRIC_OMM,
            address(0x7777777777777777777777777777777777777777),
            address(0x5555555555555555555555555555555555555555),
            0,
            0,
            1900,
            0
        );
        steps[1] = T.Step(
            T.V3,
            address(0x1111111111111111111111111111111111111111),
            address(0x4444444444444444444444444444444444444444),
            500,
            10,
            1100,
            0
        );
        T.Plan memory plan = T.Plan(
            T.Funding.Wallet,
            address(0),
            false,
            address(0x4444444444444444444444444444444444444444),
            1000,
            0,
            steps,
            100,
            2_000_000_000
        );
        require(
            keccak256(abi.encodeCall(executor.execute, (plan)))
                == 0x18173699c2cc0343068d297c5b70ab714df179f73cf21ac0cb6b8bcb30e41a6b,
            "Go/Solidity ABI differs"
        );
        bytes memory packed = PackedTestPlan.encode(plan);
        require(packed.length == 255, "packed length");
        require(
            keccak256(abi.encodeCall(executor.executePacked, (packed)))
                == 0x5fc1814129150e95761d030d4d968a1773abf3a9a3001548d6cf3820af160546,
            "packed Go/Solidity ABI differs"
        );
        (T.Plan memory decoded,,) = this.decodePacked(packed);
        require(decoded.steps[0].kind == T.METRIC_OMM && decoded.steps[0].sqrtPriceLimitX96 == 0, "decoded leg");
        require(decoded.steps[0].minAmountOut == 1900 && decoded.steps[0].fee == 0, "decoded minimum");
    }

    // Shared with internal/exec/wrap_plan_test.go: a wallet-funded route in
    // the wrapped native token that ends by wrapping a V4 pool's native
    // output. The WRAP leg carries kind 9, the token as its target and as its
    // output, and its own minimum; the packed plan decodes back to the same.
    function testGoWrapVectorMatchesSolidityABI() public view {
        address wrapped = address(0x4444444444444444444444444444444444444444);
        T.Step[] memory steps = new T.Step[](3);
        steps[0] = T.Step(
            T.V3,
            address(0x1111111111111111111111111111111111111111),
            address(0x5555555555555555555555555555555555555555),
            500,
            10,
            1900,
            0
        );
        steps[1] = T.Step(T.V4, address(0x2222222222222222222222222222222222222222), address(0), 3000, 60, 1100, 0);
        steps[2] = T.Step(T.WRAP, wrapped, wrapped, 0, 0, 1100, 0);
        T.Plan memory plan = T.Plan(T.Funding.Wallet, address(0), false, wrapped, 1000, 0, steps, 100, 2_000_000_000);
        require(
            keccak256(abi.encodeCall(executor.execute, (plan)))
                == 0xd2b76c4fb05c46932f61bd24be7097ca3a5e669fa9a2e28077a1eb19a9f7141c,
            "Go/Solidity ABI differs"
        );
        bytes memory packed = PackedTestPlan.encode(plan);
        require(packed.length == 335, "packed length");
        require(
            keccak256(abi.encodeCall(executor.executePacked, (packed)))
                == 0x0378bf18cdf12a655c51a5f6288c94d7137200829d8632fa30a495d5804a0f40,
            "packed Go/Solidity ABI differs"
        );
        (T.Plan memory decoded,,) = this.decodePacked(packed);
        require(decoded.steps[1].kind == T.V4 && decoded.steps[1].tokenOut == address(0), "decoded native leg");
        require(decoded.steps[2].kind == T.WRAP && decoded.steps[2].target == wrapped, "decoded leg");
        require(decoded.steps[2].tokenOut == wrapped && decoded.steps[2].minAmountOut == 1100, "decoded output");
        require(decoded.steps[2].fee == 0 && decoded.steps[2].sqrtPriceLimitX96 == 0, "decoded fee or limit");
    }

    function decodePacked(bytes calldata encoded)
        external
        pure
        returns (T.Plan memory plan, uint256[] memory prices, uint256 minRate)
    {
        return PackedPlan.decode(encoded);
    }
}
