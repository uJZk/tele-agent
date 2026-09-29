package claudecompat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

// loggingPrefix writes a CLAUDE_CODE_SHELL_PREFIX that logs its argv and
// runs its single argument with sh -c, as tele-exec does remotely.
func loggingPrefix(t *testing.T) (prefix, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "argv")
	return claudetest.WriteScript(t, t.TempDir(), "tele-exec", logArgv(log, `/bin/sh -c "$1"; exit`)+"\n"), log
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAppendSystemPromptFile pins docs/claude-code.md "附加系统提示词": the
// file's content reaches the system prompt of every agent request.
func TestAppendSystemPromptFile(t *testing.T) {
	claude := claudetest.Require(t)
	f := filepath.Join(t.TempDir(), "system-prompt.md")
	const marker = "Hostname: tele-compat-host"
	if err := os.WriteFile(f, []byte("# Target host (tele)\n\n- "+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := claudetest.NewAPI(t, claudetest.Say("ok"))
	r := claudetest.Run(t, claude, api, claudetest.Options{Prompt: "hi", Args: []string{"--append-system-prompt-file", f}})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
	reqs := api.AgentRequests()
	if len(reqs) == 0 {
		t.Fatal("no agent request")
	}
	for i, req := range reqs {
		if !strings.Contains(req.System, marker) {
			t.Errorf("agent request %d: system prompt lacks %q", i, marker)
		}
	}
}

// TestHookShellPrefix pins docs/claude-code.md "CLAUDE_CODE_SHELL_PREFIX"
// for shell hooks: the prefix runs with the hook command as its single
// argument.
func TestHookShellPrefix(t *testing.T) {
	claude := claudetest.Require(t)
	prefix, log := loggingPrefix(t)
	parentLog := filepath.Join(t.TempDir(), "parent")
	// Also log the prefix's parent; its NUL-separated cmdline plus a
	// newline is the log format of logArgv.
	if err := os.WriteFile(prefix, []byte("#!/bin/sh\n{ cat /proc/$PPID/cmdline; printf '\\n'; } >> '"+parentLog+"'\n"+logArgv(log, `/bin/sh -c "$1"; exit`)+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	settings := filepath.Join(t.TempDir(), "settings.json")
	hookCmd := "touch " + marker
	writeJSON(t, settings, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{
			"matcher": "Bash",
			"hooks":   []any{map[string]any{"type": "command", "command": hookCmd}},
		}},
	}})
	api := claudetest.NewAPI(t,
		claudetest.Use("Bash", map[string]any{"command": "true", "description": "Do nothing"}),
		claudetest.Say("done"),
	)
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "run it",
		Args:   []string{"--allowedTools", "Bash", "--settings", settings},
		Env:    []string{"CLAUDE_CODE_SHELL_PREFIX=" + prefix},
	})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("hook did not run: %v", err)
	}
	argv := commandInvocation(t, invocations(t, log), hookCmd)
	t.Logf("hook invocation: %q", argv)
	if len(argv) != 2 || argv[1] != hookCmd {
		t.Errorf("prefix argv %q, want [%s %q]", argv, prefix, hookCmd)
	}
	// The prefix is started by /bin/sh -c, which is why tele's sh shim
	// must recognize the wrapping (docs/exec.md "shim").
	parent := commandInvocation(t, invocations(t, parentLog), prefix)
	t.Logf("hook parent: %q", parent)
	if len(parent) != 3 || parent[0] != "/bin/sh" || parent[1] != "-c" {
		t.Errorf("hook parent %q, want [/bin/sh -c <wrapped command>]", parent)
	}
}

// TestMCPShellPrefix pins docs/claude-code.md "CLAUDE_CODE_SHELL_PREFIX" for
// stdio MCP servers: the prefix itself is spawned, with command and args
// quoted into one shell string as its only argument.
func TestMCPShellPrefix(t *testing.T) {
	claude := claudetest.Require(t)
	prefix, log := loggingPrefix(t)
	cfg := filepath.Join(t.TempDir(), "mcp.json")
	writeJSON(t, cfg, map[string]any{"mcpServers": map[string]any{
		"probe": map[string]any{"type": "stdio", "command": "/bin/echo", "args": []string{"tele mcp", "x"}},
	}})
	api := claudetest.NewAPI(t, claudetest.Say("ok"))
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "hi",
		Args:   []string{"--mcp-config", cfg, "--strict-mcp-config"},
		Env:    []string{"CLAUDE_CODE_SHELL_PREFIX=" + prefix},
	})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
	argv := commandInvocation(t, invocations(t, log), "/bin/echo")
	t.Logf("MCP invocation: %q", argv)
	if len(argv) != 2 {
		t.Fatalf("prefix argv %q, want a single argument", argv)
	}
	if got := shellWords(t, argv[1]); strings.Join(got, "|") != "/bin/echo|tele mcp|x" {
		t.Errorf("shell string %q splits into %q, want the command and its args", argv[1], got)
	}
}

// shellWords splits s as sh would, by running it through printf.
func shellWords(t *testing.T, s string) []string {
	t.Helper()
	out, err := runSh(t.Context(), `eval "set -- $1"; printf '%s\n' "$@"`, s)
	if err != nil {
		t.Fatalf("split %q: %v", s, err)
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// TestBuiltinRipgrepOff pins docs/claude-code.md "USE_BUILTIN_RIPGREP 与
// git": with USE_BUILTIN_RIPGREP=0, Grep runs the rg found in PATH.
func TestBuiltinRipgrepOff(t *testing.T) {
	claude := claudetest.Require(t)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "argv")
	// A stand-in rg that reports one match in the expected format.
	claudetest.WriteScript(t, bin, "rg", logArgv(log, `/bin/echo`)+"\n")
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("tele\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := claudetest.NewAPI(t,
		claudetest.Use("Grep", map[string]any{"pattern": "tele", "path": work}),
		claudetest.Say("done"),
	)
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "search",
		Dir:    work,
		Args:   []string{"--allowedTools", "Grep"},
		Env:    []string{"USE_BUILTIN_RIPGREP=0", "PATH=" + bin + ":/usr/bin:/bin"},
	})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
	argvs := invocations(t, log)
	t.Logf("rg invocations: %q", argvs)
	if len(argvs) == 0 || filepath.Base(argvs[0][0]) != "rg" {
		t.Fatalf("Grep did not run the rg in PATH")
	}
}
