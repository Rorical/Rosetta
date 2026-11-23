package discovery

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// The fixture holds real Robinhood code: two tokens of the allowlist-buy
// template (NMTRA, JHV; different immutables, so different code hashes) and
// two ordinary tokens (USDG behind a proxy, WETH).
func TestDenyTokenCodeMatchesTheTemplateOnly(t *testing.T) {
	raw, err := os.ReadFile("testdata/robinhood-token-code.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tokens map[string]struct{ Code string }
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig([]byte(`{"deny_token_code_selectors":["0x0174639d"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, denied := range map[string]bool{"NMTRA": true, "JHV": true, "USDG": false, "WETH": false} {
		code, err := hex.DecodeString(strings.TrimPrefix(fixture.Tokens[name].Code, "0x"))
		if err != nil || len(code) == 0 {
			t.Fatal(name, err)
		}
		if got := config.DeniedTokenCode(code); got != denied {
			t.Errorf("%s denied = %v, want %v", name, got, denied)
		}
	}
	if (Config{}).DeniedTokenCode([]byte{0x63, 0x01, 0x74, 0x63, 0x9d}) {
		t.Error("no configured selector denied code")
	}
	for _, bad := range []string{`["0x0174"]`, `["zz74639d"]`} {
		if _, err := LoadConfig([]byte(`{"deny_token_code_selectors":` + bad + `}`)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
}
