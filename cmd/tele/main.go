// Command tele runs Claude Code locally against a remote host. It is a
// multi-call binary: the role is chosen by the name it was invoked as
// (shims and internal roles) or by its first argument (docs/architecture.md
// "进程与角色").
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ujzk/tele-agent/internal/doctor"
	"github.com/ujzk/tele-agent/internal/hostcmd"
	"github.com/ujzk/tele-agent/internal/launcher"
	"github.com/ujzk/tele-agent/internal/preflight"
	"github.com/ujzk/tele-agent/internal/servercmd"
	"github.com/ujzk/tele-agent/internal/shim"
	"github.com/ujzk/tele-agent/internal/version"
	"github.com/ujzk/tele-agent/internal/view"
)

func main() {
	os.Exit(run(filepath.Base(os.Args[0]), os.Args[1:]))
}

func run(name string, args []string) int {
	switch name {
	case "tele":
	case launcher.RoleSession:
		if len(args) != 1 {
			return internalMisuse(name)
		}
		return launcher.SessionMain(args[0])
	case launcher.RoleView:
		return view.HelperMain()
	case launcher.RoleLaunch:
		return view.LaunchMain(args)
	case doctor.RoleProbe:
		return doctor.ProbeMain()
	default:
		// Every other name is a shim in <sess>/bin (docs/exec.md "shim").
		return shim.Main(name, append([]string{os.Args[0]}, args...))
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, launcher.Usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Println("tele", version.String())
		return 0
	case "server":
		return servercmd.Main(args[1:], servercmd.Streams{In: os.Stdin, Terminal: preflight.OpenTerminal(os.Stdin), Out: os.Stdout, Err: os.Stderr})
	case "doctor":
		return doctor.Main(args[1:], doctor.Streams{Terminal: preflight.OpenTerminal(os.Stdin), Out: os.Stdout, Err: os.Stderr})
	case "host":
		return hostcmd.Main(args[1:], hostcmd.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
	}
	return launcher.Main(args)
}

func internalMisuse(name string) int {
	fmt.Fprintf(os.Stderr, "tele: %s is internal to tele\n", name)
	return 2
}
