package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/hostcfg"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/server"
	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
	"github.com/ujzk/tele-agent/internal/testutil/faultnet"
	"github.com/ujzk/tele-agent/internal/testutil/privtest"
	"github.com/ujzk/tele-agent/internal/testutil/servertest"
)

// teleRun is one run of the tele command with the real claude.
type teleRun struct {
	stdout, stderr string
	err            error
	output         struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	cache string // XDG_CACHE_HOME: holds the session directory
	home  string // the local HOME
	log   string // session main's debug log (--log)
}

// sessionLogs returns session main's log.
func (r *teleRun) sessionLogs() string {
	return r.log
}

// runTele runs "tele [--debug] dev:<dir> -p go <args>" against the server at
// endpoint ep with a fresh local HOME, config and cache.
func runTele(t *testing.T, tele, claude string, api *claudetest.API, ep, dir string, extraEnv []string, args ...string) *teleRun {
	t.Helper()
	cfg := t.TempDir()
	hostDir := filepath.Join(cfg, "tele", "hosts")
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(hostcfg.Host{Endpoint: ep, Token: "s3cret"})
	if err := os.WriteFile(filepath.Join(hostDir, "dev.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &teleRun{cache: t.TempDir(), home: t.TempDir()}
	logFile := filepath.Join(t.TempDir(), "session.log")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	teleArgs := []string{"--debug", "--log", logFile, "--claude", claude, "dev:" + dir, "-p", "go", "--output-format", "json"}
	teleArgs = append(append(teleArgs, claudetest.PinnedArgs...), args...)
	cmd := exec.CommandContext(ctx, tele, teleArgs...)
	cmd.Env = append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + r.home,
		"LANG=C.UTF-8",
		"XDG_CONFIG_HOME=" + cfg,
		"XDG_CACHE_HOME=" + r.cache,
		"ANTHROPIC_BASE_URL=" + api.URL(),
		"ANTHROPIC_API_KEY=sk-ant-tele-test",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
	}, extraEnv...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	r.err = cmd.Run()
	r.stdout, r.stderr = so.String(), se.String()
	if b, err := os.ReadFile(logFile); err == nil {
		r.log = string(b)
	}
	_ = json.Unmarshal(so.Bytes(), &r.output)
	return r
}

// requireViewSwitch skips t unless the real claude and the privileges of
// the view switch are available.
func requireViewSwitch(t *testing.T) (tele, claude string) {
	t.Helper()
	claude = claudetest.Require(t)
	privtest.RequireUserNS(t)
	privtest.RequireFUSE(t)
	return buildTele(t), claude
}

// TestTeleSwitchesView runs "tele dev:proj" with the real claude and this
// machine as the target, told apart by the target's HOME. Claude's Bash
// runs on the target, and its Read and Write reach the target's files
// through telefs in the remote view, while its own configuration stays in
// the local HOME.
func TestTeleSwitchesView(t *testing.T) {
	tele, claude := requireViewSwitch(t)
	rhome := t.TempDir()
	work := filepath.Join(rhome, "proj")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	target := &proto.TargetInfo{
		Hostname: "tele-e2e-host", OSPrettyName: "Test OS", User: "bob", Home: rhome,
		Shell: "/bin/sh", LoginPath: "/usr/local/bin:/usr/bin:/bin",
	}
	ep := servertest.StartRoot(t, "s3cret", target, "/")
	api := claudetest.NewAPI(t,
		claudetest.Use("Bash", map[string]any{"command": `echo "pwd=$(pwd) home=$HOME"; echo remote > made.txt`, "description": "Probe"}),
		claudetest.Use("Read", map[string]any{"file_path": filepath.Join(work, "made.txt")}),
		claudetest.Use("Write", map[string]any{"file_path": filepath.Join(work, "written.txt"), "content": "through telefs\n"}),
		claudetest.Say("done"),
	)
	r := runTele(t, tele, claude, api, ep.String(), "proj", nil, "--allowedTools", "Bash", "Read", "Write")
	if r.err != nil || r.output.Result != "done" {
		t.Fatalf("tele: %v, result %q\nstderr:\n%s\nsession log:\n%s", r.err, r.output.Result, r.stderr, r.sessionLogs())
	}
	if bash, ok := api.ToolResult(0); !ok || !strings.Contains(bash.ResultText(), "pwd="+work+" home="+rhome) {
		t.Errorf("Bash result %q, want it run in %s on the target", bash.ResultText(), work)
	}
	if read, ok := api.ToolResult(1); !ok || !strings.Contains(read.ResultText(), "remote") {
		t.Errorf("Read result %q, want the file Bash made", read.ResultText())
	}
	if b, err := os.ReadFile(filepath.Join(work, "written.txt")); err != nil || string(b) != "through telefs\n" {
		t.Errorf("written.txt on the target = %q, %v", b, err)
	}
	// The system prompt names the target (docs/claude-code.md "附加系统提示词").
	if reqs := api.Requests(); len(reqs) == 0 || !strings.Contains(reqs[0].System, "tele-e2e-host") {
		t.Errorf("system prompt lacks the target host")
	}
	// Claude's global configuration is local; the target's HOME has none.
	if _, err := os.Stat(filepath.Join(r.home, ".claude.json")); err != nil {
		t.Errorf("local ~/.claude.json: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(rhome, ".claude.json")); err == nil {
		t.Errorf("Claude wrote .claude.json into the target's HOME")
	}
	if _, err := os.Lstat(filepath.Join(rhome, ".claude")); err == nil {
		t.Errorf("Claude's config directory appeared in the target's HOME")
	}
	if lines := runtimeLookups(r.log); len(lines) != 0 {
		t.Errorf("Claude looked up runtime files after the switch:\n%s", strings.Join(lines, "\n"))
	}
	// A clean exit removes the session directory.
	if left, _ := filepath.Glob(filepath.Join(r.cache, "tele", "s", "*")); len(left) != 0 {
		t.Errorf("left behind: %q", left)
	}
}

// TestClaudeNeedsNoLocalFilesAfterSwitch runs claude against a target whose
// "/" is a bare directory tree: no libraries, no CA certificates, no
// /etc/passwd entry for the local user, no NSS configuration. Claude still
// runs, answers from the API and reads a target file, so after the switch
// it opens none of the local runtime files it loaded before
// (docs/claude-code.md "待验证的行为").
func TestClaudeNeedsNoLocalFilesAfterSwitch(t *testing.T) {
	tele, claude := requireViewSwitch(t)
	root := t.TempDir()
	for p, data := range map[string]string{
		"home/bob/proj/notes.txt": "only on the bare target\n",
		"etc/hostname":            "bare\n",
		// No entry for the local user, whoever runs the test.
		"etc/passwd": "bob:x:4242:4242::/home/bob:/bin/sh\n",
		"tmp/.keep":  "",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "tmp"), 0o1777); err != nil {
		t.Fatal(err)
	}
	target := &proto.TargetInfo{Hostname: "bare", User: "bob", Home: "/home/bob", Shell: "/bin/sh", LoginPath: "/usr/bin:/bin"}
	// The server's scratch directories go elsewhere than below the
	// target's HOME, which exists only in the bare tree.
	ep := servertest.StartConfig(t, server.Config{Token: []byte("s3cret"), FSRoot: root, Target: target, ScratchBase: t.TempDir()})
	// Over TLS, with a CA only the user's NODE_EXTRA_CA_CERTS names: tele's
	// CA bundle must carry it, as the target has no certificates at all.
	api := claudetest.NewTLSAPI(t, "127.0.0.1",
		claudetest.Use("Read", map[string]any{"file_path": "/home/bob/proj/notes.txt"}),
		claudetest.Say("done"),
	)
	r := runTele(t, tele, claude, api, ep.String(), "proj",
		[]string{"ANTHROPIC_BASE_URL=" + api.DirectURL(), "NODE_EXTRA_CA_CERTS=" + api.CAFile()},
		"--allowedTools", "Read")
	if r.err != nil || r.output.Result != "done" {
		t.Fatalf("tele: %v, result %q\nstderr:\n%s\nsession log:\n%s", r.err, r.output.Result, r.stderr, r.sessionLogs())
	}
	if read, ok := api.ToolResult(0); !ok || !strings.Contains(read.ResultText(), "only on the bare target") {
		t.Errorf("Read result %q, want the target's file", read.ResultText())
	}
	// telefs logs the runtime-like files the switched Claude looks up
	// (docs/telefs.md "组成"); a verified Claude looks up none.
	if lines := runtimeLookups(r.log); len(lines) != 0 {
		t.Errorf("Claude looked up runtime files after the switch:\n%s", strings.Join(lines, "\n"))
	}
}

// runtimeLookups returns the lines of a session log that record lookups of
// runtime-like files, except Claude's environment probe for musl, whose
// answer is rightly the target's (docs/claude-code.md "其它内置行为").
func runtimeLookups(log string) []string {
	var out []string
	for l := range strings.SplitSeq(log, "\n") {
		if strings.Contains(l, "runtime-like file looked up") && !strings.Contains(l, "/libc.musl-") {
			out = append(out, l)
		}
	}
	return out
}

// TestTeleSurvivesDisconnects cuts the transport again and again while
// Claude works: the session resumes each time, so the command's output,
// the file reads through telefs and Claude itself notice nothing
// (docs/transport.md "可恢复会话层").
func TestTeleSurvivesDisconnects(t *testing.T) {
	tele, claude := requireViewSwitch(t)
	rhome := t.TempDir()
	if err := os.WriteFile(filepath.Join(rhome, "notes.txt"), []byte("still here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := &proto.TargetInfo{Hostname: "flaky", User: "bob", Home: rhome, Shell: "/bin/sh", LoginPath: "/usr/local/bin:/usr/bin:/bin"}
	ep := servertest.StartRoot(t, "s3cret", target, "/")
	px := faultnet.New(t, strings.TrimPrefix(ep.String(), "unix:"))
	api := claudetest.NewAPI(t,
		claudetest.Use("Bash", map[string]any{"command": `for i in 1 2 3 4 5 6; do echo "line$i"; sleep 0.25; done`, "description": "Slow"}),
		claudetest.Use("Read", map[string]any{"file_path": filepath.Join(rhome, "notes.txt")}),
		claudetest.Say("done"),
	)
	stop := make(chan struct{})
	var cuts sync.WaitGroup
	cuts.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
				px.Cut()
			}
		}
	})
	r := runTele(t, tele, claude, api, "unix:"+px.Addr(), "", nil, "--allowedTools", "Bash", "Read")
	close(stop)
	cuts.Wait()
	if r.err != nil || r.output.Result != "done" {
		t.Fatalf("tele: %v, result %q\nstderr:\n%s\nsession log:\n%s", r.err, r.output.Result, r.stderr, r.sessionLogs())
	}
	if bash, ok := api.ToolResult(0); !ok || !strings.Contains(bash.ResultText(), "line1\nline2\nline3\nline4\nline5\nline6") {
		t.Errorf("Bash result %q, want every line once", bash.ResultText())
	}
	if read, ok := api.ToolResult(1); !ok || !strings.Contains(read.ResultText(), "still here") {
		t.Errorf("Read result %q", read.ResultText())
	}
	if !strings.Contains(r.log, "session resumed") {
		t.Errorf("the session never resumed; were there cuts?\n%s", r.log)
	}
}
