package remote

import (
	"bufio"
	"encoding/json"
	"os"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// feedArrivalLog writes when each standard feed message arrived, for a side by
// side comparison with another feed of the same sequences. A goroutine
// writes the file; a full queue drops rows rather than delay the feed reader.
type feedArrivalLog struct {
	rows chan feedArrivalRow
	done chan struct{}
}

type feedArrivalRow struct {
	Sequence  uint64 `json:"seq"`
	BlockHash string `json:"block_hash"`
	AtNS      int64  `json:"at_ns"`
}

func openFeedArrivalLog(path string) (*feedArrivalLog, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l := &feedArrivalLog{rows: make(chan feedArrivalRow, 4096), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		defer file.Close()
		buffered := bufio.NewWriter(file)
		encoder := json.NewEncoder(buffered)
		for row := range l.rows {
			_ = encoder.Encode(row)
			if len(l.rows) == 0 {
				_ = buffered.Flush()
			}
		}
		_ = buffered.Flush()
	}()
	return l, nil
}

func (l *feedArrivalLog) record(sequence uint64, hash domain.Hash, at time.Time) {
	if l == nil {
		return
	}
	select {
	case l.rows <- feedArrivalRow{Sequence: sequence, BlockHash: hash.Hex(), AtNS: at.UnixNano()}:
	default:
	}
}

func (l *feedArrivalLog) close() {
	if l == nil {
		return
	}
	close(l.rows)
	<-l.done
}
