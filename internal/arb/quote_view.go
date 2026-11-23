package arb

import (
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type shallowQuoteState struct {
	state pool.State
	ok    bool
}

type deepQuoteState struct {
	state pool.DeepState
	ok    bool
}

// Only the exact quoter sees this cache. Quoter's read-only state contract lets
// all amount samples share one owned snapshot copy per pool, including misses.
// Each Optimize call has a separate cache, including same-root tick retries.
type quoteView struct {
	memo    *legQuoteMemo
	source  View
	shallow map[domain.PoolKey]shallowQuoteState
	deep    map[domain.PoolKey]deepQuoteState
}

// CachedView copies each pool's state at most once and hands that copy to every
// later read. Quotes and plans built from one view may share it under the
// quoter's read-only state contract; it is not safe for concurrent use.
func CachedView(source View) View {
	return &quoteView{source: source, shallow: make(map[domain.PoolKey]shallowQuoteState), deep: make(map[domain.PoolKey]deepQuoteState)}
}

// SharedDeepView is a view whose tick state can be read without a copy. The
// copy costs about 5% of CPU and most of a reactive plan build; the quoter
// only reads.
type SharedDeepView interface {
	SharedDeep(key domain.PoolKey) (pool.DeepState, bool)
}

// sharedDeep reads tick state for a quote, without a copy where the view allows.
func sharedDeep(view View, key domain.PoolKey) (pool.DeepState, bool) {
	if shared, ok := view.(SharedDeepView); ok {
		return shared.SharedDeep(key)
	}
	return view.Deep(key)
}

var quoteViews = sync.Pool{New: func() any {
	return &quoteView{shallow: make(map[domain.PoolKey]shallowQuoteState), deep: make(map[domain.PoolKey]deepQuoteState)}
}}

func (v *quoteView) Pool(key domain.PoolKey) (pool.State, bool) {
	if cached, exists := v.shallow[key]; exists {
		return cached.state, cached.ok
	}
	if v.source == nil {
		return pool.State{}, false
	}
	state, ok := v.source.Pool(key)
	v.shallow[key] = shallowQuoteState{state, ok}
	return state, ok
}

func (v *quoteView) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	if cached, exists := v.deep[key]; exists {
		return cached.state, cached.ok
	}
	if v.source == nil {
		return pool.DeepState{}, false
	}
	state, ok := sharedDeep(v.source, key)
	v.deep[key] = deepQuoteState{state, ok}
	return state, ok
}

func (v *quoteView) release() {
	oversized := len(v.shallow) > 4096 || len(v.deep) > 4096
	v.source = nil
	v.memo = nil
	clear(v.shallow)
	clear(v.deep)
	if !oversized {
		quoteViews.Put(v)
	}
}
