package arb

import (
	"math/big"
	"testing"
)

func TestPoolRevisionTracksIndependentReverseScreeningCurve(t *testing.T) {
	original := rankPool{ok: true}
	original.legs[0].model.InReserve = big.NewInt(10)
	original.legs[0].model.OutReserve = big.NewInt(20)
	original.legs[1].model.InReserve = big.NewInt(30)
	original.legs[1].model.OutReserve = big.NewInt(40)
	var revision poolRevision
	if !revision.update(original) || revision.update(original) {
		t.Fatal("initial revision identity failed")
	}
	updated := original
	updated.legs[1].model.InReserve = big.NewInt(31)
	if !revision.update(updated) || revision.update(updated) {
		t.Fatal("reverse-only model update reused stale route estimates")
	}
}
