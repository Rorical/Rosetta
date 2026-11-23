package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"runtime"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
)

// SearchFunding fixes exact-search bounds and the coarse ranking input.
type SearchFunding struct {
	Bounds arb.InputBounds
	Seed   *big.Int
}

// SearchPolicy configures topology and per-asset search budgets for either backend.
// ExactK is how many of an asset's K ranked candidates per job get the full
// amount search; the rest keep only their initial one-amount quote.
type SearchPolicy struct {
	K, ExactK, Depth, MaxCycles int
	// MaxCyclesSource says how an "auto" max_cycles was sized; empty when the
	// config fixed it.
	MaxCyclesSource string
	// ExplorePaths caps the background explorer's retained path cache; zero is
	// 8192. More cached paths trade memory and per-search CPU for coverage on
	// dense graphs.
	ExplorePaths int
	// FastCoarseCycles bounds the fast lane's ranked routes per dirty pool;
	// zero ranks them all. The full search worker is never bounded.
	FastCoarseCycles int
	// FastLaneHops is the longest route the fast lane holds; zero is three.
	// Longer routes rank behind shorter ones under FastCoarseCycles, except
	// routes the full search recently found profitable.
	FastLaneHops int
	// FastLaneQuotes is how many routes per funding asset the fast lane quotes
	// exactly after its coarse rank; zero is two.
	FastLaneQuotes int
	WordRadius     int16
	Funding        map[domain.Address]SearchFunding
}

// LoadSearchPolicy validates topology limits and uint256 funding ranges.
func LoadSearchPolicy(path string) (SearchPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SearchPolicy{}, fmt.Errorf("read search config: %w", err)
	}
	var document struct {
		K          int             `json:"k"`
		ExactK     *int            `json:"exact_k"`
		Depth      int             `json:"max_depth"`
		MaxCycles  json.RawMessage `json:"max_cycles"`
		Explore    int             `json:"explore_paths"`
		FastCoarse int             `json:"fast_coarse_cycles"`
		FastHops   int             `json:"fast_lane_hops"`
		FastQuotes int             `json:"fast_lane_quotes"`
		WordRadius int16           `json:"word_radius"`
		Funding    []struct {
			Token string `json:"token"`
			Min   string `json:"min_input"`
			Max   string `json:"max_input"`
			Seed  string `json:"seed_input"`
			// MinGross is the smallest gross, in token units, worth ranking or
			// publishing; set it below the cheapest executor transaction.
			MinGross string `json:"min_gross,omitempty"`
		} `json:"funding"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return SearchPolicy{}, fmt.Errorf("decode search config: %w", err)
	}
	if document.K <= 0 || document.Depth < 2 || document.Depth > 16 || len(document.Funding) == 0 {
		return SearchPolicy{}, errors.New("search config requires positive k, max_depth in [2,16] and funding assets")
	}
	maxCycles, source, err := parseMaxCycles(document.MaxCycles, document.Depth)
	if err != nil {
		return SearchPolicy{}, err
	}
	// Omitted or zero keeps the historical cache size.
	if document.Explore == 0 {
		document.Explore = defaultExplorePaths
	}
	if document.Explore < 1024 || document.Explore > 262144 {
		return SearchPolicy{}, errors.New("search explore_paths must be in [1024,262144]")
	}
	if document.FastCoarse < 0 {
		return SearchPolicy{}, errors.New("search fast_coarse_cycles must not be negative")
	}
	if document.FastHops != 0 && (document.FastHops < 2 || document.FastHops > document.Depth) {
		return SearchPolicy{}, errors.New("search fast_lane_hops must be in [2,max_depth]")
	}
	if document.FastQuotes < 0 || document.FastQuotes > document.K {
		return SearchPolicy{}, errors.New("search fast_lane_quotes must be in [0,k]")
	}
	if document.WordRadius == 0 {
		document.WordRadius = 1
	}
	if document.WordRadius < 1 || document.WordRadius > 64 {
		return SearchPolicy{}, errors.New("search word_radius must be in [1,64]")
	}
	// An omitted exact_k sizes every ranked candidate, as before the option.
	exactK := document.K
	if document.ExactK != nil {
		exactK = *document.ExactK
	}
	if exactK < 1 || exactK > document.K {
		return SearchPolicy{}, errors.New("search exact_k must be in [1,k]")
	}
	cfg := SearchPolicy{K: document.K, ExactK: exactK, Depth: document.Depth, MaxCycles: maxCycles, MaxCyclesSource: source, ExplorePaths: document.Explore, FastCoarseCycles: document.FastCoarse, FastLaneHops: document.FastHops, FastLaneQuotes: document.FastQuotes, WordRadius: document.WordRadius, Funding: make(map[domain.Address]SearchFunding)}
	for i, entry := range document.Funding {
		token, err := domain.ParseAddress(entry.Token)
		if err != nil {
			return SearchPolicy{}, fmt.Errorf("search Funding %d token: %w", i, err)
		}
		minimum, minOK := new(big.Int).SetString(entry.Min, 10)
		maximum, maxOK := new(big.Int).SetString(entry.Max, 10)
		Seed, seedOK := new(big.Int).SetString(entry.Seed, 10)
		if !minOK || !maxOK || !seedOK || minimum.Sign() <= 0 || minimum.Cmp(Seed) > 0 || Seed.Cmp(maximum) > 0 || maximum.BitLen() > 256 {
			return SearchPolicy{}, fmt.Errorf("search Funding %d requires 0 < min_input <= seed_input <= max_input <= uint256", i)
		}
		if _, exists := cfg.Funding[token]; exists {
			return SearchPolicy{}, fmt.Errorf("duplicate search Funding token %s", token)
		}
		var floor *big.Int
		if entry.MinGross != "" {
			var ok bool
			if floor, ok = new(big.Int).SetString(entry.MinGross, 10); !ok || floor.Sign() <= 0 || floor.BitLen() > 256 {
				return SearchPolicy{}, fmt.Errorf("search Funding %d min_gross must be a positive uint256", i)
			}
		}
		cfg.Funding[token] = SearchFunding{Bounds: arb.InputBounds{Min: minimum, Max: maximum, Floor: floor}, Seed: Seed}
	}
	return cfg, nil
}

// parseMaxCycles takes a positive route count, or "auto" to size the table
// from this host's memory limit and GOMAXPROCS (AutoMaxCycles).
func parseMaxCycles(raw json.RawMessage, depth int) (int, string, error) {
	var auto string
	if json.Unmarshal(raw, &auto) == nil {
		if auto != "auto" {
			return 0, "", fmt.Errorf("search max_cycles must be a positive integer or \"auto\", got %q", auto)
		}
		limit, source, err := MemoryLimit()
		if err != nil {
			return 0, "", fmt.Errorf("search max_cycles auto: %w", err)
		}
		cores := runtime.GOMAXPROCS(0)
		return max(1, AutoMaxCycles(limit, cores, depth)), fmt.Sprintf("auto: %s %d MiB, %d cores", source, limit>>20, cores), nil
	}
	var count int
	if err := json.Unmarshal(raw, &count); err != nil || count < 1 || count > maxRouteBudget {
		return 0, "", fmt.Errorf("search max_cycles must be in [1,%d] or \"auto\"", maxRouteBudget)
	}
	return count, "", nil
}

func (c SearchPolicy) Bounds(token domain.Address) (arb.InputBounds, bool) {
	Funding, ok := c.Funding[token]
	return Funding.Bounds, ok
}

func (c SearchPolicy) Notional(token domain.Address) *big.Int {
	return c.Funding[token].Seed
}
