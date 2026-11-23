package feed

import (
	"fmt"
	"net/http"
	"strconv"
)

// These handshake versions follow the pinned Nitro transport contract. They
// are separate from the v1 JSON frame envelope version.
func validateHandshake(headers http.Header, required bool, chainID uint64) error {
	for _, field := range []struct {
		name     string
		expected uint64
	}{
		{"Arbitrum-Feed-Server-Version", 2}, {"Arbitrum-Chain-Id", chainID},
	} {
		value := headers.Get(field.name)
		if value == "" && !required {
			continue
		}
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed != field.expected {
			return &protocolError{reason: fmt.Sprintf("missing or incompatible %s", field.name)}
		}
	}
	return nil
}

type protocolError struct{ reason string }

func (e *protocolError) Error() string { return "feed: protocol: " + e.reason }
