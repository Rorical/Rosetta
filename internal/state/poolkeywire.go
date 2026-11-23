package state

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// keyFieldSeparator joins the fields of a key that needs more than an address.
// Hex addresses and hashes never contain it, so the encoding is unambiguous.
const keyFieldSeparator = ":"

// encodePoolKeyString renders a key for a wire list that holds only strings,
// such as the dirty set. A key without a pool ID or hooks is the bare address,
// the form version 1 wrote for every pool. A key that carries V4 identity
// appends the kind, pool ID, and hooks so two pools behind one address stay
// distinct.
func encodePoolKeyString(key domain.PoolKey) string {
	if key.PoolID == (domain.Hash{}) && key.Hooks == (domain.Address{}) {
		return key.Address.Hex()
	}
	return strings.Join([]string{
		key.Address.Hex(),
		strconv.FormatUint(uint64(key.Kind), 10),
		key.PoolID.Hex(),
		key.Hooks.Hex(),
	}, keyFieldSeparator)
}

// decodePoolKeyString parses an encoded key. legacy is true for a bare address,
// the version 1 form, whose kind the caller resolves against the decoded pool
// set; the returned key then carries only the address.
func decodePoolKeyString(raw string) (key domain.PoolKey, legacy bool, err error) {
	fields := strings.Split(raw, keyFieldSeparator)
	if len(fields) == 1 {
		address, err := domain.ParseAddress(raw)
		if err != nil {
			return domain.PoolKey{}, false, fmt.Errorf("pool address %q: %v", raw, err)
		}
		return domain.PoolKey{Address: address}, true, nil
	}
	if len(fields) != 4 {
		return domain.PoolKey{}, false, fmt.Errorf("pool key %q: want 4 fields, got %d", raw, len(fields))
	}
	address, err := domain.ParseAddress(fields[0])
	if err != nil {
		return domain.PoolKey{}, false, fmt.Errorf("pool key %q address: %v", raw, err)
	}
	kind, err := strconv.ParseUint(fields[1], 10, 8)
	if err != nil {
		return domain.PoolKey{}, false, fmt.Errorf("pool key %q kind %q: %v", raw, fields[1], err)
	}
	if kind == 0 {
		return domain.PoolKey{}, false, fmt.Errorf("pool key %q: zero kind", raw)
	}
	poolID, err := domain.ParseHash(fields[2])
	if err != nil {
		return domain.PoolKey{}, false, fmt.Errorf("pool key %q pool id: %v", raw, err)
	}
	hooks, err := domain.ParseAddress(fields[3])
	if err != nil {
		return domain.PoolKey{}, false, fmt.Errorf("pool key %q hooks: %v", raw, err)
	}
	return domain.PoolKey{Address: address, Kind: domain.PoolKind(kind), PoolID: poolID, Hooks: hooks}, false, nil
}

// resolvePoolKey maps a decoded key onto the pool set. A complete key must name
// a stored pool. A version 1 key carries only an address, so it resolves to the
// single pool at that address; that document format predates V4, where several
// pools share one address.
func resolvePoolKey(candidate domain.PoolKey, pools map[domain.PoolKey]pool.State) (domain.PoolKey, error) {
	if candidate.Kind != 0 {
		if _, ok := pools[candidate]; !ok {
			return domain.PoolKey{}, fmt.Errorf("unknown pool %s", formatPoolKey(candidate))
		}
		return candidate, nil
	}

	var matched domain.PoolKey
	found := false
	for key := range pools {
		if key.Address != candidate.Address {
			continue
		}
		if found {
			return domain.PoolKey{}, fmt.Errorf("ambiguous pool address %s in a version 1 document", candidate.Address.Hex())
		}
		matched = key
		found = true
	}
	if !found {
		return domain.PoolKey{}, fmt.Errorf("unknown pool address %s", candidate.Address.Hex())
	}
	return matched, nil
}
