package racetest

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Event is one row of the JSON-lines report. Fields are all `omitempty`
// so each event type writes only the keys it actually populates. The
// downstream consumer (scripts/racecheck/summarize.sh, US-008) reads
// the kind + the keys it cares about per kind.
type Event struct {
	Event      string                 `json:"event"`
	Timestamp  time.Time              `json:"ts"`
	WorkerID   int                    `json:"worker_id,omitempty"`
	Class      string                 `json:"class,omitempty"`
	Bucket     string                 `json:"bucket,omitempty"`
	Key        string                 `json:"key,omitempty"`
	Status     int                    `json:"status,omitempty"`
	DurationMs int64                  `json:"duration_ms,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Summary    map[string]any         `json:"summary,omitempty"`
	Inconsist  *Inconsistency         `json:"inconsistency,omitempty"`
}

// EventSink is the surface racetest writes JSON-lines events through.
// Implementations must be safe for concurrent Emit from many workers.
type EventSink interface {
	Emit(ev Event)
	Close() error
}

// nopSink discards events; used when ReportPath + EventWriter are both
// empty so the per-op call site can always call Emit unconditionally.
type nopSink struct{}

func (nopSink) Emit(Event)   {}
func (nopSink) Close() error { return nil }

// writerSink serialises events as JSON-lines to an io.Writer. Writes
// are mutex-guarded so concurrent workers do not interleave bytes
// within one line.
type writerSink struct {
	mu sync.Mutex
	w  io.Writer
}

func newWriterSink(w io.Writer) *writerSink { return &writerSink{w: w} }

func (s *writerSink) Emit(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.mu.Lock()
	_, _ = s.w.Write(b)
	_, _ = s.w.Write([]byte{'\n'})
	s.mu.Unlock()
}

func (s *writerSink) Close() error { return nil }

// fileSink wraps a buffered file writer. The bufio.Writer must be
// flushed before close — flushing inside Close keeps callers honest.
type fileSink struct {
	*writerSink
	f  *os.File
	bw *bufio.Writer
}

func newFileSink(path string) (*fileSink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	return &fileSink{writerSink: newWriterSink(bw), f: f, bw: bw}, nil
}

func (s *fileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.bw.Flush(); err != nil {
		_ = s.f.Close()
		return err
	}
	return s.f.Close()
}
