package cli

import (
	"bytes"
	"errors"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"version", []string{"version"}, 0, "zymple dev\n", ""},
		{"no arguments", nil, 2, "", usage + "\n"},
		{"unknown command", []string{"nope"}, 2, "", usage + "\n"},
		{"extra arguments", []string{"version", "x"}, 2, "", usage + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := Run(tt.args, &stdout, &stderr); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d", got, tt.wantCode)
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if stderr.String() != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestRunWriteErrors(t *testing.T) {
	var buf bytes.Buffer
	if got := Run([]string{"version"}, failingWriter{}, &buf); got != 1 {
		t.Errorf("version with failing stdout: exit code = %d, want 1", got)
	}
	if got := Run(nil, &buf, failingWriter{}); got != 1 {
		t.Errorf("usage with failing stderr: exit code = %d, want 1", got)
	}
}
