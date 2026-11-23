// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;

import {ExecutorTypes as T, IV3Pool} from "./ExecutorTypes.sol";
import {HotV3Executor} from "./HotV3Executor.sol";
import {TokenActions} from "./TokenActions.sol";
import {ExecutionState as S} from "./ExecutionState.sol";
import {PackedPlan} from "./PackedPlan.sol";
import {PriceCheck} from "./PriceCheck.sol";

/// @notice Executes one closed route. Gas/L1 data costs must also pass off-chain EVM confirmation.
contract ArbitrageExecutor is HotV3Executor {
    error InvalidLoan();

    constructor(address owner_, address[2] memory hot) HotV3Executor(owner_, hot) {}

    function execute(T.Plan calldata plan) external payable returns (uint256 profit, uint256 fee) {
        checkAccess();
        // Reject expired plans before copying their dynamic route into memory.
        checkPlan(plan.deadline, plan.steps.length, plan.minProfit);
        return executePlan(plan);
    }

    function executePacked(bytes calldata encoded) external payable returns (uint256 profit, uint256 fee) {
        checkAccess();
        (T.Plan memory plan, uint256[] memory prices, uint256 minRate) = PackedPlan.decode(encoded);
        checkPlan(plan.deadline, plan.steps.length, plan.minProfit);
        // Versions 3 and 4: a pool a rival moved first fails here, before funding or swaps.
        if (minRate != 0) PriceCheck.checkCumulative(plan, prices, minRate);
        else if (prices.length != 0) PriceCheck.check(plan, prices);
        return executePlan(plan);
    }

    function checkAccess() private view {
        if (!operators[msg.sender]) revert Unauthorized();
        if (S.executing()) revert Busy();
        if (paused || block.chainid != chainId) revert Unavailable();
    }

    function checkPlan(uint256 deadline, uint256 length, uint256 minProfit) private view {
        if (block.timestamp > deadline || length == 0 || length > 16 || minProfit == 0) revert InvalidPlan();
    }

    function executePlan(T.Plan memory plan) private returns (uint256 profit, uint256 fee) {
        S.setExecuting(true);
        uint256 baseline = TokenActions.balance(plan.asset, address(this));
        T.RouteStep[] memory route;
        uint256[] memory inventory;
        if (plan.funding == T.Funding.Wallet || plan.funding == T.Funding.Inventory) {
            if (plan.lender != address(0) || plan.lenderToken0 || plan.maxLoanFee != 0) revert InvalidPlan();
            route = prepareRoute(plan, address(0));
            inventory = inventoryBefore(route, plan.asset);
            if (plan.funding == T.Funding.Inventory) {
                // The executor's own balance is the principal: no pull and no
                // repayment. The closing balance check keeps it whole.
                if (msg.value != 0 || plan.asset == address(0)) revert InvalidPlan();
            } else if (plan.asset == address(0)) {
                if (msg.value != plan.amount) revert InvalidPlan();
                baseline -= msg.value;
            } else {
                if (msg.value != 0) revert InvalidPlan();
                TokenActions.pull(plan.asset, msg.sender, plan.amount, baseline);
            }
            runRoute(route, plan.amount, 0, plan.amount + plan.minProfit);
        } else if (plan.funding == T.Funding.V3FlashSwap || plan.funding == T.Funding.V4Flash) {
            if (
                msg.value != 0 || plan.lender != address(0) || plan.lenderToken0 || plan.maxLoanFee != 0
                    || (plan.funding == T.Funding.V3FlashSwap && plan.asset == address(0))
            ) revert InvalidPlan();
            route = prepareRoute(plan, address(0));
            inventory = inventoryBefore(route, plan.asset);
            if (plan.funding == T.Funding.V3FlashSwap) {
                runFlashSwap(route, plan.amount, plan.minProfit);
            } else {
                // A closed route on one manager settles only its net profit.
                // amount is the swap input and exposure limit, never a transfer.
                runRoute(route, plan.amount, 0, plan.amount + plan.minProfit);
            }
        } else {
            address lender = plan.lender;
            if (msg.value != 0 || plan.asset == address(0) || lender == address(0) || lender == address(this)) {
                revert InvalidPlan();
            }
            route = prepareRoute(plan, lender);
            inventory = inventoryBefore(route, plan.asset);
            bytes memory payload = abi.encode(route, plan.amount, plan.maxLoanFee, plan.minProfit);
            // A lender that sends another token fails the received-balance check
            // in completeLoan; nothing else trusts the lender's token order.
            S.setLoan(S.Loan(lender, plan.lenderToken0, keccak256(payload), baseline, 1, 0));
            IV3Pool(lender)
                .flash(address(this), plan.lenderToken0 ? plan.amount : 0, plan.lenderToken0 ? 0 : plan.amount, payload);
            uint256 status;
            (status, fee) = S.loanResult();
            if (status != 3) revert InvalidLoan();
            S.setLoanStatus(0);
        }
        checkInventory(route, plan.asset, inventory);
        uint256 endBalance = TokenActions.balance(plan.asset, address(this));
        uint256 required = baseline + (plan.funding == T.Funding.Wallet ? plan.amount : 0);
        if (endBalance < required || endBalance - required < plan.minProfit) revert InsufficientProfit();
        profit = endBalance - required;
        // Profit stays in the executor; the owner withdraws it with sweep. Paying
        // it out cost a transfer and three balance reads on every success.
        if (plan.funding == T.Funding.Wallet) {
            TokenActions.payFromBalance(plan.asset, msg.sender, plan.amount, endBalance);
        }
        S.setExecuting(false);
        emit Executed(msg.sender, plan.asset, plan.amount, fee, profit);
    }

    function uniswapV3FlashCallback(uint256 fee0, uint256 fee1, bytes calldata payload) external {
        completeLoan(fee0, fee1, payload);
    }

    function pancakeV3FlashCallback(uint256 fee0, uint256 fee1, bytes calldata payload) external {
        completeLoan(fee0, fee1, payload);
    }

    function completeLoan(uint256 fee0, uint256 fee1, bytes calldata payload) private {
        S.Loan memory loan = S.loan();
        if (!S.executing() || loan.status != 1 || msg.sender != loan.lender || keccak256(payload) != loan.dataHash) {
            revert InvalidCallback();
        }
        S.setLoanStatus(2);
        (T.RouteStep[] memory route, uint256 amount, uint256 maxLoanFee, uint256 minProfit) =
            abi.decode(payload, (T.RouteStep[], uint256, uint256, uint256));
        address asset = route[0].tokenIn;
        // The direction comes from the plan; the balance check below rejects a
        // lender that delivered anything but exactly the requested asset amount.
        bool token0 = loan.token0;
        uint256 fee = token0 ? fee0 : fee1;
        if ((token0 ? fee1 : fee0) != 0 || fee > maxLoanFee) revert InvalidLoan();
        // A lender that delivered less leaves the asset short at the plan's
        // final check; it verifies its own repayment.
        runRoute(route, amount, 0, amount + fee + minProfit);
        TokenActions.payPool(asset, loan.lender, amount + fee);
        S.finishLoan(fee);
    }
}
