// Package preflight runs the checks of "tele server install" and "tele
// doctor" and repairs what it may (docs/cli.md "预检与修复策略"): it checks
// everything first, repairs what only affects the current user and can be
// undone, shows the exact commands of system-wide or privileged repairs
// and runs them only with consent, and reports what cannot be repaired.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/manifest"
)

// Status is the outcome of a check.
type Status int

// Statuses, from best to worst.
const (
	OK Status = iota
	Warn
	// Fixable is repaired without asking: it only affects the current
	// user and can be undone.
	Fixable
	// Consent is repaired by commands that need the user's consent.
	Consent
	Fatal
)

// Symbol returns the mark of s in the checklist.
func (s Status) Symbol() string {
	switch s {
	case OK:
		return "✅"
	case Warn:
		return "⚠️"
	case Fixable:
		return "🔧"
	case Consent:
		return "🔐"
	default:
		return "❌"
	}
}

// Result is the outcome of one check.
type Result struct {
	Status Status
	// Detail explains the status in one line.
	Detail string
	// Fix repairs a Fixable item, recording its changes with rec.
	Fix func(ctx context.Context, rec Recorder) error
	// Commands repair a Consent item. They are shown to the user verbatim
	// and run with sh -c, in order.
	Commands []string
	// Undo, if set, is recorded once Commands succeed.
	Undo *manifest.Entry
	// Declined is what a Consent item becomes without consent: Warn or
	// Fatal, and Consequence says what it means.
	Declined    Status
	Consequence string
}

// Check is one item of the checklist. Run must not change anything.
type Check struct {
	Name string
	Run  func(ctx context.Context) Result
}

// Recorder records changes for rollback.
type Recorder interface {
	Record(manifest.Entry) error
	WriteFile(id, desc, path string, data []byte, perm os.FileMode) error
}

// Mode selects what Run does after checking.
type Mode int

// Modes (docs/cli.md "预检与修复策略").
const (
	// Repair repairs Fixable items and asks for consent to Consent items,
	// which it treats as declined without a terminal.
	Repair Mode = iota
	// Yes consents to every Consent item (--yes).
	Yes
	// CheckOnly only checks (--check).
	CheckOnly
	// PrintCommands prints the commands of Consent items and runs nothing
	// (--print-commands).
	PrintCommands
)

// Runner runs a checklist.
type Runner struct {
	Mode Mode
	// Out receives the checklist and the commands.
	Out io.Writer
	// Terminal, if set, is where consent is asked and answered; without
	// one, Consent items are declined and nothing asks for privileges.
	Terminal io.ReadWriter
	// Recorder records changes; nil records nothing.
	Recorder Recorder
	// Shell runs a consented command; nil runs it with sh -c on Terminal.
	Shell func(ctx context.Context, command string) error
}

// Run checks, repairs according to the mode, checks again and reports.
// It returns whether everything required passed.
func (r *Runner) Run(ctx context.Context, checks []Check) bool {
	results := runAll(ctx, checks)
	printList(r.Out, checks, results, false)
	switch r.Mode {
	case CheckOnly:
		return allPassed(results, false)
	case PrintCommands:
		printCommands(r.Out, checks, results, "Commands that need consent:")
		return allPassed(results, false)
	case Repair, Yes:
	}

	rec := r.Recorder
	if rec == nil {
		rec = nopRecorder{}
	}
	pending := false
	for i, c := range checks {
		if results[i].Status != Fixable && results[i].Status != Consent {
			continue
		}
		pending = true
		// Checks again right before repairing: an earlier repair may
		// have changed this item (a new key makes the service restart).
		res := c.Run(ctx)
		switch res.Status {
		case Fixable:
			fmt.Fprintf(r.Out, "🔧 %s: %s …\n", c.Name, res.Detail)
			if err := res.Fix(ctx, rec); err != nil {
				fmt.Fprintf(r.Out, "   failed: %v\n", err)
			}
		case OK, Warn, Fatal:
		case Consent:
			if !r.consent(c.Name, res) {
				continue
			}
			if err := r.runCommands(ctx, res.Commands); err != nil {
				fmt.Fprintf(r.Out, "   failed: %v\n", err)
				continue
			}
			if res.Undo != nil {
				if err := rec.Record(*res.Undo); err != nil {
					fmt.Fprintf(r.Out, "   cannot record the change for rollback: %v\n", err)
				}
			}
		}
	}
	if !pending {
		return allPassed(results, true)
	}
	results = runAll(ctx, checks)
	fmt.Fprintln(r.Out)
	printList(r.Out, checks, results, true)
	printCommands(r.Out, checks, results, "Run these commands yourself, then run this again:")
	return allPassed(results, true)
}

