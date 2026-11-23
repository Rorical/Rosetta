package remote

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestReactivePolicyBidMinWinProbability(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "reactive.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	load := func(field string) (reactiveSettings, error) {
		text := string(example)
		if field != "" {
			text = strings.Replace(text, `"mode": "shadow",`, `"mode": "shadow", `+field+`,`, 1)
		}
		path := filepath.Join(t.TempDir(), "reactive.json")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadReactiveSettings(path, chain)
	}
	cases := []struct {
		field string
		want  float64
		ok    bool
	}{
		{"", defaultMinWinProbability, true},
		{`"bid_min_win_probability": 0`, 0, true}, // the gate off
		{`"bid_min_win_probability": 0.35`, 0.35, true},
		{`"bid_min_win_probability": 1`, 0, false},
		{`"bid_min_win_probability": -0.1`, 0, false},
	}
	for _, c := range cases {
		s, err := load(c.field)
		if (err == nil) != c.ok || c.ok && s.minWin != c.want {
			t.Errorf("%q: min win %v, error %v", c.field, s.minWin, err)
		}
	}
	// The deployed executor decodes only packed version 2: bounds stay opt-in.
	if s, err := load(""); err != nil || s.planning.PricePrecheck {
		t.Fatal("price precheck on by default", err)
	}
	if s, err := load(`"price_precheck": true`); err != nil || !s.planning.PricePrecheck {
		t.Fatal("price precheck not loaded", err)
	}
}

// The Nitro examples load for their own chains.
func TestNitroReactiveExamplesLoad(t *testing.T) {
	for name, chain := range map[string]domain.ChainIdentity{
		"arbitrum-one-reactive.example.json": {Family: domain.FamilyNitro, ChainID: 42161},
		"robinhood-reactive.example.json":    {Family: domain.FamilyNitro, ChainID: 4663},
	} {
		if _, err := loadReactiveSettings(filepath.Join("..", "..", "configs", name), chain); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// OP Stack's conditional semantics are unmeasured: hot_path loads only on Nitro.
func TestReactiveHotPathNeedsNitro(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "arbitrum-one-reactive.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reactive.json")
	text := strings.Replace(string(example), `"price_precheck": true,`, `"price_precheck": true, "hot_path": true, "hot_chain": true,`, 1)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := loadReactiveSettings(path, domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}); err != nil || !s.planning.HotPath {
		t.Fatal("hot_path not loaded on Nitro", err)
	}
	if _, err := loadReactiveSettings(path, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}); err == nil {
		t.Fatal("hot_path accepted on OP Stack")
	}
}

// Extra lanes need their own sender, secret and journal; the daily budget is
// split evenly so the lanes together stay within it.
func TestReactiveLanesSplitTheDailyBudget(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "arbitrum-one-reactive.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	chain := domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}
	load := func(lanes string) (reactiveSettings, error) {
		text := strings.Replace(string(example), `"price_precheck": true,`, `"price_precheck": true, "lanes": `+lanes+`,`, 1)
		path := filepath.Join(t.TempDir(), "reactive.json")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadReactiveSettings(path, chain)
	}
	single, err := load(`[]`)
	if err != nil {
		t.Fatal(err)
	}
	lane := `{"sender": "0x00000000000000000000000000000000000000e1", "secret_id": "operator-2", "journal": "/tmp/rosetta-lane-2.json"}`
	two, err := load(`[` + lane + `]`)
	if err != nil {
		t.Fatal(err)
	}
	half := new(big.Int).Quo(single.limits.PerDay, big.NewInt(2))
	if len(two.lanes) != 1 || two.limits.PerDay.Cmp(half) != 0 || two.lanes[0].limits.PerDay.Cmp(half) != 0 || two.lanes[0].limits.Sender.Hex() != "0x00000000000000000000000000000000000000e1" || two.lanes[0].limits.Executor != two.limits.Executor {
		t.Fatalf("lane limits %+v", two.lanes)
	}
	for name, lanes := range map[string]string{
		"repeated sender": `[` + lane + `,` + lane + `]`,
		"primary sender":  strings.Replace(`[`+lane+`]`, "0x00000000000000000000000000000000000000e1", single.planning.Sender.Hex(), 1),
		"no secret":       `[{"sender": "0x00000000000000000000000000000000000000e1", "journal": "/tmp/x.json"}]`,
		"primary journal": `[{"sender": "0x00000000000000000000000000000000000000e1", "secret_id": "s", "journal": "` + single.document.Journal + `"}]`,
	} {
		if _, err := load(lanes); err == nil {
			t.Error(name, "accepted")
		}
	}
}
