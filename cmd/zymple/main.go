// Command zymple is the Zymple server and CLI.
package main

import (
	"os"

	"github.com/gokaykucuk/zymple/internal/cli"
)

// exit is replaced in tests.
var exit = os.Exit

func main() {
	exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
