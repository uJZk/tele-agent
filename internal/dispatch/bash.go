package dispatch

import "strings"

// bashLongOptions maps bash's word options to whether they take an
// argument. bash matches them after "--" and also after a single "-"
// ("-login" is --login), before any single-letter option.
var bashLongOptions = map[string]bool{
	"debug":           false,
	"debugger":        false,
	"dump-po-strings": false,
	"dump-strings":    false,
	"help":            false,
	"init-file":       true,
	"login":           false,
	"noediting":       false,
	"noprofile":       false,
	"norc":            false,
	"posix":           false,
	"pretty-print":    false,
	"protected":       false,
	"rcfile":          true,
	"restricted":      false,
	"verbose":         false,
	"version":         false,
	"wordexp":         false,
}

// bashScriptIndex returns the index in args (bash's argv without argv[0])
// of the -c command string, or -1 if bash would not run one. It mirrors
// parse_long_options and parse_shell_options in bash's shell.c: word options
// first, then clusters of single-letter options introduced by '-' or '+',
// where each 'o' or 'O' consumes the next argument and "-" or "--" ends
// the options. The command string is the first argument after the options.
// Arguments bash would reject are skipped like valid ones: bash then exits
// before running any script, so the index no longer matters.
func bashScriptIndex(args []string) int {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		name, long := strings.CutPrefix(args[i][1:], "-")
		long = long && name != ""
		if !long {
			name = args[i][1:]
		}
		takesArg, known := bashLongOptions[name]
		if !known {
			if long {
				return -1 // bash: invalid option, exits
			}
			break
		}
		if takesArg {
			i++
		}
		i++
	}

	wantCommand := false
	for i < len(args) && (strings.HasPrefix(args[i], "-") || strings.HasPrefix(args[i], "+")) {
		arg := args[i]
		next := i + 1
		if arg == "-" || arg == "--" {
			i = next
			break
		}
		for _, c := range arg[1:] {
			switch c {
			case 'c':
				wantCommand = true
			case 'o', 'O':
				if next < len(args) {
					next++
				}
			}
		}
		i = next
	}
	if !wantCommand || i >= len(args) {
		return -1
	}
	return i
}
