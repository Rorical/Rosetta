// Package planning builds execution plans over portable, fixed-position readers.
package planning

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// AssetPolicy fixes funding and profit denomination for one starting asset.
type AssetPolicy struct {
	Funding                  execution.Funding
	Lender                   domain.PoolKey
	MaxLoanFee, MinNetProfit *big.Int
	PriceFeed                domain.Address
	MaxPriceAge              uint64
}

// Policy is immutable after loading and shared by one runtime's confirmation jobs.
type Policy struct {
	TokenCheck               func(domain.Address) error
	PoolCheck                func(domain.PoolKey) error // quarantined pools
	Sender, Executor         domain.Address
	CodeHash, PolicyHash     domain.Hash
	GasLimit                 uint64
	MaxFee, Tip, MaxTotalFee *big.Int
	FeeMultiplier            uint64
	Slippage                 uint64
	ValidSeconds             uint64
	MaxCandidates, TimeoutMS int
	NativePrice, Sequencer   domain.Address
	Grace                    uint64
	Assets                   map[domain.Address]AssetPolicy
}

// LoadPolicy validates the complete policy and hashes its exact source bytes.
func LoadPolicy(path string) (Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, fmt.Errorf("read execution config: %w", err)
	}
	var document struct {
		Sender        string `json:"sender"`
		Executor      string `json:"executor"`
		CodeHash      string `json:"code_hash"`
		GasLimit      uint64 `json:"gas_limit"`
		MaxFee        string `json:"max_fee_per_gas_wei"`
		Tip           string `json:"tip_per_gas_wei"`
		MaxTotalFee   string `json:"max_total_fee_wei"`
		FeeMultiplier uint64 `json:"fee_multiplier_bps"`
		Slippage      uint64 `json:"slippage_bps"`
		ValidSeconds  uint64 `json:"valid_for_seconds"`
		MaxCandidates int    `json:"max_candidates"`
		TimeoutMS     int    `json:"timeout_ms"`
		NativePrice   string `json:"native_usd_feed"`
		Sequencer     string `json:"sequencer_feed"`
		Grace         uint64 `json:"sequencer_grace_seconds"`
		Assets        []struct {
			Token        string `json:"token"`
			Funding      string `json:"funding"`
			Lender       string `json:"lender"`
			MaxLoanFee   string `json:"max_loan_fee"`
			MinNetProfit string `json:"min_net_profit"`
			PriceFeed    string `json:"usd_feed"`
			MaxPriceAge  uint64 `json:"max_price_age_seconds"`
		} `json:"assets"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return Policy{}, fmt.Errorf("decode execution config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Policy{}, fmt.Errorf("execution config requires one JSON object")
	}
	cfg := Policy{GasLimit: document.GasLimit, FeeMultiplier: document.FeeMultiplier,
		Slippage: document.Slippage, ValidSeconds: document.ValidSeconds, MaxCandidates: document.MaxCandidates,
		TimeoutMS: document.TimeoutMS, Grace: document.Grace, Assets: make(map[domain.Address]AssetPolicy),
		PolicyHash: domain.Hash(crypto.Keccak256Hash(raw))}
	if cfg.GasLimit < 21_000 || cfg.GasLimit > 32_000_000 || cfg.FeeMultiplier < 10_000 || cfg.FeeMultiplier > 100_000 ||
		cfg.Slippage >= 10_000 || cfg.ValidSeconds == 0 || cfg.ValidSeconds > 30 || cfg.MaxCandidates < 1 || cfg.MaxCandidates > 64 ||
		cfg.TimeoutMS < 1 || cfg.TimeoutMS > 1000 || len(document.Assets) == 0 || len(document.Assets) > 64 {
		return Policy{}, fmt.Errorf("invalid execution gas, fee multiplier, slippage, validity, candidate or timeout limits")
	}
	for _, field := range []struct {
		text     string
		out      *domain.Address
		required bool
	}{
		{document.Sender, &cfg.Sender, true}, {document.Executor, &cfg.Executor, true},
		{document.NativePrice, &cfg.NativePrice, false}, {document.Sequencer, &cfg.Sequencer, false},
	} {
		if field.text == "" && !field.required {
			continue
		}
		value, err := domain.ParseAddress(field.text)
		if err != nil || value == (domain.Address{}) {
			return Policy{}, fmt.Errorf("invalid execution address %q", field.text)
		}
		*field.out = value
	}
	cfg.CodeHash, err = domain.ParseHash(document.CodeHash)
	if err != nil || cfg.CodeHash == (domain.Hash{}) {
		return Policy{}, fmt.Errorf("execution requires a nonzero executor code hash")
	}
	for _, field := range []struct {
		text string
		out  **big.Int
		zero bool
	}{
		{document.MaxFee, &cfg.MaxFee, false}, {document.Tip, &cfg.Tip, true}, {document.MaxTotalFee, &cfg.MaxTotalFee, false},
	} {
		*field.out, err = Integer(field.text, field.zero)
		if err != nil {
			return Policy{}, err
		}
	}
	if cfg.Tip.Cmp(cfg.MaxFee) > 0 {
		return Policy{}, fmt.Errorf("execution tip exceeds maximum fee")
	}
	for _, entry := range document.Assets {
		token, err := domain.ParseAddress(entry.Token)
		if err != nil {
			return Policy{}, fmt.Errorf("execution asset: %w", err)
		}
		if _, duplicate := cfg.Assets[token]; duplicate {
			return Policy{}, fmt.Errorf("duplicate execution asset %s", token)
		}
		asset := AssetPolicy{MaxPriceAge: entry.MaxPriceAge}
		asset.MinNetProfit, err = Integer(entry.MinNetProfit, false)
		if err != nil {
			return Policy{}, err
		}
		asset.MaxLoanFee, err = Integer(entry.MaxLoanFee, true)
		if err != nil {
			return Policy{}, err
		}
		switch entry.Funding {
		case "wallet":
			if entry.Lender != "" || asset.MaxLoanFee.Sign() != 0 {
				return Policy{}, fmt.Errorf("wallet asset has lender or loan fee")
			}
		case "v3_flash":
			asset.Funding = execution.FundingV3Flash
			lender, err := domain.ParseAddress(entry.Lender)
			if err != nil || lender == (domain.Address{}) || token == (domain.Address{}) {
				return Policy{}, fmt.Errorf("flash asset needs an ERC20 and V3 lender")
			}
			asset.Lender = domain.PoolKey{Kind: domain.PoolKindV3, Address: lender}
		case "v3_flash_swap":
			if entry.Lender != "" || asset.MaxLoanFee.Sign() != 0 || token == (domain.Address{}) {
				return Policy{}, fmt.Errorf("flash swap asset requires an ERC20, no lender and zero additional loan fee")
			}
			asset.Funding = execution.FundingV3FlashSwap
		case "v4_flash":
			if entry.Lender != "" || asset.MaxLoanFee.Sign() != 0 {
				return Policy{}, fmt.Errorf("V4 flash accounting requires no lender and zero additional loan fee")
			}
			asset.Funding = execution.FundingV4Flash
		default:
			return Policy{}, fmt.Errorf("unknown execution funding %q", entry.Funding)
		}
		if token != (domain.Address{}) {
			asset.PriceFeed, err = domain.ParseAddress(entry.PriceFeed)
			if err != nil || asset.PriceFeed == (domain.Address{}) || cfg.NativePrice == (domain.Address{}) || cfg.Sequencer == (domain.Address{}) ||
				asset.MaxPriceAge == 0 || cfg.Grace == 0 {
				return Policy{}, fmt.Errorf("ERC20 execution requires native/asset USD feeds, price age and sequencer grace")
			}
		}
		cfg.Assets[token] = asset
	}
	return cfg, nil
}

// Integer accepts a decimal uint256, optionally including zero.
func Integer(text string, zero bool) (*big.Int, error) {
	value, ok := new(big.Int).SetString(text, 10)
	if !ok || value.Sign() < 0 || (!zero && value.Sign() == 0) || value.BitLen() > 256 {
		return nil, fmt.Errorf("invalid execution uint256 %q", text)
	}
	return value, nil
}
