package rpc

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

type receiptFeePayload struct {
	Type                 string  `json:"type"`
	GasUsed              *string `json:"gasUsed"`
	EffectiveGasPrice    *string `json:"effectiveGasPrice"`
	L1Fee                *string `json:"l1Fee"`
	OperatorFeeScalar    *string `json:"operatorFeeScalar"`
	OperatorFeeConstant  *string `json:"operatorFeeConstant"`
	DAFootprintGasScalar *string `json:"daFootprintGasScalar"`
	L1GasPrice           *string `json:"l1GasPrice"`
	L1BlobBaseFee        *string `json:"l1BlobBaseFee"`
	L1BaseFeeScalar      *string `json:"l1BaseFeeScalar"`
	L1BlobBaseFeeScalar  *string `json:"l1BlobBaseFeeScalar"`
}

// feeParams fingerprints the OP fee parameters a user transaction was charged
// under. The L1 values come from the L1Block predeploy, which changes them only
// with the L1 origin; the operator values only by a SystemConfig update. Equal
// fingerprints therefore mean equal L1 and operator fee functions. Deposits and
// receipts without L1 fields have none.
func (p receiptFeePayload) feeParams(family domain.ChainFamily) string {
	if family != domain.FamilyOPStack || p.Type == "0x7e" || p.L1GasPrice == nil {
		return ""
	}
	key := ""
	for _, field := range []*string{p.L1GasPrice, p.L1BlobBaseFee, p.L1BaseFeeScalar, p.L1BlobBaseFeeScalar, p.OperatorFeeScalar, p.OperatorFeeConstant, p.DAFootprintGasScalar} {
		value := "-"
		if field != nil {
			value = *field
		}
		key += value + "|"
	}
	return key
}

func receiptFeeWord(name string, text *string) (*big.Int, error) {
	if text == nil {
		return nil, nil
	}
	if len(*text) < 3 || (*text)[:2] != "0x" {
		return nil, fmt.Errorf("rpc: invalid %s fee quantity", name)
	}
	n, ok := new(big.Int).SetString((*text)[2:], 16)
	if !ok || n.Sign() < 0 || n.BitLen() > 256 {
		return nil, fmt.Errorf("rpc: invalid %s fee quantity", name)
	}
	return n, nil
}
func (p receiptFeePayload) fees(family domain.ChainFamily) (*domain.ReceiptFees, error) {
	if family == "" || p.Type == "0x7e" {
		return nil, nil
	}
	if p.GasUsed == nil || p.EffectiveGasPrice == nil {
		return nil, nil
	}
	gas, err := receiptFeeWord("gasUsed", p.GasUsed)
	if err != nil {
		return nil, err
	}
	if !gas.IsUint64() {
		return nil, fmt.Errorf("rpc: receipt gas exceeds uint64")
	}
	price, err := receiptFeeWord("effectiveGasPrice", p.EffectiveGasPrice)
	if err != nil {
		return nil, err
	}
	f := &domain.ReceiptFees{Family: family, GasUsed: gas.Uint64(), GasPrice: price, Execution: new(big.Int).Mul(gas, price)}
	f.Total = new(big.Int).Set(f.Execution)
	switch family {
	case domain.FamilyNitro: // gasUsed includes Nitro's L1 gas. Do not add it twice.
	case domain.FamilyOPStack:
		f.L1, err = receiptFeeWord("l1Fee", p.L1Fee)
		if err != nil {
			return nil, err
		}
		if f.L1 == nil {
			return nil, nil
		}
		f.Operator = new(big.Int)
		if p.OperatorFeeScalar != nil || p.OperatorFeeConstant != nil {
			scalar, e := receiptFeeWord("operatorFeeScalar", p.OperatorFeeScalar)
			if e != nil {
				return nil, e
			}
			constant, e := receiptFeeWord("operatorFeeConstant", p.OperatorFeeConstant)
			if e != nil {
				return nil, e
			}
			if scalar == nil || constant == nil || scalar.BitLen() > 32 || constant.BitLen() > 64 {
				return nil, fmt.Errorf("rpc: incomplete operator fee parameters")
			}
			f.Operator.Mul(gas, scalar)
			if p.DAFootprintGasScalar != nil { // Jovian receipt marker; not the DA charge itself.
				marker, e := receiptFeeWord("daFootprintGasScalar", p.DAFootprintGasScalar)
				if e != nil || marker.BitLen() > 16 {
					return nil, fmt.Errorf("rpc: invalid Jovian receipt marker")
				}
				f.Operator.Mul(f.Operator, big.NewInt(100))
			} else {
				f.Operator.Quo(f.Operator, big.NewInt(1000000))
			}
			f.Operator.Add(f.Operator, constant)
		}
		// Standard OP receipts omit both operator fields when the charge is zero.
		f.Total.Add(f.Total, f.L1).Add(f.Total, f.Operator)
	default:
		return nil, nil
	}
	if f.Total.BitLen() > 256 {
		return nil, fmt.Errorf("rpc: paid fee exceeds uint256")
	}
	return f, nil
}
