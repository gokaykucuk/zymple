package server

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gokaykucuk/zymple/internal/deploy"
)

// stream buffers the events of one operation so that the operation never
// blocks on, or dies with, the client that started it.
type stream struct {
	mu     sync.Mutex
	events []deploy.Event
	// dropped counts events already delivered and released, so a long
	// `logs -f` does not keep every line in memory.
	dropped int
	closed  bool
	wake    chan struct{}
}

func newStream() *stream { return &stream{wake: make(chan struct{})} }

func (s *stream) add(e deploy.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.events = append(s.events, e)
	close(s.wake)
	s.wake = make(chan struct{})
}

func (s *stream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.wake)
}

// from returns the events at index i and later, whether the stream has
// ended, and a channel that is closed when there is more to read.
func (s *stream) from(i int) ([]deploy.Event, bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := i - s.dropped
	if start < 0 {
		// The caller asked for events that were already released.
		start = 0
	}
	return s.events[start:], s.closed, s.wake
}

// release forgets the events before index i. Each stream has one reader.
func (s *stream) release(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := i - s.dropped; n > 0 {
		s.events = append([]deploy.Event(nil), s.events[n:]...)
		s.dropped = i
	}
}

// Write lets a stream receive command output as log events.
func (s *stream) writer(step string) *lineWriter { return &lineWriter{stream: s, step: step} }

type lineWriter struct {
	stream *stream
	step   string
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := indexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		w.stream.add(deploy.Event{Step: w.step, Status: "log", Message: line})
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.stream.add(deploy.Event{Step: w.step, Status: "log", Message: string(w.buf)})
		w.buf = nil
	}
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// serve copies a stream to the client as newline-delimited JSON until the
// stream ends or the client goes away.
func (s *stream) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	sent := 0
	for {
		events, closed, wake := s.from(sent)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return
			}
		}
		sent += len(events)
		s.release(sent)
		if flusher != nil {
			flusher.Flush()
		}
		if closed && len(events) == 0 {
			return
		}
		if closed {
			continue
		}
		select {
		case <-wake:
		case <-r.Context().Done():
			return
		}
	}
}
