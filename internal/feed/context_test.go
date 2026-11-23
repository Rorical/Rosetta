package feed

import (
	"strings"
	"testing"
)

func TestDecodeFramePreservesCompleteReplayContext(t *testing.T) {
	// The shape follows the pinned Nitro broadcast serialization contract. This
	// is a synthetic wire vector, not a captured mainnet delayed message.
	frame := `{"version":1,"messages":[{"sequenceNumber":12345,"message":{"message":{"header":{"kind":13,"sender":"0x0000000000000000000000000000000000001234","blockNumber":123,"timestamp":456,"requestId":"0x` + strings.Repeat("ab", 32) + `","baseFeeL1":9007199254740993},"l2Msg":"","batchGasCost":72,"batchDataTokens":{"length":5,"nonzeros":3}},"delayedMessagesRead":3333},"blockHash":"` + testHashA + `"}]}`
	decoded, err := DecodeFrame([]byte(frame))
	if err != nil {
		t.Fatal(err)
	}
	message := decoded.Messages[0]
	if message.Header.Poster == nil || message.Header.Poster.Hex() != "0x0000000000000000000000000000000000001234" || message.Header.RequestID == nil || message.Header.RequestID.Hex() != testHashA || message.Header.L1BaseFee.String() != "9007199254740993" {
		t.Fatalf("lost header context: %+v", message.Header)
	}
	if message.DelayedMessagesRead == nil || *message.DelayedMessagesRead != 3333 || message.LegacyBatchGasCost == nil || *message.LegacyBatchGasCost != 72 || message.BatchDataStats == nil || *message.BatchDataStats != (BatchDataStats{Length: 5, NonZeros: 3}) {
		t.Fatalf("lost replay metadata: %+v", message)
	}
	decoded, err = DecodeFrame([]byte(strings.Replace(frame, `"delayedMessagesRead":3333`, `"delayedMessagesRead":0`, 1)))
	if err != nil || decoded.Messages[0].DelayedMessagesRead == nil || *decoded.Messages[0].DelayedMessagesRead != 0 {
		t.Fatalf("explicit zero delayed count lost: %+v %v", decoded, err)
	}
}

func TestDecodeFrameRejectsMalformedExecutionContext(t *testing.T) {
	for _, header := range []string{
		`{"sender":"0x1234"}`,
		`{"requestId":"0x1234"}`,
		`{"baseFeeL1":-1}`,
		`{"baseFeeL1":` + "1" + strings.Repeat("0", 78) + `}`,
	} {
		if _, err := DecodeFrame([]byte(testFrame(testMessage(1, testHashA, header, "")))); err == nil {
			t.Fatalf("accepted malformed execution context %s", header)
		}
	}
}

func TestDecodeFrameDistinguishesNullFeeFromMissingContext(t *testing.T) {
	for _, header := range []string{`{"baseFeeL1":null}`, `{}`} {
		frame, err := DecodeFrame([]byte(testFrame(testMessage(1, testHashA, header, ""))))
		if err != nil {
			t.Fatal(err)
		}
		decoded := frame.Messages[0].Header
		if decoded.L1BaseFee != nil || decoded.L1BaseFeeKnown != strings.Contains(header, "baseFeeL1") {
			t.Fatalf("null and omitted context conflated: %+v", decoded)
		}
	}
}
