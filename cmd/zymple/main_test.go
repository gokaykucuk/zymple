package main

import (
	"os"
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
}
