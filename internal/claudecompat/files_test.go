package claudecompat

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

// TestSessionEnvFile pins what a SessionStart hook finds of
// CLAUDE_ENV_FILE (docs/claude-code.md "scratch 文件"): it lies in
// <config>/session-env/<session id>/, which exists, while the file itself
// does not exist yet. So scratch uploads must create claimed empty
// directories on the remote side, or the hook's >> "$CLAUDE_ENV_FILE"
// fails there.
func TestSessionEnvFile(t *testing.T) {
	claude := claudetest.Require(t)
	home := t.TempDir()
	report := filepath.Join(t.TempDir(), "report")
	settings := filepath.Join(t.TempDir(), "settings.json")
	writeJSON(t, settings, map[string]any{"hooks": map[string]any{
		"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{"type": "command",
			"command": `{ echo "$CLAUDE_ENV_FILE"; test -d "$(dirname "$CLAUDE_ENV_FILE")" && echo dir; test -e "$CLAUDE_ENV_FILE" && echo file; } > ` + report,
		}}}},
	}})
	api := claudetest.NewAPI(t, claudetest.Say("ok"))
	r := claudetest.Run(t, claude, api, claudetest.Options{Prompt: "hi", Home: home, Args: []string{"--settings", settings}})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("SessionStart hook did not run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	t.Logf("hook saw %q", lines)
	prefix := filepath.Join(home, ".claude", "session-env") + "/"
	if len(lines) == 0 || !strings.HasPrefix(lines[0], prefix) || strings.Count(strings.TrimPrefix(lines[0], prefix), "/") != 1 {
		t.Fatalf("CLAUDE_ENV_FILE %q, want %s<session id>/<file>", lines[0], prefix)
	}
	if !slices.Equal(lines[1:], []string{"dir"}) {
		t.Errorf("hook found %q, want the directory but not the file", lines[1:])
	}
}

// TestBackgroundTaskOutput pins that a background Bash task writes its
// output under CLAUDE_CODE_TMPDIR, in a tasks directory, so that it is
// inside a scratch prefix (docs/claude-code.md "scratch 文件").
func TestBackgroundTaskOutput(t *testing.T) {
	claude := claudetest.Require(t)
	tmp := t.TempDir()
	api := claudetest.NewAPI(t,
		claudetest.Use("Bash", map[string]any{"command": "echo bg", "description": "Background", "run_in_background": true}),
		claudetest.Use("Bash", map[string]any{"command": "sleep 1", "description": "Wait"}),
		claudetest.Say("done"),
	)
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "go",
		Args:   []string{"--allowedTools", "Bash"},
		Env:    []string{"CLAUDE_CODE_TMPDIR=" + tmp},
	})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
	var outputs []string
	err := filepath.WalkDir(tmp, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".output") {
			outputs = append(outputs, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 || filepath.Base(filepath.Dir(outputs[0])) != "tasks" {
		t.Fatalf("output files %q, want one in a tasks directory under %s", outputs, tmp)
	}
	if b, _ := os.ReadFile(outputs[0]); !strings.HasPrefix(string(b), "bg\n") {
		t.Errorf("output %q, want it to start with the command's output", b)
	}
}

// TestClaudeJSONWrite pins how Claude writes ~/.claude.json
// (docs/claude-code.md "~/.claude.json"): every name it creates directly in
// HOME starts with ".claude.json": temporary files renamed over the
// target, and a lock directory. A bind mount of the single file cannot
// take the rename, which is why telefs serves those names locally.
func TestClaudeJSONWrite(t *testing.T) {
	claude := claudetest.Require(t)
	home := t.TempDir()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if _, err := unix.InotifyAddWatch(fd, home, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_MOVED_FROM); err != nil {
		t.Fatal(err)
	}
	api := claudetest.NewAPI(t, claudetest.Say("ok"))
	r := claudetest.Run(t, claude, api, claudetest.Options{Prompt: "hi", Home: home})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
	events := readInotify(t, fd)
	var tmpRenamed, lockDir bool
	for _, e := range events {
		switch {
		case e.name == ".claude":
			// The configuration directory, a bind mount under tele.
		case !strings.HasPrefix(e.name, ".claude.json"):
			t.Errorf("Claude created %q in HOME, outside the .claude.json names", e.name)
		case e.mask&unix.IN_MOVED_FROM != 0 && strings.HasPrefix(e.name, ".claude.json.tmp."):
			tmpRenamed = true
		case e.name == ".claude.json.lock" && e.mask&unix.IN_ISDIR != 0:
			lockDir = true
		}
	}
	if !tmpRenamed || !lockDir {
		t.Errorf("events %+v: temporary file renamed %v, lock directory %v; want both", events, tmpRenamed, lockDir)
	}
}

type inotifyEvent struct {
	mask uint32
	name string
}

// readInotify returns the queued events.
func readInotify(t *testing.T, fd int) []inotifyEvent {
	t.Helper()
	var out []inotifyEvent
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(fd, buf)
		if err == unix.EAGAIN {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			name := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(ev.Len)]
			out = append(out, inotifyEvent{mask: ev.Mask, name: string(bytes.TrimRight(name, "\x00"))})
			off += unix.SizeofInotifyEvent + int(ev.Len)
		}
	}
}
