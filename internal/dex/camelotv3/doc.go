// Package camelotv3 loads and prices Camelot V3 pools.
//
// Camelot V3 is an Algebra fork: it exposes globalState() instead of V3's
// slot0(), and keeps initialized ticks in tickTable() words indexed by the raw
// tick. Load reads the shallow state; LoadDeep reads the tick table and
// repacks it into the V3-shaped bitmap internal/math consumes. Both are tagged
// PoolKindCamelotV3.
package camelotv3
