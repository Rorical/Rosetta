package discovery

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"testing"
)

func TestCreationIndexIdentitySurvivesSelectionTuning(t *testing.T) {
	cfg := Config{Sources: []Source{SourceUniswapV2}}
	before := IndexIdentity(10, cfg)
	cfg.MaxPools = 200
	cfg.InventoryPath = "different-file.json"
	cfg.QuoteTokens = []domain.Address{{1}}
	if IndexIdentity(10, cfg) != before {
		t.Fatal("selection tuning invalidates immutable history")
	}
	cfg.Sources = []Source{SourceUniswapV3}
	if IndexIdentity(10, cfg) == before {
		t.Fatal("source identity was not bound")
	}
}
