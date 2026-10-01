package dispatch

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// Desktop programs: what Claude starts to open a browser and to read and
// write the clipboard (docs/claude-code.md "浏览器、剪贴板与通知"). They
// serve the user at this machine, so they are local exec proxies; unlike
// ps, each accepts only the arguments Claude passes, because the local
// exec proxies are the one place where something reaching Claude from the
// target host could run on this machine (docs/security.md "远端返回的数据").

// NameXdgOpen opens a URL in the browser.
const NameXdgOpen = "xdg-open"

// clipboardArgs lists, per program, the argument lists Claude uses: writing
// for /copy, reading text for a paste without an image.
var clipboardArgs = map[string][][]string{
	"xclip":    {{"-selection", "clipboard"}, {"-selection", "primary"}, {"-selection", "clipboard", "-o"}},
	"xsel":     {{"--clipboard", "--input"}, {"--primary", "--input"}, {"--clipboard", "--output"}},
	"wl-copy":  {{}, {"--primary"}},
	"wl-paste": {{"--no-newline"}},
}

// DesktopPrograms are the shim names of the desktop programs.
var DesktopPrograms = []string{NameXdgOpen, "xclip", "xsel", "wl-copy", "wl-paste"}

// screenshotName is the file Claude has the clipboard image saved to, in
// claude-<uid>/ under CLAUDE_CODE_TMPDIR.
const screenshotName = "claude_cli_latest_screenshot.png"

// Clipboard image scripts: Claude runs them with /bin/sh -c, the first to
// find an image, the second to save it to the screenshot file, which it
// then reads, the third to remove that file. %[1]s is the file.
var clipboardScripts = []string{
	`xclip -selection clipboard -t TARGETS -o 2>/dev/null | grep -E "image/(png|jpeg|jpg|gif|webp|bmp)" || wl-paste -l 2>/dev/null | grep -E "image/(png|jpeg|jpg|gif|webp|bmp)"`,
	`xclip -selection clipboard -t image/png -o > %[1]s 2>/dev/null || wl-paste --type image/png > %[1]s 2>/dev/null || xclip -selection clipboard -t image/bmp -o > %[1]s 2>/dev/null || wl-paste --type image/bmp > %[1]s`,
	`rm -f -- %[1]s`,
}

// ErrRejected reports a desktop program invoked with arguments Claude does
// not use.
var ErrRejected = errors.New("dispatch: arguments not accepted for a local program")

// desktopAction returns the Action for the desktop program name with
// arguments args.
func desktopAction(name string, args []string) (Action, error) {
	if name == NameXdgOpen {
		if len(args) != 1 || !isWebURL(args[0]) {
			return Action{}, fmt.Errorf("%w: %s %q: only one http or https URL", ErrRejected, name, args)
		}
		return Action{Local: true, Argv: []string{name, args[0]}}, nil
	}
	for _, a := range clipboardArgs[name] {
		if slices.Equal(a, args) {
			return Action{Local: true, Argv: slices.Concat([]string{name}, args)}, nil
		}
	}
	return Action{}, fmt.Errorf("%w: %s %q", ErrRejected, name, args)
}

// isWebURL reports whether s is an absolute http or https URL with a host.
// Anything else, a file: URL or a word xdg-open would take for an option
// or a path, is refused.
func isWebURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || strings.ContainsFunc(s, isControl) {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// clipboardScript reports whether script is one of the clipboard image
// scripts, and returns it with the screenshot file relative to the session
// directory: local actions run there (Action.Local), whereas Claude names
// the file by its path in the remote view.
func clipboardScript(script, sessDir string) (string, bool) {
	if script == clipboardScripts[0] {
		return script, true
	}
	rel, ok := screenshotFile(script, sessDir)
	if !ok {
		return "", false
	}
	file := path.Join(sessDir, rel)
	for _, s := range clipboardScripts[1:] {
		if script == fmt.Sprintf(s, file) {
			return fmt.Sprintf(s, rel), true
		}
	}
	return "", false
}

// screenshotFile finds the first screenshot file named in script, in the
// session directory, and returns its path relative to that directory.
func screenshotFile(script, sessDir string) (string, bool) {
	if !path.IsAbs(sessDir) {
		return "", false
	}
	prefix := path.Join(sessDir, "tmp", "claude-")
	i := strings.Index(script, prefix)
	if i < 0 {
		return "", false
	}
	rest := script[i+len(prefix):]
	n := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if n <= 0 || !strings.HasPrefix(rest[n:], "/"+screenshotName) {
		return "", false
	}
	return path.Join("tmp", "claude-"+rest[:n], screenshotName), true
}