// consent shows the commands of item name and asks whether to run them.
func (r *Runner) consent(name string, res Result) bool {
	fmt.Fprintf(r.Out, "🔐 %s: %s\n", name, res.Detail)
	for _, c := range res.Commands {
		fmt.Fprintf(r.Out, "%s\n", indent(c))
	}
	switch {
	case r.Mode == Yes:
		fmt.Fprintln(r.Out, "   running (--yes)")
		return true
	case r.Terminal == nil:
		fmt.Fprintln(r.Out, "   skipped: no terminal to ask for consent")
		return false
	}
	fmt.Fprint(r.Terminal, "Run these commands? [y/N] ")
	line, err := ReadLine(r.Terminal)
	if err != nil {
		return false
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return true
	}
	return false
}

func (r *Runner) runCommands(ctx context.Context, commands []string) error {
	for _, c := range commands {
		var err error
		if r.Shell != nil {
			err = r.Shell(ctx, c)
		} else {
			err = RunShell(ctx, c, r.Terminal, r.Out)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// RunShell runs command with sh -c. Its input is term, or /dev/null
// without a terminal; its output goes to out.
func RunShell(ctx context.Context, command string, term io.ReadWriter, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdout, cmd.Stderr = out, out
	if term != nil {
		cmd.Stdin = term
		cmd.Stdout, cmd.Stderr = term, term
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", firstLine(command), err)
	}
	return nil
}

func runAll(ctx context.Context, checks []Check) []Result {
	results := make([]Result, len(checks))
	for i, c := range checks {
		results[i] = c.Run(ctx)
	}
	return results
}

// allPassed reports whether nothing required is left. After repairing, a
// declined Consent item counts as its Declined status.
func allPassed(results []Result, final bool) bool {
	for _, r := range results {
		s := r.Status
		if final && s == Consent {
			s = r.Declined
		}
		if s != OK && s != Warn {
			return false
		}
	}
	return true
}

func printList(w io.Writer, checks []Check, results []Result, final bool) {
	// Padded by hand: tabwriter counts runes, and the symbols differ in
	// runes but not in width.
	width := 0
	for _, c := range checks {
		width = max(width, utf8.RuneCountInString(c.Name))
	}
	for i, c := range checks {
		res := results[i]
		sym, detail := res.Status.Symbol(), res.Detail
		if final && res.Status == Consent {
			sym, detail = res.Declined.Symbol(), "not done: "+detail
			if res.Consequence != "" {
				detail += "; " + res.Consequence
			}
		}
		fmt.Fprintf(w, "%s %s%s  %s\n", sym, c.Name, strings.Repeat(" ", width-utf8.RuneCountInString(c.Name)), detail)
	}
}

func printCommands(w io.Writer, checks []Check, results []Result, title string) {
	first := true
	for i, c := range checks {
		if results[i].Status != Consent {
			continue
		}
		if first {
			fmt.Fprintf(w, "\n%s\n", title)
			first = false
		}
		fmt.Fprintf(w, "\n  # %s\n", c.Name)
		for _, cmd := range results[i].Commands {
			fmt.Fprintln(w, indent(cmd))
		}
	}
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

type nopRecorder struct{}

func (nopRecorder) Record(manifest.Entry) error { return nil }

func (nopRecorder) WriteFile(_, _, path string, data []byte, perm os.FileMode) error {
	return manifest.WriteAtomic(path, data, perm)
}

// OpenTerminal returns the controlling terminal when f, the standard
// input, is one, or nil.
func OpenTerminal(f *os.File) io.ReadWriter {
	if !IsTerminal(f) {
		return nil
	}
	return readWriter{f, os.Stderr}
}

type readWriter struct {
	io.Reader
	io.Writer
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// ReadLine reads one line from r without buffering beyond it, so that
// later reads and child processes see the rest of the input.
func ReadLine(r io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < 8192 {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimSpace(b.String()), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && b.Len() > 0 {
				return strings.TrimSpace(b.String()), nil
			}
			return "", err
		}
	}
	return "", errors.New("line too long")
}

// ReadSecret reads a line from the terminal f without echoing it.
func ReadSecret(f *os.File) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	noEcho.Lflag |= unix.ICANON | unix.ECHONL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return "", err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }()
	return ReadLine(f)
}

// ShellQuote quotes s for sh.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r)
		return !safe
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteRootFile returns the command that writes content to path with sudo,
// as a here-document.
func WriteRootFile(path, content string) string {
	const eof = "TELE_EOF"
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return "sudo tee " + ShellQuote(path) + " >/dev/null <<'" + eof + "'\n" + content + eof
}
