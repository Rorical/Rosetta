package feed

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

var (
	testHashA = "0x" + strings.Repeat("ab", 32)
	testHashB = "0x" + strings.Repeat("cd", 32)
	testBody  = []byte{0x03, 0x04, 0xde, 0xad}
)

func testMessage(sequence uint64, blockHash string, header string, encodedL2Msg string) string {
	return fmt.Sprintf(
		`{"sequenceNumber":%d,"blockHash":%q,"message":{"message":{"header":%s,"l2Msg":%q}}}`,
		sequence, blockHash, header, encodedL2Msg,
	)
}

func testFrame(messages ...string) string {
	return fmt.Sprintf(`{"version":1,"messages":[%s]}`, strings.Join(messages, ","))
}

func TestDecodeFrameValid(t *testing.T) {
	payload := testFrame(
		testMessage(496933984, testHashA, `{"kind":3,"blockNumber":519141801,"timestamp":1761744000,"sender":"0x0000000000000000000000000000000000001234"}`, base64.StdEncoding.EncodeToString(testBody)),
		testMessage(496933985, testHashB, `{}`, ""),
	)

	frame, err := DecodeFrame([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if frame.Version != 1 {
		t.Fatalf("Version = %d, want 1", frame.Version)
	}
	if len(frame.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(frame.Messages))
	}

	wantHash, err := domain.ParseHash(testHashA)
	if err != nil {
		t.Fatalf("ParseHash: %v", err)
	}
	first := frame.Messages[0]
	if first.Sequence != 496933984 {
		t.Fatalf("Sequence = %d, want 496933984", first.Sequence)
	}
	if first.BlockHash != wantHash {
		t.Fatalf("BlockHash = %s, want %s", first.BlockHash.Hex(), testHashA)
	}
	if first.Header.Kind != 3 || first.Header.L1BlockNumber != 519141801 || first.Header.Timestamp != 1761744000 {
		t.Fatalf("Header = %+v, want kind 3, L1 block 519141801, timestamp 1761744000", first.Header)
	}
	if !bytes.Equal(first.L2Msg, testBody) {
		t.Fatalf("L2Msg = %x, want %x", first.L2Msg, testBody)
	}

	second := frame.Messages[1]
	if second.Sequence != 496933985 {
		t.Fatalf("second Sequence = %d, want 496933985", second.Sequence)
	}
	if second.Header != (Header{}) {
		t.Fatalf("second Header = %+v, want zero value", second.Header)
	}
	if second.L2Msg != nil {
		t.Fatalf("second L2Msg = %v, want nil", second.L2Msg)
	}
}

func TestDecodeFrameErrors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{
			name:    "malformed json",
			payload: `{"version":1`,
			wantErr: "decode frame",
		},
		{
			name:    "missing block hash",
			payload: testFrame(`{"sequenceNumber":1,"message":{"message":{}}}`),
			wantErr: "blockHash",
		},
		{
			name:    "empty block hash",
			payload: testFrame(testMessage(1, "", `{}`, "")),
			wantErr: "blockHash",
		},
		{
			name:    "bad block hash hex",
			payload: testFrame(testMessage(1, "0x"+strings.Repeat("zz", 32), `{}`, "")),
			wantErr: "blockHash",
		},
		{
			name:    "bad l2 msg base64",
			payload: testFrame(testMessage(1, testHashA, `{}`, "not base64!")),
			wantErr: "l2Msg",
		},
		{
			name: "duplicate sequence",
			payload: testFrame(
				testMessage(7, testHashA, `{}`, ""),
				testMessage(7, testHashB, `{}`, ""),
			),
			wantErr: "sequence",
		},
		{
			name: "decreasing sequence",
			payload: testFrame(
				testMessage(8, testHashA, `{}`, ""),
				testMessage(7, testHashB, `{}`, ""),
			),
			wantErr: "sequence",
		},
		{
			name:    "non-numeric header field",
			payload: testFrame(`{"sequenceNumber":1,"blockHash":"` + testHashA + `","message":{"message":{"header":{"blockNumber":"519141801"}}}}`),
			wantErr: "decode frame",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeFrame([]byte(test.payload)); err == nil {
				t.Fatalf("DecodeFrame succeeded, want error containing %q", test.wantErr)
			} else if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeFrameAllowsGap(t *testing.T) {
	payload := testFrame(
		testMessage(5, testHashA, `{}`, ""),
		testMessage(7, testHashB, `{}`, ""),
	)
	frame, err := DecodeFrame([]byte(payload))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if len(frame.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(frame.Messages))
	}
	if frame.Messages[0].Sequence != 5 || frame.Messages[1].Sequence != 7 {
		t.Fatalf("sequences = %d, %d, want 5, 7", frame.Messages[0].Sequence, frame.Messages[1].Sequence)
	}
}
