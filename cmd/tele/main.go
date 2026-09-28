// Command tele runs Claude Code locally against a remote host. It is a
// multi-call binary: the role is chosen by the name it was invoked as
// (shims) or by its first argument (docs/architecture.md section 4).
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:]))
}

func run(name string, args []string) int {
	if name != "tele" {
		fmt.Fprintf(os.Stderr, "tele: unknown role %q\n", name)
		return 2
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: tele [options] <alias>[:<dir>] [claude args...]")
		return 2
	}
	fmt.Fprintf(os.Stderr, "tele: %s: not implemented yet\n", args[0])
	return 1
}
