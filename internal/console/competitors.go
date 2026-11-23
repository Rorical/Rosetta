package console

import (
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

type poolRef struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
}

type competitorLine struct {
	Type         string           `json:"type"`
	Block        uint64           `json:"block"`
	Frame        uint64           `json:"frame"`
	Index        uint64           `json:"tx_index"`
	Hash         domain.Hash      `json:"hash"`
	From         domain.Address   `json:"from"`
	Own          bool             `json:"own"`
	Tip          *big.Int         `json:"tip_per_gas"`
	Success      bool             `json:"success"`
	Pools        []domain.PoolKey `json:"pools"`
	ReceivedAtNS int64            `json:"received_at_ns"`
}

type rivalRow struct {
	Block, Frame, Index uint64
	Hash, Sender, Tip   string
	TipValue            float64
	Success             bool
	Pools               []poolRef
	ReceivedMS          int64
}

func parseCompetitor(line []byte, into *batch) error {
	var record competitorLine
	if err := json.Unmarshal(line, &record); err != nil {
		return fmt.Errorf("decode competitor record: %w", err)
	}
	// Our own transactions come from the reactive output with more detail.
	if record.Type != "competitor_tx" || record.Own {
		return nil
	}
	tip := orZero(record.Tip)
	into.rivals = append(into.rivals, rivalRow{Block: record.Block, Frame: record.Frame, Index: record.Index, Hash: record.Hash.Hex(), Sender: record.From.Hex(),
		Tip: tip.String(), TipValue: approximate(tip), Success: record.Success, Pools: poolRefs(record.Pools), ReceivedMS: record.ReceivedAtNS / 1e6})
	return nil
}

func poolRefs(keys []domain.PoolKey) []poolRef {
	refs := make([]poolRef, 0, len(keys))
	for _, key := range keys {
		ref := poolRef{Address: key.Address.Hex(), Kind: poolKindName(key.Kind)}
		// One V4 manager address hosts every pool, so the ID identifies the pool.
		if key.IsV4() {
			ref.Address = key.PoolID.Hex()
		}
		refs = append(refs, ref)
	}
	return refs
}

func poolKindName(kind domain.PoolKind) string {
	switch kind {
	case domain.PoolKindV2:
		return "V2"
	case domain.PoolKindV3:
		return "V3"
	case domain.PoolKindV4:
		return "V4"
	case domain.PoolKindCamelotV3:
		return "Camelot V3"
	case domain.PoolKindSlipstream:
		return "Slipstream"
	case domain.PoolKindVelodromeV2:
		return "Velodrome V2"
	case domain.PoolKindSolidlyV3:
		return "Solidly V3"
	case domain.PoolKindMetricOmm:
		return "Metric OMM"
	case domain.PoolKindWrap:
		return "Wrap"
	default:
		return fmt.Sprintf("kind %d", uint8(kind))
	}
}
