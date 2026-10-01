package claudetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Env names the claude executable the compatibility tests run.
const Env = "TELE_TEST_CLAUDE"

// runTimeout bounds one claude run.
const runTimeout = 2 * time.Minute

// Require returns the claude executable named by TELE_TEST_CLAUDE, and
// skips t when it is unset. Unlike the privileged tests these are opt-in:
// they need a claude binary, which CI does not have by default.
func Require(t testing.TB) string {
	t.Helper()
	path := os.Getenv(Env)
	if path == "" {
		t.Skipf("%s is not set; set it to a claude executable to run the Claude compatibility tests", Env)
	}
	if _, err := exec.LookPath(path); err != nil {
		t.Fatalf("%s=%q: %v", Env, path, err)
	}
	return path
}

// Options configure one claude run.
type Options struct {
	// Prompt is passed with -p.
	Prompt string
	// Args are appended after the prompt and output options.
	Args []string
	// Env is added to the minimal environment Run builds; entries override
	// it, including PATH and HOME.
	Env []string
	// Dir is the working directory; empty means a fresh temporary one.
	Dir string
	// Home is HOME; empty means a fresh temporary one. Claude's
	// configuration directory is <Home>/.claude, as under tele.
	Home string
}

// Result is how a claude run ended.
type Result struct {
	Stdout, Stderr string
	// Err is the exec error: nil for exit status 0.
	Err error
	// Output is the --output-format json result, if stdout held one.
	Output struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
}

// Must fails t unless claude exited with status 0, showing its stderr.
func (r Result) Must(t testing.TB) {
	t.Helper()
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s", r.Err, r.Stderr)
	}
}

// PinnedArgs are claude arguments every test passes, ahead of its own, so
// that a test does not depend on a default that changes between Claude Code
// versions. Without an explicit permission mode, newer versions run -p in
// auto mode, whose classifier asks the API about commands such as a for
// loop even under --allowedTools Bash; the scripted API cannot answer, so
// Claude blocks the command.
var PinnedArgs = []string{"--permission-mode", "default"}

// Run runs claude -p against api with a minimal environment that holds
// nothing from the test's own, so that the user's configuration, proxy
// and credentials never leak into a test.
func Run(t testing.TB, claude string, api *API, o Options) Result {
	t.Helper()
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	if o.Home == "" {
		o.Home = t.TempDir()
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + o.Home,
		"LANG=C.UTF-8",
		"ANTHROPIC_BASE_URL=" + api.URL(),
		"ANTHROPIC_API_KEY=sk-ant-tele-test",
		// No update checks, telemetry or other traffic beyond the API.
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
	}
	env = append(env, o.Env...)
	args := append(append([]string{"-p", o.Prompt, "--output-format", "json"}, PinnedArgs...), o.Args...)

	ctx, cancel := context.WithTimeout(t.Context(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, args...)
	cmd.Dir = o.Dir
	cmd.Env = env // os/exec keeps the last value of a repeated variable
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("claude did not finish within %v; stderr:\n%s", runTimeout, stderr.String())
	}
	r := Result{Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
	_ = json.Unmarshal(stdout.Bytes(), &r.Output) // not JSON: Output stays empty
	return r
}

// WriteScript writes an executable shell script to dir/name and returns
// its path.
func WriteScript(t testing.TB, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // an executable test script
		t.Fatal(err)
	}
	return p
}
