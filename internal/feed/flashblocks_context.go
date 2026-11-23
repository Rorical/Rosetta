package feed

import (
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// FlashExecution carries the header fields required to replay a signed prefix.
// Missing context leaves the stream usable for discovery, but not confirmation.
type FlashExecution struct {
	Coinbase           domain.Address
	Random, BeaconRoot domain.Hash
	GasLimit           uint64
	BaseFee            *big.Int
	Extra              []byte
}

func decodeFlashExecution(raw json.RawMessage) (*FlashExecution, error) {
	var wire struct {
		Coinbase string  `json:"fee_recipient"`
		Random   string  `json:"prev_randao"`
		Beacon   string  `json:"parent_beacon_block_root"`
		Gas      string  `json:"gas_limit"`
		Fee      string  `json:"base_fee_per_gas"`
		Extra    *string `json:"extra_data"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	if wire.Coinbase == "" && wire.Random == "" && wire.Beacon == "" && wire.Gas == "" && wire.Fee == "" && wire.Extra == nil {
		return nil, nil
	}
	coinbase, err := domain.ParseAddress(wire.Coinbase)
	if err != nil {
		return nil, err
	}
	random, err := domain.ParseHash(wire.Random)
	if err != nil {
		return nil, err
	}
	beacon, err := domain.ParseHash(wire.Beacon)
	if err != nil {
		return nil, err
	}
	gas, err := flashQuantity(wire.Gas)
	if err != nil || gas == 0 {
		return nil, fmt.Errorf("flashblocks: invalid execution gas limit")
	}
	fee, ok := new(big.Int).SetString(wire.Fee, 0)
	if !ok || fee.Sign() <= 0 || fee.BitLen() > 256 || wire.Extra == nil {
		return nil, fmt.Errorf("flashblocks: incomplete execution context")
	}
	extra, err := flashBytes(*wire.Extra)
	if err != nil || len(extra) > 32 {
		return nil, fmt.Errorf("flashblocks: invalid extra data")
	}
	return &FlashExecution{Coinbase: coinbase, Random: random, BeaconRoot: beacon, GasLimit: gas, BaseFee: fee, Extra: extra}, nil
}
