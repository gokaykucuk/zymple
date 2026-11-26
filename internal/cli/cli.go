// Package cli implements the zymple command line.
package cli

import (
	"fmt"
	"io"
)

// Version is set at build time with -ldflags.
var Version = "dev"

const usage = "usage: zymple version"

// Run executes one command line and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		if _, err := fmt.Fprintln(stdout, "zymple", Version); err != nil {
			return 1
		}
		return 0
	}
	if _, err := fmt.Fprintln(stderr, usage); err != nil {
		return 1
	}
	return 2
}
