package metricomm

import "github.com/holiman/uint256"

// word is an EVM word. The port keeps the contract's 256-bit arithmetic so
// that unchecked blocks wrap and checked operations revert at the same values.
type word = uint256.Int

var (
	oneX64     = new(word).Lsh(uint256.NewInt(1), 64)
	oneX128    = new(word).Lsh(uint256.NewInt(1), 128)
	maxPosBin  = new(word).SubUint64(new(word).Lsh(uint256.NewInt(1), 104), 1)
	maxUint128 = new(word).SubUint64(new(word).Lsh(uint256.NewInt(1), 128), 1)
	e6         = uint256.NewInt(1_000_000)
	e8         = uint256.NewInt(100_000_000)
	zero       = new(word)
	one        = uint256.NewInt(1)
	two        = uint256.NewInt(2)
	four       = uint256.NewInt(4)
)

// Wrapping operations, as inside a Solidity unchecked block.

func add(a, b *word) *word { return new(word).Add(a, b) }
func sub(a, b *word) *word { return new(word).Sub(a, b) }
func mul(a, b *word) *word { return new(word).Mul(a, b) }
func shl(a *word, bits uint) *word {
	return new(word).Lsh(a, bits)
}

func minWord(a, b *word) *word {
	if a.Lt(b) {
		return a
	}
	return b
}

// evm records the first revert of a computation. After a revert the remaining
// operations still return values, which the caller discards once it sees err;
// this keeps the port linear, like the Solidity it mirrors.
type evm struct {
	err error
}

func (e *evm) revert(err error) {
	if e.err == nil {
		e.err = err
	}
}

// Checked operations, as outside an unchecked block.

func (e *evm) checkedAdd(a, b *word) *word {
	sum, overflow := new(word).AddOverflow(a, b)
	if overflow {
		e.revert(ErrArithmeticOverflow)
	}
	return sum
}

func (e *evm) checkedSub(a, b *word) *word {
	difference, underflow := new(word).SubOverflow(a, b)
	if underflow {
		e.revert(ErrArithmeticOverflow)
	}
	return difference
}

func (e *evm) checkedMul(a, b *word) *word {
	product, overflow := new(word).MulOverflow(a, b)
	if overflow {
		e.revert(ErrArithmeticOverflow)
	}
	return product
}

// div is Solidity's "/": it reverts on a zero divisor even when unchecked.
func (e *evm) div(a, b *word) *word {
	if b.IsZero() {
		e.revert(ErrDivisionByZero)
		return new(word)
	}
	return new(word).Div(a, b)
}

// ceilDiv is OpenZeppelin Math.ceilDiv.
func (e *evm) ceilDiv(a, b *word) *word {
	if b.IsZero() {
		e.revert(ErrDivisionByZero)
		return new(word)
	}
	if a.IsZero() {
		return new(word)
	}
	quotient := new(word).Div(sub(a, one), b)
	return quotient.Add(quotient, one)
}

// mulDiv is OpenZeppelin Math.mulDiv: floor(x*y/d) over a 512-bit product.
func (e *evm) mulDiv(x, y, d *word) *word {
	if d.IsZero() {
		e.revert(ErrDivisionByZero)
		return new(word)
	}
	quotient, overflow := new(word).MulDivOverflow(x, y, d)
	if overflow {
		e.revert(ErrArithmeticOverflow)
		return new(word)
	}
	return quotient
}

// mulDivUp is Math.mulDiv with Rounding.Ceil.
func (e *evm) mulDivUp(x, y, d *word) *word {
	quotient := e.mulDiv(x, y, d)
	if e.err != nil {
		return quotient
	}
	if new(word).MulMod(x, y, d).IsZero() {
		return quotient
	}
	return e.checkedAdd(quotient, one)
}

// toUint104 is SafeCast.toUint104.
func (e *evm) toUint104(value *word) *word {
	if value.Gt(maxPosBin) {
		e.revert(ErrArithmeticOverflow)
	}
	return value
}

// toUint128 is SafeCast.toUint128.
func (e *evm) toUint128(value *word) *word {
	if value.Gt(maxUint128) {
		e.revert(ErrArithmeticOverflow)
	}
	return value
}
