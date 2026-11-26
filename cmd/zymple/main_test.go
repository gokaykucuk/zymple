package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMainPassesExitCode(t *testing.T) {
	oldArgs, oldExit := os.Args, exit
	t.Cleanup(func() { os.Args, exit = oldArgs, oldExit })

	got := -1
	exit = func(code int) { got = code }
	os.Args = []string{"zymple", "version"}
	main()
	if got != 0 {
		t.Errorf("exit code = %d, want 0", got)
	}
	os.Args = []string{"zymple", "no-such-command"}
	main()
	if got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
}

func TestServeReportsConfigAndStartupErrors(t *testing.T) {
	dir := t.TempDir()
	if err := serve(context.Background(), filepath.Join(dir, "missing.yml")); err == nil {
		t.Error("a missing config file should be an error")
	}
	path := filepath.Join(dir, "zymple.yml")
	if err := os.WriteFile(path, []byte("containerd: { socket: \"\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := serve(context.Background(), path); err == nil {
		t.Error("an unusable containerd socket should be an error")
	}
}
