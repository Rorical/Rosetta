package planning

import (
	"math"
	"testing"

	execution "github.com/Rorical/Rosetta/internal/exec"
)

func TestExpiryBindsSnapshotAndOracleValidity(t *testing.T) {
	conversion := execution.FeeConversion{MaxPriceAge: 20, NativePrice: &execution.OracleRound{UpdatedAt: 990}, AssetPrice: &execution.OracleRound{UpdatedAt: 995}}
	expires, err := Expiry(1_000_000, 1015, 30, conversion)
	if err != nil || expires != 1_010_000 {
		t.Fatal("oracle limit missing", expires, err)
	}
	for _, test := range []struct {
		created            int64
		deadline, lifetime uint64
	}{
		{1_010_000, 1030, 30},
		{1_000_000, 1000, 30},
		{math.MaxInt64 - 10, uint64(math.MaxInt64 / 1000), 1},
	} {
		if _, err := Expiry(test.created, test.deadline, test.lifetime, conversion); err == nil {
			t.Fatal("expired or overflowing plan accepted", test)
		}
	}
}
