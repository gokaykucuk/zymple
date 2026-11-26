package services

import (
	"bytes"
	"context"
	"io"
	"os"

	"github.com/gokaykucuk/zymple/internal/execx"
)

// Grace periods before Docker kills a managed service. A database needs time
// to finish its shutdown checkpoint; `docker rm -f` on its own is SIGKILL.
const (
	postgresStopSeconds = "120"
	redisStopSeconds    = "60"
)

// secretEnv returns the docker CLI's environment with the given NAME=value
// pairs added. A secret is handed to a container with `-e NAME` and no value,
// so Docker takes it from here and it never appears on a command line, which
// every local user can read through /proc.
func secretEnv(pairs ...string) []string {
	return append(os.Environ(), pairs...)
}

// dockerOutput runs docker and returns its trimmed stdout. env and stdin may
// be nil.
func dockerOutput(ctx context.Context, r execx.Runner, env []string, stdin io.Reader, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := execx.Cmd{Name: "docker", Args: args, Stdin: stdin, Stdout: &stdout, Stderr: &stderr, Env: env}
	if err := r.Run(ctx, cmd); err != nil {
		return "", execx.Wrap(cmd, err, stderr.String())
	}
	return string(bytes.TrimSpace(stdout.Bytes())), nil
}

// dockerStream runs docker sending stdout and stderr to out.
func dockerStream(ctx context.Context, r execx.Runner, out io.Writer, env []string, args ...string) error {
	cmd := execx.Cmd{Name: "docker", Args: args, Stdout: out, Stderr: out, Env: env}
	if err := r.Run(ctx, cmd); err != nil {
		return execx.Wrap(cmd, err, "")
	}
	return nil
}

// replace stops a managed container gracefully, if it runs, and removes it.
func replace(ctx context.Context, r execx.Runner, name, state, graceSeconds string) error {
	if state == "" {
		return nil
	}
	if state == "running" {
		if _, err := execx.Output(ctx, r, "docker", "stop", "--time", graceSeconds, name); err != nil {
			return err
		}
	}
	_, err := execx.Output(ctx, r, "docker", "rm", "-f", name)
	return err
}
