package domain

// StateSource distinguishes a verified standard block, a standard intra-block
// working prefix, and a tentative Fast Feed prefix.
type StateSource uint8

const (
	SourceStandard StateSource = iota + 1
	SourceStandardPrefix
	SourceFast
)

// StateVersion identifies the entire execution prefix, not just changed pools.
// Epoch changes on reset or rebase. Fast block numbers are tentative and never
// substitute for Anchor.Hash. A prefix advance invalidates older ready plans.
type StateVersion struct {
	Source      StateSource
	Anchor      BlockRef
	Epoch       uint64
	BlockNumber uint64
	TxIndex     uint64
	PrefixHash  Hash
}
