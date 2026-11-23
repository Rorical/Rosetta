package exec

import (
	"fmt"
	"math/big"
)

type Economics struct {
	Conversion       FeeConversion
	NativeFeeBudget  *big.Int
	ActualFeeInAsset *big.Int
	BudgetInAsset    *big.Int
	ActualNetProfit  *big.Int
	BudgetNetProfit  *big.Int
	MinimumNetProfit *big.Int
}

// CalculateEconomics rounds expenses up and subtracts fees once. Profit from
// the executor already includes flash repayment and the actual loan fee.
func CalculateEconomics(evidence ExecutionEvidence, conversion FeeConversion, minimum *big.Int, models ...FeeModel) (Economics, error) {
	for _, field := range []struct {
		name     string
		value    *big.Int
		bits     int
		positive bool
	}{
		{"profit", evidence.Profit, 256, true}, {"fee", evidence.TotalFeeWei, 256, false}, {"fee cap", evidence.Call.FeeCap, 256, true},
		{"numerator", conversion.Numerator, 4096, true}, {"denominator", conversion.Denominator, 4096, true}, {"minimum", minimum, 256, true},
	} {
		if err := validUint(field.name, field.value, field.bits, field.positive); err != nil {
			return Economics{}, err
		}
	}
	if evidence.Call.GasLimit == 0 || evidence.GasUsed > evidence.Call.GasLimit {
		return Economics{}, fmt.Errorf("execution gas differs from its budget")
	}
	model, err := feeModel(evidence, models)
	if err != nil {
		return Economics{}, err
	}
	budget, err := model.Budget(evidence)
	if err != nil {
		return Economics{}, err
	}
	if budget == nil || budget.Sign() < 0 {
		return Economics{}, fmt.Errorf("invalid native fee budget")
	}
	if evidence.TotalFeeWei.Cmp(budget) > 0 {
		return Economics{}, fmt.Errorf("actual native fee exceeds transaction budget")
	}
	actualCost := ceilRatio(evidence.TotalFeeWei, conversion.Numerator, conversion.Denominator)
	budgetCost := ceilRatio(budget, conversion.Numerator, conversion.Denominator)
	return Economics{Conversion: conversion, NativeFeeBudget: budget, ActualFeeInAsset: actualCost, BudgetInAsset: budgetCost,
		ActualNetProfit: new(big.Int).Sub(evidence.Profit, actualCost), BudgetNetProfit: new(big.Int).Sub(evidence.Profit, budgetCost),
		MinimumNetProfit: new(big.Int).Set(minimum)}, nil
}

// FeeInAsset is used before simulation to put the budget into the contract's
// minProfit guard. All amounts are integer base units.
func FeeInAsset(native *big.Int, conversion FeeConversion) (*big.Int, error) {
	if native == nil || native.Sign() < 0 || conversion.Numerator == nil || conversion.Numerator.Sign() <= 0 ||
		conversion.Denominator == nil || conversion.Denominator.Sign() <= 0 {
		return nil, fmt.Errorf("invalid native fee conversion")
	}
	return ceilRatio(native, conversion.Numerator, conversion.Denominator), nil
}

func ceilRatio(amount, numerator, denominator *big.Int) *big.Int {
	product := new(big.Int).Mul(amount, numerator)
	result, remainder := new(big.Int), new(big.Int)
	result.QuoRem(product, denominator, remainder)
	if remainder.Sign() != 0 {
		result.Add(result, big.NewInt(1))
	}
	return result
}
