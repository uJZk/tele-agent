// Package sysprompt builds the text tele appends to Claude's system prompt
// (docs/claude-code.md section 7) and merges it with the user's own
// --append-system-prompt and --append-system-prompt-file arguments, because
// tele passes a single --append-system-prompt-file and must not drop what
// the user asked for.
//
// The prompt states only facts about the target host, which stay true for
// the whole session. The values come from the remote side and are treated
// as untrusted: they are rendered on one line each, so a hostile host cannot
// inject extra prompt lines through, say, its hostname.
package sysprompt

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ujzk/tele-agent/internal/proto"
)

// maxValueLen bounds each rendered value, in bytes. It is PATH_MAX, so no
// legitimate path is cut, while a hostile peer cannot inflate the prompt.
const maxValueLen = 4096

// Render returns the appended system prompt for the target host. Its shape
// is the template in docs/claude-code.md section 7; compatibility tests
// check that Claude answers questions about the host from it.
func Render(alias string, t proto.TargetInfo, workdir string) string {
	var b strings.Builder
	b.WriteString("# Target host (tele)\n\n")
	fmt.Fprintf(&b, "This session operates on the remote host \"%s\" via tele.\n",
		strings.ReplaceAll(value(alias), `"`, `\"`))
	fmt.Fprintf(&b, "- Hostname: %s\n", value(t.Hostname))
	fmt.Fprintf(&b, "- OS: %s (%s, %s)\n", value(t.OSPrettyName), value(t.Kernel), value(t.Arch))
	fmt.Fprintf(&b, "- User: %s (HOME=%s), login shell: %s\n", value(t.User), value(t.Home), value(t.Shell))
	fmt.Fprintf(&b, "- Working directory: %s\n", value(workdir))
	return b.String()
}

// value renders s on a single line: control characters, line and paragraph
// separators and invalid UTF-8 are escaped Go-style, and overlong values are
// truncated. An empty value reads "unknown".
func value(s string) string {
	if s == "" {
		return "unknown"
	}
	truncated := len(s) > maxValueLen
	if truncated {
		// Cut at a rune boundary so the cut itself adds no invalid UTF-8.
		cut := maxValueLen
		for cut > maxValueLen-utf8.UTFMax && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp):
			if r < 0x80 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	if truncated {
		b.WriteString("…")
	}
	return b.String()
}

// Claude Code options that append to the system prompt. They are
// long-only commander.js options with a required argument.
const (
	flagPrompt = "--append-system-prompt"
	flagFile   = "--append-system-prompt-file"
)

// SplitAppendArgs removes --append-system-prompt and
// --append-system-prompt-file (each as "<flag> <value>" or "<flag>=<value>")
// from Claude's arguments and returns the remaining arguments in their
// original order, the prompt texts, and the file paths as given.
//
// Matching follows commander.js, which parses Claude's command line
// (docs/claude-code.md section 7): a flag with a required value takes the
// next argument even if it starts with '-', a missing value is an error, and
// nothing after "--" is an option.
//
// Limitation: Claude's other options are not modeled, so an argument that
// is exactly one of these flags is taken as the flag even where Claude would
// read it as the value of a preceding option (for example
// `--system-prompt --append-system-prompt`). Arguments that merely contain
// the flag text, such as a prompt mentioning it, are never matched.
func SplitAppendArgs(args []string) (rest, prompts, files []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		var dst *[]string
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case flagPrompt:
			dst = &prompts
		case flagFile:
			dst = &files
		default:
			rest = append(rest, a)
			continue
		}
		if !hasVal {
			if i+1 == len(args) {
				return nil, nil, nil, fmt.Errorf("claude option %s requires a value", name)
			}
			i++
			val = args[i]
		}
		*dst = append(*dst, val)
	}
	return rest, prompts, files, nil
}

// Compose joins the generated prompt and the user's appended prompts and
// prompt files into the content of the single file passed to Claude:
// generated first, then the prompts, then the files, each in the order
// given, separated by a blank line. Empty parts are skipped. The user's
// content is otherwise kept verbatim.
func Compose(generated string, userPrompts []string, userFiles [][]byte) string {
	parts := make([]string, 0, 1+len(userPrompts)+len(userFiles))
	add := func(s string) {
		s = strings.TrimRight(s, "\r\n")
		if strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	add(generated)
	for _, p := range userPrompts {
		add(p)
	}
	for _, f := range userFiles {
		add(string(f))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}
