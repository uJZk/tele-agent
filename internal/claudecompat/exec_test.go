package claudecompat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

// TestOwnExecs pins which programs the Claude process itself starts in a
// tele-like environment (docs/exec.md "shim"): with PATH holding only the
// shim directory and the tele variables set, everything is found in that
// directory, except /bin/sh, which Claude starts by absolute path for
// shell hooks and which tele replaces in the remote view.
func TestOwnExecs(t *testing.T) {
	claude := claudetest.Require(t)
	tr := claudetest.NewTrace(t, claude)
	bin := t.TempDir()
	for _, name := range []string{"bash", "tele-exec", "rg", "git", "uname", "ps"} {
		prog := "/usr/bin/" + name
		if name == "tele-exec" {
			prog = `/bin/sh -c "$1"; exit`
		}
		claudetest.WriteScript(t, bin, name, "exec "+prog+` "$@"`+"\n")
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("tele\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(t.TempDir(), "settings.json")
	writeJSON(t, settings, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "true"}}}},
	}})
	mcp := filepath.Join(t.TempDir(), "mcp.json")
	writeJSON(t, mcp, map[string]any{"mcpServers": map[string]any{"probe": map[string]any{"type": "stdio", "command": "true"}}})
	api := claudetest.NewAPI(t,
		claudetest.Use("Bash", map[string]any{"command": "echo hi", "description": "Print"}),
		claudetest.Use("Grep", map[string]any{"pattern": "tele", "path": work}),
		claudetest.Use("Glob", map[string]any{"pattern": "*.txt", "path": work}),
		claudetest.Say("done"),
	)
	r := claudetest.Run(t, tr.Claude, api, claudetest.Options{
		Prompt: "go",
		Dir:    work,
		Args:   []string{"--allowedTools", "Bash", "Grep", "Glob", "--settings", settings, "--mcp-config", mcp},
		Env: []string{
			"PATH=" + bin, "SHELL=" + bin + "/bash",
			"CLAUDE_CODE_SHELL=" + bin + "/bash", "CLAUDE_CODE_SHELL_PREFIX=" + bin + "/tele-exec",
			"USE_BUILTIN_RIPGREP=0",
		},
	})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
	seen := map[string]bool{}
	started := map[int]bool{} // children whose first program was seen
	for _, c := range tr.Calls(t) {
		// Only a child's first program is Claude's choice; the shims exec
		// the real ones in the same process.
		if c.Name != "execve" || !c.Child || c.Failed() || started[c.PID] {
			continue
		}
		started[c.PID] = true
		p := c.ExecPath()
		seen[p] = true
		if filepath.Dir(p) != bin && p != "/bin/sh" {
			t.Errorf("Claude started %s, neither a shim nor /bin/sh: execve(%s", p, c.Line)
		}
	}
	t.Logf("programs started: %v", seen)
	for _, want := range []string{bin + "/bash", bin + "/rg", "/bin/sh"} {
		if !seen[want] {
			t.Errorf("Claude did not start %s", want)
		}
	}
	if !strings.Contains(r.Output.Result, "done") {
		t.Errorf("result %q", r.Output.Result)
	}
}
