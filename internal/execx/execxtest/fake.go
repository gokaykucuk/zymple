// Package execxtest provides a scriptable command runner for tests.
package execxtest

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/gokaykucuk/zymple/internal/execx"
)

// Fake records commands instead of running them.
type Fake struct {
	// Handler decides each command's stdout and error. Nil means every
	// command succeeds with no output.
	Handler func(cmd execx.Cmd) (stdout string, err error)

	mu    sync.Mutex
	calls []string
}

// Run implements execx.Runner.
func (f *Fake) Run(_ context.Context, cmd execx.Cmd) error {
	f.mu.Lock()
	f.calls = append(f.calls, cmd.String())
	handler := f.Handler
	f.mu.Unlock()
	if handler == nil {
		return nil
	}
	out, err := handler(cmd)
	if out != "" && cmd.Stdout != nil {
		_, _ = io.WriteString(cmd.Stdout, out)
	}
	if err != nil && cmd.Stderr != nil {
		_, _ = io.WriteString(cmd.Stderr, err.Error())
	}
	return err
}

// Calls returns the command lines run so far.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Reset forgets the recorded commands.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// Count returns how many recorded commands contain substr.
func (f *Fake) Count(substr string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// Find returns the first recorded command containing substr, or "".
func (f *Fake) Find(substr string) string {
	for _, c := range f.Calls() {
		if strings.Contains(c, substr) {
			return c
		}
	}
	return ""
}
