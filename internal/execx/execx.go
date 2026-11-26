// Package execx runs external commands behind an interface so that the code
// which builds docker, caddy and backup command lines can be tested
// without running them.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Cmd describes one command invocation.
type Cmd struct {
	Name   string
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Env    []string
}

// String renders the command for logs and test assertions.
func (c Cmd) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Runner runs commands.
type Runner interface {
	Run(ctx context.Context, cmd Cmd) error
}

// OS runs commands on the host.
type OS struct{}

// Run implements Runner. Arguments are passed as an argv array, never through
// a shell, and every caller builds them from validated values.
func (OS) Run(ctx context.Context, c Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) // NOSONAR argv built from validated app definitions, no shell
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	if c.Env != nil {
		cmd.Env = c.Env
	}
	return cmd.Run()
}

// Output runs a command and returns its trimmed stdout. On failure the error
// carries the command and its stderr.
func Output(ctx context.Context, r Runner, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := Cmd{Name: name, Args: args, Stdout: &stdout, Stderr: &stderr}
	if err := r.Run(ctx, cmd); err != nil {
		return "", Wrap(cmd, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Stream runs a command sending both stdout and stderr to out.
func Stream(ctx context.Context, r Runner, out io.Writer, name string, args ...string) error {
	cmd := Cmd{Name: name, Args: args, Stdout: out, Stderr: out}
	if err := r.Run(ctx, cmd); err != nil {
		return Wrap(cmd, err, "")
	}
	return nil
}

// Wrap adds the command line and stderr to an error.
func Wrap(c Cmd, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return fmt.Errorf("%s %s: %w", c.Name, firstArgs(c.Args), err)
	}
	return fmt.Errorf("%s %s: %w: %s", c.Name, firstArgs(c.Args), err, stderr)
}

func firstArgs(args []string) string {
	if len(args) > 3 {
		args = args[:3]
	}
	return strings.Join(args, " ")
}
