package pipeline

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// rotatingFile lets another goroutine replace the destination between whole
// writes. A json.Encoder writes each record with one Write call, so a record
// lands entirely in one file.
type rotatingFile struct {
	mu   sync.Mutex
	file *os.File
}

func (f *rotatingFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.file.Write(p)
}

func (f *rotatingFile) swap(next *os.File) *os.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	previous := f.file
	f.file = next
	return previous
}

func (f *rotatingFile) current() *os.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.file
}

func (r *Research) session(at time.Time) ResearchEvent {
	return ResearchEvent{Type: "session", ChainID: r.chainID, AtMS: at.UnixMilli(), Details: r.policy}
}

// StartFile writes the session header a new observation file needs, before
// Swap makes it the destination. Readers expect every file to open with one.
func (r *Research) StartFile(next *os.File, at time.Time) error {
	return json.NewEncoder(next).Encode(r.session(at))
}

// Swap moves later records to next and returns the file that no longer
// receives any; the caller syncs and closes it. Routes being tracked continue
// in next, so a lifetime can begin in one file and end in the following one.
func (r *Research) Swap(next *os.File) *os.File {
	return r.output.swap(next)
}
