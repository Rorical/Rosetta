package discovery

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
)

// TestConstants pins the byte literals to the hex values verified on Arbitrum
// One, so a transcription error cannot silently point discovery at another
// contract or event.
func TestConstants(t *testing.T) {
	hashes := []struct {
		name string
		text string
		got  domain.Hash
	}{
		{"v3 pool created", "0x783cca1c0412dd0d695e784568c96da2e9c22ff989357a2e8b1d9b2b4e6b7118", topicV3PoolCreated},
		{"slipstream pool created", "0xab0d57f0df537bb25e80245ef7748fa62353808c54d6e528a9dd20887aed9ac2", topicSlipstreamPoolCreated},
		{"v4 initialize", "0xdd466e674ea557f56295e2d0218a125ea4b4f0f6f3307b95f85e6110838d6438", topicV4Initialize},
	}
	for _, test := range hashes {
		want, err := domain.ParseHash(test.text)
		if err != nil {
			t.Fatalf("ParseHash(%s): %v", test.name, err)
		}
		if test.got != want {
			t.Fatalf("%s = %s, want %s", test.name, test.got.Hex(), test.text)
		}
	}

	addresses := []struct {
		name string
		text string
		got  domain.Address
	}{
		{"uniswap v3 factory", "0x1F98431c8aD98523631AE4a59f267346ea31F984", uniswapV3Factory},
		{"pancake v3 factory", "0x0BFbCF9fa4f9C56B0F40a671Ad40E0805A091865", pancakeV3Factory},
		{"slipstream factory", "0x88a60ee77392577c355c0acb2a8d476d5a4ec03a", slipstreamFactory},
		{"v4 pool manager", "0x360e68faccca8ca495c1b759fd9eee466db9fb32", v4PoolManager},
		{"v4 state view", uniswapv4.StateViewAddress, stateView},
	}
	for _, test := range addresses {
		want, err := domain.ParseAddress(test.text)
		if err != nil {
			t.Fatalf("ParseAddress(%s): %v", test.name, err)
		}
		if test.got != want {
			t.Fatalf("%s = %s, want %s", test.name, test.got.Hex(), test.text)
		}
	}
}
