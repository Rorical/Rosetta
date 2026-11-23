package discovery

import (
	"encoding/json"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// IndexIdentity binds immutable source identities, not tuning, token filters,
// file paths or provider URLs. Changing selection limits reuses scanned history.
func IndexIdentity(chainID uint64, config Config) domain.Hash {
	emitters := config.Emitters()
	slices.SortFunc(emitters, func(a, b Emitter) int {
		if a.Source < b.Source {
			return -1
		}
		if a.Source > b.Source {
			return 1
		}
		for i := range a.Address {
			if a.Address[i] < b.Address[i] {
				return -1
			}
			if a.Address[i] > b.Address[i] {
				return 1
			}
		}
		return 0
	})
	raw, _ := json.Marshal([]any{chainID, emitters, config.Deployments})
	return eventHash(string(raw))
}

// VerdictIdentity binds saved verification outcomes to the whole discovery
// configuration except the index path: seeds, sources, fee caps and selection
// all decide admission, so any change discards the outcomes.
func VerdictIdentity(chainID uint64, config Config) domain.Hash {
	config.InventoryPath = ""
	raw, _ := json.Marshal([]any{chainID, config, config.Emitters()})
	return eventHash(string(raw))
}

func (c Config) AcceptSpec(spec Spec) bool {
	scan := &scanState{quotes: map[domain.Address]struct{}{}, seen: map[domain.PoolKey]struct{}{}}
	for _, token := range c.QuoteTokens {
		scan.quotes[token] = struct{}{}
	}
	return (&Discoverer{Config: c}).accept(spec, scan)
}
