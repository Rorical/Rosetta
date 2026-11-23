package camelotv3

import "fmt"

const SelectorTickSpacing = "0xd0c93a7c"

type Metadata struct {
	Fee, FeeOneForZero uint32
	TickSpacing        int32
}

// ParseMetadata reads current spacing and both fee directions from the same
// snapshot. A six-word symmetric layout reports the same fee in both directions.
func ParseMetadata(globalOut, spacingOut string) (Metadata, error) {
	global, err := parseGlobalState(globalOut)
	if err != nil {
		return Metadata{}, err
	}
	spacing, err := ParseTickSpacing(spacingOut)
	if err != nil {
		return Metadata{}, err
	}
	metadata := Metadata{Fee: global.fee, FeeOneForZero: global.fee, TickSpacing: spacing}
	if global.reverse != nil {
		metadata.FeeOneForZero = *global.reverse
	}
	return metadata, nil
}

func ParseTickSpacing(out string) (int32, error) {
	words, err := decodeWords("tickSpacing", out, 1)
	if err != nil {
		return 0, err
	}
	spacing, err := signedWord("tickSpacing", words[0], 24)
	if err != nil {
		return 0, err
	}
	if spacing.Sign() <= 0 {
		return 0, fmt.Errorf("%w: nonpositive tick spacing", ErrInvalidResponse)
	}
	return int32(spacing.Int64()), nil
}
