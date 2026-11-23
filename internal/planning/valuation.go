package planning

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// Valuer caches oracle observations within one immutable execution position.
// It must not be reused with a different Reader or Policy.
type Valuer struct {
	Reader execution.StateReader
	Policy Policy
	cache  map[domain.Address]execution.FeeConversion
}

// Conversion returns snapshot-bound native-to-asset fee conversion evidence.
func (r *Valuer) Conversion(ctx context.Context, asset domain.Address) (execution.FeeConversion, error) {
	if result, ok := r.cache[asset]; ok {
		return result, nil
	}
	if asset == (domain.Address{}) {
		return execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1), AssetDecimals: 18}, nil
	}
	config := r.Policy.Assets[asset]
	uptime, err := r.round(ctx, r.Policy.Sequencer, false)
	if err != nil {
		return execution.FeeConversion{}, err
	}
	now := r.Reader.Position().Timestamp
	if uptime.Answer.Sign() != 0 || uptime.StartedAt == 0 || uptime.StartedAt > now || now-uptime.StartedAt <= r.Policy.Grace {
		return execution.FeeConversion{}, fmt.Errorf("sequencer is down, uninitialized or inside recovery grace")
	}
	nativePrice, err := r.round(ctx, r.Policy.NativePrice, true)
	if err != nil {
		return execution.FeeConversion{}, err
	}
	assetPrice, err := r.round(ctx, config.PriceFeed, true)
	if err != nil {
		return execution.FeeConversion{}, err
	}
	for _, price := range []*execution.OracleRound{nativePrice, assetPrice} {
		if price.Answer.Sign() <= 0 || price.UpdatedAt == 0 || price.UpdatedAt > now || now-price.UpdatedAt > config.MaxPriceAge {
			return execution.FeeConversion{}, fmt.Errorf("execution USD price is nonpositive, future or stale")
		}
	}
	decimals, err := r.decimals(ctx, asset)
	if err != nil {
		return execution.FeeConversion{}, err
	}
	numerator := new(big.Int).Mul(nativePrice.Answer, power10(uint64(decimals)+uint64(assetPrice.Decimals)))
	denominator := new(big.Int).Mul(assetPrice.Answer, power10(18+uint64(nativePrice.Decimals)))
	result := execution.FeeConversion{Numerator: numerator, Denominator: denominator, AssetDecimals: decimals,
		NativePrice: nativePrice, AssetPrice: assetPrice, Sequencer: uptime, MaxPriceAge: config.MaxPriceAge, GraceSeconds: r.Policy.Grace}
	if r.cache == nil {
		r.cache = make(map[domain.Address]execution.FeeConversion)
	}
	r.cache[asset] = result
	return result, nil
}

func (r *Valuer) decimals(ctx context.Context, target domain.Address) (uint8, error) {
	word, err := r.Reader.ReadContract(ctx, target, []byte{0x31, 0x3c, 0xe5, 0x67})
	if err != nil {
		return 0, err
	}
	if len(word) != 32 {
		return 0, fmt.Errorf("decimals returned malformed ABI")
	}
	value := new(big.Int).SetBytes(word)
	if value.BitLen() > 8 {
		return 0, fmt.Errorf("invalid token/feed decimals")
	}
	return uint8(value.Uint64()), nil
}

func (r *Valuer) round(ctx context.Context, target domain.Address, price bool) (*execution.OracleRound, error) {
	codeHash, err := r.Reader.CodeHash(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("read oracle code: %w", err)
	}
	if codeHash == (domain.Hash{}) {
		return nil, fmt.Errorf("oracle has no deployed code")
	}
	raw, err := r.Reader.ReadContract(ctx, target, []byte{0xfe, 0xaf, 0x96, 0x8c})
	if err != nil {
		return nil, err
	}
	if len(raw) != 160 {
		return nil, fmt.Errorf("oracle round has malformed ABI")
	}
	fields := make([]*big.Int, 5)
	for i := range fields {
		fields[i] = new(big.Int).SetBytes(raw[i*32 : (i+1)*32])
	}
	if fields[0].Sign() == 0 || fields[0].BitLen() > 80 || fields[4].BitLen() > 80 || fields[4].Cmp(fields[0]) < 0 ||
		!fields[2].IsUint64() || !fields[3].IsUint64() || fields[2].Cmp(fields[3]) > 0 || fields[3].Uint64() > r.Reader.Position().Timestamp {
		return nil, fmt.Errorf("oracle round is incomplete or has invalid timestamps")
	}
	if raw[32]&0x80 != 0 {
		fields[1].Sub(fields[1], new(big.Int).Lsh(big.NewInt(1), 256))
	}
	result := &execution.OracleRound{Feed: target, CodeHash: codeHash, RoundID: fields[0], Answer: fields[1],
		StartedAt: fields[2].Uint64(), UpdatedAt: fields[3].Uint64(), AnsweredInRound: fields[4]}
	if price {
		result.Decimals, err = r.decimals(ctx, target)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func power10(decimals uint64) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), new(big.Int).SetUint64(decimals), nil)
}

var _ execution.AssetValuer = (*Valuer)(nil)
