package claudetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	args := append([]string{"-p", o.Prompt, "--output-format", "json"}, o.Args...)

	ctx, cancel := context.WithTimeout(t.Context(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, args...)
	cmd.Dir = o.Dir
	cmd.Env = dedupEnv(env)
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

// dedupEnv keeps the last entry of each variable, as later settings
// override earlier ones.
func dedupEnv(env []string) []string {
	last := map[string]int{}
	for i, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		last[k] = i
	}
	var out []string
	for i, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if last[k] == i {
			out = append(out, kv)
		}
	}
	return out
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
