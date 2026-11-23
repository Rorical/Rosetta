package feed

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Header is the L2 message header carried by a feed message. L1BlockNumber is
// the L1 block height; Timestamp is the L2 block timestamp in seconds. The
// measurement record shows these fields can disagree with the canonical block
// at batch boundaries, so callers should use BlockHash for block metadata.
type Header struct {
	Kind          uint8
	L1BlockNumber uint64
	Timestamp     uint64
	Poster        *domain.Address
	RequestID     *domain.Hash
	L1BaseFee     *big.Int
	// Explicit null is valid for sequencer messages and differs from omission.
	L1BaseFeeKnown bool
}

// Message is one decoded feed message. Sequence is the feed sequence number,
// not the L2 block number.
type Message struct {
	Sequence  uint64
	BlockHash domain.Hash
	Header    Header
	L2Msg     []byte
	// Nil means the frame omitted execution metadata, not a zero counter.
	DelayedMessagesRead *uint64
	LegacyBatchGasCost  *uint64
	BatchDataStats      *BatchDataStats
}

// BatchDataStats describes the batch bytes used in an L1 posting report.
type BatchDataStats struct {
	Length   uint64 `json:"length"`
	NonZeros uint64 `json:"nonzeros"`
}

// Frame is one decoded broadcast frame.
type Frame struct {
	Version  int
	Messages []Message
}

// wireFrame mirrors the JSON envelope. Nitro encodes the l2 message as an
// extra nested "message" object.
type wireFrame struct {
	Version  int           `json:"version"`
	Messages []wireMessage `json:"messages"`
}

type wireMessage struct {
	SequenceNumber uint64          `json:"sequenceNumber"`
	BlockHash      string          `json:"blockHash"`
	Message        wireMessageBody `json:"message"`
}

type wireMessageBody struct {
	Message             wireL2Message `json:"message"`
	DelayedMessagesRead *uint64       `json:"delayedMessagesRead"`
}

type wireL2Message struct {
	Header             wireHeader      `json:"header"`
	L2Msg              string          `json:"l2Msg"`
	LegacyBatchGasCost *uint64         `json:"batchGasCost"`
	BatchDataStats     *BatchDataStats `json:"batchDataTokens"`
}

type wireHeader struct {
	Kind        uint8           `json:"kind"`
	BlockNumber uint64          `json:"blockNumber"`
	Timestamp   uint64          `json:"timestamp"`
	Poster      *string         `json:"sender"`
	RequestID   *string         `json:"requestId"`
	L1BaseFee   json.RawMessage `json:"baseFeeL1"`
}

// DecodeFrame parses a sequencer broadcast frame. Sequence numbers must be
// strictly increasing within the frame; gaps are allowed and left to the
// caller's recovery logic. Missing optional header fields decode as zero.
func DecodeFrame(data []byte) (Frame, error) {
	var wire wireFrame
	if err := json.Unmarshal(data, &wire); err != nil {
		return Frame{}, fmt.Errorf("feed: decode frame: %w", err)
	}
	if wire.Version != 1 {
		return Frame{}, fmt.Errorf("feed: unsupported frame version %d", wire.Version)
	}

	frame := Frame{
		Version:  wire.Version,
		Messages: make([]Message, len(wire.Messages)),
	}
	var previous uint64
	for i, message := range wire.Messages {
		if i > 0 && message.SequenceNumber <= previous {
			return Frame{}, fmt.Errorf(
				"feed: message %d: sequence %d is not greater than previous sequence %d",
				i, message.SequenceNumber, previous,
			)
		}
		if message.BlockHash == "" {
			return Frame{}, fmt.Errorf("feed: message %d: missing blockHash", i)
		}
		blockHash, err := domain.ParseHash(message.BlockHash)
		if err != nil {
			return Frame{}, fmt.Errorf("feed: message %d: blockHash: %w", i, err)
		}

		var l2Msg []byte
		if encoded := message.Message.Message.L2Msg; encoded != "" {
			l2Msg, err = base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return Frame{}, fmt.Errorf("feed: message %d: l2Msg: %w", i, err)
			}
		}

		header, err := decodeHeader(message.Message.Message.Header)
		if err != nil {
			return Frame{}, fmt.Errorf("feed: message %d header: %w", i, err)
		}
		stats := message.Message.Message.BatchDataStats
		if stats != nil && stats.NonZeros > stats.Length {
			return Frame{}, fmt.Errorf("feed: message %d: batch nonzeros exceed length", i)
		}
		frame.Messages[i] = Message{
			Sequence:            message.SequenceNumber,
			BlockHash:           blockHash,
			Header:              header,
			L2Msg:               l2Msg,
			DelayedMessagesRead: message.Message.DelayedMessagesRead,
			LegacyBatchGasCost:  message.Message.Message.LegacyBatchGasCost,
			BatchDataStats:      stats,
		}
		previous = message.SequenceNumber
	}
	return frame, nil
}

func decodeHeader(wire wireHeader) (Header, error) {
	header := Header{Kind: wire.Kind, L1BlockNumber: wire.BlockNumber, Timestamp: wire.Timestamp, L1BaseFeeKnown: len(wire.L1BaseFee) != 0}
	if header.L1BaseFeeKnown && !bytes.Equal(bytes.TrimSpace(wire.L1BaseFee), []byte("null")) {
		var fee big.Int
		if err := json.Unmarshal(wire.L1BaseFee, &fee); err != nil {
			return Header{}, fmt.Errorf("baseFeeL1: %w", err)
		}
		if fee.Sign() < 0 || fee.BitLen() > 256 {
			return Header{}, fmt.Errorf("baseFeeL1 outside uint256")
		}
		header.L1BaseFee = &fee
	}
	if wire.Poster != nil {
		poster, err := domain.ParseAddress(*wire.Poster)
		if err != nil {
			return Header{}, fmt.Errorf("sender: %w", err)
		}
		header.Poster = &poster
	}
	if wire.RequestID != nil {
		request, err := domain.ParseHash(*wire.RequestID)
		if err != nil {
			return Header{}, fmt.Errorf("requestId: %w", err)
		}
		header.RequestID = &request
	}
	return header, nil
}
