package editproxy

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

func TestCommand(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"vi", "nano"} {
		claudetest.WriteScript(t, bin, name, "exit 0\n")
	}
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{"VISUAL first", []string{"EDITOR=nano", "VISUAL=vim -u NONE"}, []string{"vim", "-u", "NONE"}},
		{"EDITOR", []string{"EDITOR=emacs -nw"}, []string{"emacs", "-nw"}},
		{"the last entry wins", []string{"EDITOR=a", "EDITOR=b"}, []string{"b"}},
		{"blank VISUAL is unset", []string{"VISUAL= ", "EDITOR=nano"}, []string{"nano"}},
		{"code waits", []string{"VISUAL=code"}, []string{"code", "-w"}},
		{"subl waits", []string{"EDITOR=subl"}, []string{"subl", "--wait"}},
		{"explicit options are kept", []string{"VISUAL=code --new-window"}, []string{"code", "--new-window"}},
		{"PATH fallback in Claude's order", []string{"PATH=" + bin}, []string{"vi"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Command(tt.env)
			if err != nil || !slices.Equal(got, tt.want) {
				t.Errorf("Command(%q) = %q, %v, want %q", tt.env, got, err, tt.want)
			}
		})
	}
	if got, err := Command([]string{"PATH=" + t.TempDir()}); !errors.Is(err, ErrNoEditor) {
		t.Errorf("Command without an editor = %q, %v", got, err)
	}
}

// editor writes a fake editor: it records its argv and working directory
// next to itself, then runs body with the file in $f.
func editor(t *testing.T, body string) (env []string, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	claudetest.WriteScript(t, dir, "ed", `for f; do :; done
printf '%s\n' "$*" "$(/bin/pwd)" >> '`+log+`'
`+body)
	return []string{"PATH=/usr/bin:/bin", "VISUAL=" + filepath.Join(dir, "ed") + " -x"}, log
}

func run(t *testing.T, env []string, file string) (code, sig int, msg string) {
	t.Helper()
	sess := t.TempDir()
	st := Edit(t.Context(), Config{Dir: sess, Env: env}, Request{Path: file, ViewPID: os.Getpid()}, nil)
	if left, _ := os.ReadDir(sess); len(left) != 0 {
		t.Errorf("Edit left %d entries in the session directory", len(left))
	}
	return st.Code, st.Signal, st.Msg
}

func TestEditWritesBack(t *testing.T) {
	env, log := editor(t, `printf 'more\n' >> "$f"`)
	file := filepath.Join(t.TempDir(), "CLAUDE.md")
	if err := os.WriteFile(file, []byte("start\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(file)
	if code, sig, msg := run(t, env, file); code != 0 || sig != 0 {
		t.Fatalf("Edit = %d, signal %d, %q", code, sig, msg)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "start\nmore\n" {
		t.Fatalf("file holds %q, %v", b, err)
	}
	after, _ := os.Stat(file)
	if !os.SameFile(before, after) || after.Mode() != before.Mode() {
		t.Errorf("the file was replaced: %v -> %v", before.Mode(), after.Mode())
	}
	// The editor got its options and a copy with the same name, in a
	// directory of its own in the session directory.
	l, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(l)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "-x ") || filepath.Base(lines[0]) != "CLAUDE.md" ||
		lines[0] == "-x "+file || filepath.Dir(strings.TrimPrefix(lines[0], "-x ")) != lines[1] {
		t.Errorf("editor ran as %q", lines)
	}
}

func TestEditUnchanged(t *testing.T) {
	env, _ := editor(t, "exit 3\n")
	file := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1e9, 0)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	if code, _, msg := run(t, env, file); code != 3 {
		t.Fatalf("Edit = %d, %q, want the editor's status", code, msg)
	}
	if fi, err := os.Stat(file); err != nil || !fi.ModTime().Equal(old) {
		t.Errorf("an unchanged file was written back: %v", err)
	}
}

func TestEditSignal(t *testing.T) {
	env, _ := editor(t, "kill -TERM $$\n")
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, sig, msg := run(t, env, file); sig != int(syscall.SIGTERM) {
		t.Fatalf("Edit = signal %d, %q", sig, msg)
	}
}

func TestEditFailures(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, MaxSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(dir, "ro")
	if err := os.WriteFile(ro, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	changing, _ := editor(t, `printf 'more\n' >> "$f"`)
	for _, tc := range []struct {
		file string
		env  []string
		want string
	}{
		{filepath.Join(dir, "missing"), changing, "no such file"},
		{dir, changing, "not a regular file"},
		{fifo, changing, "not a regular file"},
		{big, changing, "larger than"},
	} {
		code, _, msg := run(t, tc.env, tc.file)
		if code != codeFailure || !strings.Contains(msg, tc.want) {
			t.Errorf("Edit(%s) = %d, %q, want %q", tc.file, code, msg, tc.want)
		}
	}
	if os.Geteuid() != 0 { // root may write it
		if code, _, msg := run(t, changing, ro); code != codeFailure || !strings.Contains(msg, "permission denied") {
			t.Errorf("Edit(%s) = %d, %q, want permission denied", ro, code, msg)
		}
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, msg := run(t, []string{"PATH=" + t.TempDir()}, file); code != 127 || !strings.Contains(msg, "VISUAL") {
		t.Errorf("Edit without an editor = %d, %q", code, msg)
	}
	if code, _, _ := run(t, []string{"PATH=/bin", "VISUAL=/nonexistent/ed"}, file); code != 127 {
		t.Errorf("Edit with a missing editor = %d", code)
	}
}

// TestOpenInRoot checks that paths, symbolic links included, resolve
// within the view's root: an absolute link from the target host must not
// reach a file of this machine's own view.
func TestOpenInRoot(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	view := t.TempDir()
	if err := os.MkdirAll(filepath.Join(view, filepath.Dir(outside)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view, outside), []byte("remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"abs": outside, "up": "../../../../../../../.." + outside} {
		if err := os.Symlink(target, filepath.Join(view, name)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := unix.Open(view, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	for _, p := range []string{outside, "/abs", "/up", "/../../.." + outside} {
		b, err := readIn(root, p)
		if err != nil || string(b) != "remote" {
			t.Errorf("readIn(%s) = %q, %v, want the view's file", p, b, err)
		}
	}
	if err := writeBack(root, "/abs", []byte("edited")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "local" {
		t.Errorf("writing through a link changed this machine's file: %q", b)
	}
}

func TestNameOf(t *testing.T) {
	for p, want := range map[string]string{"/a/CLAUDE.md": "CLAUDE.md", "/": "file", "/a/b/": "b"} {
		if got := NameOf(p); got != want {
			t.Errorf("NameOf(%q) = %q, want %q", p, got, want)
		}
	}
}
