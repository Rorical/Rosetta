package remote

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// A refill unwraps what the wallet already holds, sweeps only what it lacks,
// and stops once the balance is back above the threshold.
func TestRefillStep(t *testing.T) {
	below, target := big.NewInt(50), big.NewInt(200)
	n := func(v int64) *big.Int { return big.NewInt(v) }
	for _, c := range []struct {
		native, wallet, executor int64
		kind                     string
		amount                   int64
	}{
		{native: 60, wallet: 500, executor: 500},                                               // above the threshold
		{native: 40, wallet: 160, executor: 0, kind: execution.MaintenanceUnwrap, amount: 160}, // the wallet covers the need
		{native: 40, wallet: 100, executor: 500, kind: execution.MaintenanceSweep, amount: 60}, // sweep only the shortfall
		{native: 40, wallet: 0, executor: 30, kind: execution.MaintenanceSweep, amount: 30},    // sweep what there is
		{native: 40, wallet: 30, executor: 0, kind: execution.MaintenanceUnwrap, amount: 30},   // a partial refill
		{native: 40, wallet: 0, executor: 0},                                                   // nothing to refill
	} {
		kind, amount := refillStep(n(c.native), below, target, n(c.wallet), n(c.executor))
		if kind != c.kind || (amount == nil) != (c.kind == "") || amount != nil && amount.Int64() != c.amount {
			t.Errorf("%+v: %s %v", c, kind, amount)
		}
	}
}

// The refill must start above the transaction budget, where the context
// refresh would already stop trading, and needs wrapped native as an asset.
func TestNativeRefillPolicy(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "robinhood-reactive.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain := domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 4663}
	load := func(refill string) (reactiveSettings, error) {
		text := strings.Replace(string(example), `"below": "5000000000000000"`, refill, 1)
		path := filepath.Join(t.TempDir(), "reactive.json")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadReactiveSettings(path, chain)
	}
	s, err := load(`"below": "5000000000000000"`)
	if err != nil || s.refillBelow == nil || s.limits.Wrapped != s.wrapped {
		t.Fatal("example refill not loaded", err)
	}
	for _, bad := range []string{`"below": "10000000000000"`, `"below": "30000000000000000"`} {
		if _, err := load(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
