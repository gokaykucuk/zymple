package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"unicode/utf8"

	"github.com/gokaykucuk/zymple/internal/deploy"
)

const (
	// maxBacklog bounds the events a stream holds for a reader that is slow
	// or gone. Beyond it the oldest are dropped; the outcome always arrives
	// last, so it is never the one dropped.
	maxBacklog = 10000
	// maxBacklogBytes bounds the same backlog by the size of its messages:
	// ten thousand events of the largest size would be far too much to hold.
	maxBacklogBytes = 16 << 20
	// maxLine bounds one log event. Output with no newline is cut into
	// events of this size instead of growing a buffer without limit.
	maxLine = 64 << 10
)

// stream buffers the events of one operation so that the operation never
// blocks on, or dies with, the client that started it.
type stream struct {
	mu     sync.Mutex
	events []deploy.Event
	// bytes is the total length of the messages in events.
	bytes int
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
	s.bytes += len(e.Message)
	s.drop(s.excess())
	close(s.wake)
	s.wake = make(chan struct{})
}

// excess returns how many of the oldest events are over the limits. The
// newest event is never counted, however large it is.
func (s *stream) excess() int {
	n := max(len(s.events)-maxBacklog, 0)
	size := s.bytes
	for _, e := range s.events[:n] {
		size -= len(e.Message)
	}
	for n < len(s.events)-1 && size > maxBacklogBytes {
		size -= len(s.events[n].Message)
		n++
	}
	return n
}

// drop forgets the n oldest events. The caller holds the lock.
func (s *stream) drop(n int) {
	if n <= 0 {
		return
	}
	for _, e := range s.events[:n] {
		s.bytes -= len(e.Message)
	}
	s.events = append([]deploy.Event(nil), s.events[n:]...)
	s.dropped += n
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
	events, _, closed, wake := s.read(i)
	return events, closed, wake
}

// read is from, and also returns the index to ask for next. That is not
// always i plus the number of events: a reader that fell behind skips the
// events dropped in the meantime.
func (s *stream) read(i int) ([]deploy.Event, int, bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := i - s.dropped
	if start < 0 {
		// The caller asked for events that were already released or dropped.
		start = 0
	}
	if start > len(s.events) {
		start = len(s.events)
	}
	events := s.events[start:]
	return events, s.dropped + start + len(events), s.closed, s.wake
}

// release forgets the events before index i. Each stream has one reader.
func (s *stream) release(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop(min(i-s.dropped, len(s.events)))
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
		next := i + 1
		if i < 0 || i > maxLine {
			if len(w.buf) < maxLine {
				break
			}
			// No line end within the limit: emit what there is.
			i = cutPoint(w.buf, maxLine)
			next = i
		}
		line := string(w.buf[:i])
		w.buf = w.buf[next:]
		w.stream.add(deploy.Event{Step: w.step, Status: "log", Message: line})
	}
	if len(w.buf) == 0 {
		// Do not keep a large backing array alive for a small remainder.
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.stream.add(deploy.Event{Step: w.step, Status: "log", Message: string(w.buf)})
		w.buf = nil
	}
}

// cutPoint returns where to cut b, which holds at least limit bytes, so that
// the cut is at or just before limit and not inside a UTF-8 character.
// Bytes that are not UTF-8 are cut at limit.
func cutPoint(b []byte, limit int) int {
	for back := 1; back < utf8.UTFMax && back < limit; back++ {
		start := limit - back
		if !utf8.RuneStart(b[start]) {
			continue
		}
		if utf8.FullRune(b[start:limit]) {
			return limit
		}
		return start
	}
	return limit
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
		events, next, closed, wake := s.read(sent)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return
			}
		}
		sent = next
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
