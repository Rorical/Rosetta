package remote

import (
	"fmt"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Priority samples have their own rolling admission budget. They never bypass
// provider backoff or replace a running execution group.
type paperPriorityPolicy struct {
	MinNativeGrossWei string `json:"min_native_gross_wei"`
	MaxPerMinute      int    `json:"max_per_minute"`
	MinIntervalMS     int64  `json:"min_interval_ms"`
	RouteCooldownMS   int64  `json:"route_cooldown_ms"`
}

type paperPriorityGate struct {
	policy   paperPriorityPolicy
	minimum  *big.Int
	admitted []time.Time
	routes   map[domain.Hash]time.Time
}

func (g *paperPriorityGate) configure(policy *paperPriorityPolicy) error {
	if policy == nil {
		return nil
	}
	minimum, ok := new(big.Int).SetString(policy.MinNativeGrossWei, 10)
	if !ok || minimum.Sign() <= 0 || minimum.BitLen() > 256 || policy.MaxPerMinute < 1 || policy.MaxPerMinute > 60 || policy.MinIntervalMS < 1000 || policy.MinIntervalMS > 60000 || policy.RouteCooldownMS < policy.MinIntervalMS || policy.RouteCooldownMS > 3600000 {
		return fmt.Errorf("invalid execution paper priority budget")
	}
	g.policy, g.minimum = *policy, minimum
	g.routes = make(map[domain.Hash]time.Time)
	return nil
}

func (g *paperPriorityGate) reason(now time.Time, route domain.Hash) string {
	if until, ok := g.routes[route]; ok && now.Before(until) {
		return "priority_route_cooldown"
	}
	if n := len(g.admitted); n > 0 && now.Before(g.admitted[n-1].Add(time.Duration(g.policy.MinIntervalMS)*time.Millisecond)) {
		return "priority_interval"
	}
	count := 0
	for _, at := range g.admitted {
		if now.Before(at.Add(time.Minute)) {
			count++
		}
	}
	if count >= g.policy.MaxPerMinute {
		return "priority_budget"
	}
	return ""
}

func (g *paperPriorityGate) admit(now time.Time, route domain.Hash) {
	keep := g.admitted[:0]
	for _, at := range g.admitted {
		if now.Before(at.Add(time.Minute)) {
			keep = append(keep, at)
		}
	}
	g.admitted = append(keep, now)
	for key, until := range g.routes {
		if !now.Before(until) {
			delete(g.routes, key)
		}
	}
	g.routes[route] = now.Add(time.Duration(g.policy.RouteCooldownMS) * time.Millisecond)
}
