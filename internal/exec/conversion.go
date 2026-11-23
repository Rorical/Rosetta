package exec

import (
	"fmt"
	"math"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

func validateConversion(plan *ReadyPlan) error {
	c := plan.Economics.Conversion
	if plan.Route.Asset == (domain.Address{}) {
		if c.AssetDecimals != 18 || c.Numerator.Cmp(big.NewInt(1)) != 0 || c.Denominator.Cmp(big.NewInt(1)) != 0 {
			return fmt.Errorf("native fee conversion must be one to one")
		}
		return nil
	}
	now := plan.Evidence.Context.Timestamp
	if c.MaxPriceAge == 0 || c.GraceSeconds == 0 || c.Sequencer == nil || c.Sequencer.Answer == nil ||
		c.Sequencer.Answer.Sign() != 0 || c.Sequencer.StartedAt == 0 || c.Sequencer.StartedAt > now || now-c.Sequencer.StartedAt <= c.GraceSeconds {
		return fmt.Errorf("missing or unhealthy sequencer observation")
	}
	for _, price := range []*OracleRound{c.NativePrice, c.AssetPrice} {
		if price == nil || price.Feed == (domain.Address{}) || price.CodeHash == (domain.Hash{}) || price.Answer == nil ||
			price.Answer.Sign() <= 0 || price.Answer.BitLen() > 255 || price.RoundID == nil || price.RoundID.Sign() <= 0 ||
			price.UpdatedAt == 0 || price.UpdatedAt > now || now-price.UpdatedAt > c.MaxPriceAge ||
			c.MaxPriceAge > math.MaxInt64/1000 || price.UpdatedAt > math.MaxInt64/1000-c.MaxPriceAge ||
			plan.ExpiresUnixMS > int64(price.UpdatedAt+c.MaxPriceAge)*1000 {
			return fmt.Errorf("missing, invalid or expired price observation")
		}
	}
	numerator := new(big.Int).Mul(c.NativePrice.Answer, conversionPower(uint64(c.AssetDecimals)+uint64(c.AssetPrice.Decimals)))
	denominator := new(big.Int).Mul(c.AssetPrice.Answer, conversionPower(18+uint64(c.NativePrice.Decimals)))
	if c.Numerator.Cmp(numerator) != 0 || c.Denominator.Cmp(denominator) != 0 {
		return fmt.Errorf("fee conversion differs from USD feed observations")
	}
	return nil
}

func conversionPower(decimals uint64) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), new(big.Int).SetUint64(decimals), nil)
}
