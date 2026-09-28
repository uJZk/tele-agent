// Package dispatch decides what a shim invocation means: which program runs,
// with which argv, and whether it runs on the target host or in session
// main's local view. It also selects the environment forwarded to the target
// host. It is pure (no I/O) so that every rule can be tested exhaustively.
//
// The shim table is docs/exec.md "shim"; the shapes Claude invokes shims
// with are docs/claude-code.md "CLAUDE_CODE_SHELL" and
// "CLAUDE_CODE_SHELL_PREFIX".
package dispatch

import (
	"errors"
	"fmt"
	"path"
	"slices"
)

// Shim names with special handling.
const (
	nameBash     = "bash"
	nameSh       = "sh"
	nameTeleExec = "tele-exec"
)

// remotePrograms run on the target host under their own name with argv
// forwarded unchanged.
var remotePrograms = map[string]bool{"rg": true, "git": true, "uname": true}

// Classification errors.
var (
	ErrEmptyArgv      = errors.New("dispatch: empty argv")
	ErrMissingCommand = errors.New("dispatch: tele-exec needs a shell command argument")
	ErrUnknownProgram = errors.New("dispatch: unknown program")
)

// Action is what session main does for one shim invocation.
type Action struct {
	// Local runs Argv in session main's local view; otherwise Argv runs on
	// the target host.
	Local bool
	// Argv is the command to run. Argv[0] is a bare program name, looked
	// up in the PATH of wherever it runs.
	Argv []string
}

// Classify maps the shim name was invoked as, with its full argv (argv[0]
// included), to an Action. sessDir is the session directory as Claude sees
// it; localProgs names the local exec proxies. The names with dedicated
// rules (bash, sh, tele-exec, rg, git, uname) are never local.
func Classify(name string, argv []string, sessDir string, localProgs map[string]bool) (Action, error) {
	if len(argv) == 0 {
		return Action{}, ErrEmptyArgv
	}
	args := argv[1:]
	teleExec := teleExecPath(sessDir)
	switch {
	case name == nameBash:
		return Action{Argv: slices.Concat([]string{nameBash}, unwrapBash(args, teleExec))}, nil
	case name == nameSh:
		// /bin/sh is only reached through spawn(..., {shell: true}), which
		// always runs exactly [sh, -c, command].
		if len(args) >= 2 && args[0] == "-c" {
			if inner, ok := unwrapScript(args[1], teleExec); ok {
				// The outer script passes nothing but inner to tele-exec,
				// so the outer $0... (args[2:]) are dropped.
				return teleExecAction([]string{inner}), nil
			}
		}
		return Action{Argv: slices.Concat([]string{nameSh}, args)}, nil
	case name == nameTeleExec:
		if len(args) == 0 {
			return Action{}, ErrMissingCommand
		}
		return teleExecAction(args), nil
	case remotePrograms[name]:
		return Action{Argv: slices.Concat([]string{name}, args)}, nil
	case localProgs[name]:
		return Action{Local: true, Argv: slices.Concat([]string{name}, args)}, nil
	default:
		return Action{}, fmt.Errorf("%w %q", ErrUnknownProgram, name)
	}
}

// teleExecAction runs tele-exec's arguments: the first is a shell command,
// the rest become its $0, $1, ...
func teleExecAction(args []string) Action {
	return Action{Argv: slices.Concat([]string{nameSh, "-c"}, args)}
}

// teleExecPath is the tele-exec shim as Claude names it in
// CLAUDE_CODE_SHELL_PREFIX, or "" if sessDir cannot hold it.
func teleExecPath(sessDir string) string {
	if !path.IsAbs(sessDir) {
		return ""
	}
	return path.Join(sessDir, "bin", nameTeleExec)
}

// unwrapBash returns bash's arguments with a tele-exec wrapper removed from
// the -c script. Claude wraps Bash commands twice when both
// CLAUDE_CODE_SHELL and CLAUDE_CODE_SHELL_PREFIX are set: the script is
// '<sess>/bin/tele-exec' '<command>' (docs/claude-code.md
// "CLAUDE_CODE_SHELL_PREFIX"). That path does not exist on the target host,
// so the wrapper must go; <command> needs bash, so it stays with bash.
func unwrapBash(args []string, teleExec string) []string {
	out := slices.Clone(args)
	i := bashScriptIndex(args)
	if i < 0 {
		return out
	}
	if inner, ok := unwrapScript(args[i], teleExec); ok {
		out[i] = inner
	}
	return out
}

// unwrapScript reports whether script is exactly the two shell words
// teleExec and inner, and returns inner.
func unwrapScript(script, teleExec string) (string, bool) {
	if teleExec == "" {
		return "", false
	}
	words, ok := plainWords(script)
	if !ok || len(words) != 2 || words[0] != teleExec {
		return "", false
	}
	return words[1], true
}
