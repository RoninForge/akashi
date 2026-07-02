// Command akashi is the CLI entry point. Keep this file tiny: wire os and
// return from cli.Execute so tests can drive the same code path with their
// own writers.
package main

import (
	"os"

	"github.com/RoninForge/akashi/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Stdout, os.Stderr, os.Args[1:]))
}
