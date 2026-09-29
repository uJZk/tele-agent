// Command tele runs Claude Code locally against a remote host. It is a
// multi-call binary: the role is chosen by the name it was invoked as
// (shims) or by its first argument (docs/architecture.md "进程与角色").
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ujzk/tele-agent/internal/shim"
)

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:]))
}

func run(name string, args []string) int {
	if name != "tele" {
		// Every other name is a shim in <sess>/bin (docs/exec.md "shim").
		return shim.Main(name, append([]string{os.Args[0]}, args...))
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: tele [options] <alias>[:<dir>] [claude args...]")
		return 2
	}
	fmt.Fprintf(os.Stderr, "tele: %s: not implemented yet\n", args[0])
	return 1
}
