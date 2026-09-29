package launcher

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ujzk/tele-agent/internal/connectproxy"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/relay"
	"github.com/ujzk/tele-agent/internal/rexec"
	"github.com/ujzk/tele-agent/internal/scratch"
	"github.com/ujzk/tele-agent/internal/shimsrv"
	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

var (
	teleOnce     sync.Once
	teleBin      string
	errBuildTele error
)

// buildTele builds cmd/tele once per test binary.
func buildTele(t *testing.T) string {
	t.Helper()
	teleOnce.Do(func() {
		dir, err := os.MkdirTemp("", "tele-e2e-")
		if err != nil {
			errBuildTele = err
			return
		}
		teleBin = filepath.Join(dir, "tele")
		gobin, err := exec.LookPath("go")
		if err != nil {
			errBuildTele = err
			return
		}
		out, err := exec.CommandContext(context.Background(), gobin, "build", "-o", teleBin, "github.com/ujzk/tele-agent/cmd/tele").CombinedOutput()
		if err != nil {
			errBuildTele = &buildError{err: err, out: string(out)}
		}
	})
	if errBuildTele != nil {
		t.Fatalf("build tele: %v", errBuildTele)
	}
	return teleBin
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.out }

// TestExecChainWithClaude drives the real claude through the whole exec
// chain without the view switch: the Bash tool, a shell hook and Grep go
// through the shims in <sess>/bin to session main's shim server and relay,
// then over a session to tele server, and their output and the cwd file
// come back. The target is this machine, told apart by its own HOME.
func TestExecChainWithClaude(t *testing.T) {
	claude := claudetest.Require(t)
	tele := buildTele(t)
	target := testTarget(t)
	target.LoginPath = "/usr/local/bin:/usr/bin:/bin"
	ep, _ := startServer(t, "s3cret", target)
	rs, err := connect(t.Context(), ep, []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()

	// Without the view switch Claude sees session main's paths, so the
	// session directory keeps its local path.
	sess := filepath.Join(t.TempDir(), rs.ID)
	token := []byte("session-token")
	sd, err := prepareSessionDir(sessDirSpec{Dir: sess, Token: token, SystemPrompt: "# Target host (tele)\n", LogLevel: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sd.Close() }()
	for _, name := range []string{"bash", "sh", "tele-exec", "rg", "git", "uname"} {
		if err := os.Symlink(tele, filepath.Join(sess, binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	sm, err := scratch.New([]scratch.Area{{
		ID: proto.ScratchTmp, ClaudePath: sess + "/" + tmpDir, LocalPath: sess + "/" + tmpDir,
		RemotePath: filepath.Join(rs.ScratchDir, proto.ScratchTmp.Dir()),
	}})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir() // Claude's own HOME, with its configuration
	env := claudeEnv(claudeEnvSpec{
		UserEnv:  []string{"LANG=C.UTF-8", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1"},
		SessDir:  sess,
		Home:     home,
		User:     target.User,
		ProxyURL: "http://tele:unused@127.0.0.1:9", // the API is on loopback, which NO_PROXY exempts
	})
	rl := relay.New(relay.Config{
		SessDir:  sess,
		Baseline: env,
		Exec:     &rexec.Client{Opener: rs.Mux},
		Scratch:  sm,
		Logger:   sd.Log,
	})
	ln, err := shimsrv.Listen(rs.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		_ = (&shimsrv.Server{Token: token, UID: os.Getuid(), Handler: rl, Logger: sd.Log}).Serve(ctx, ln)
	}()
	defer func() {
		cancel()
		<-srvDone
		rl.Wait()
	}()

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(work, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hook > \"$HOME/hook-marker\""}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	api := claudetest.NewAPI(t,
		claudetest.Use("Bash", map[string]any{"command": `echo "remote-$((6*7)) home=$HOME"; cd sub`, "description": "Probe"}),
		claudetest.Use("Grep", map[string]any{"pattern": "needle", "path": work}),
		claudetest.Use("Bash", map[string]any{"command": "pwd", "description": "Where"}),
		claudetest.Say("done"),
	)
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "go", Dir: work, Home: home, Env: env,
		Args: []string{"--allowedTools", "Bash", "Grep", "--settings", settings},
	})
	if r.Err != nil {
		t.Fatalf("claude: %v\nstderr: %s\nsession log:\n%s", r.Err, r.Stderr, readLog(sess))
	}
	bash, ok := api.ToolResult(0)
	if !ok || !strings.Contains(bash.ResultText(), "remote-42 home="+target.Home) {
		t.Fatalf("Bash result %q, want it computed on the target with its HOME\nsession log:\n%s", bash.ResultText(), readLog(sess))
	}
	if b, err := os.ReadFile(filepath.Join(target.Home, "hook-marker")); err != nil || string(b) != "hook\n" {
		t.Errorf("hook marker in the target's HOME: %q, %v", b, err)
	}
	if grep, ok := api.ToolResult(1); !ok || !strings.Contains(grep.ResultText(), "a.txt") {
		t.Errorf("Grep result %q, want a.txt", grep.ResultText())
	}
	// The cwd file the first command wrote remotely came back, so Claude
	// runs the next command there (docs/claude-code.md "scratch 文件"). It
	// stays inside the project: Claude resets a cwd outside it.
	if pwd, ok := api.ToolResult(2); !ok || strings.TrimSpace(pwd.ResultText()) != filepath.Join(work, "sub") {
		t.Errorf("pwd after cd sub = %q, want %s", pwd.ResultText(), filepath.Join(work, "sub"))
	}
}

func readLog(sess string) string {
	b, _ := os.ReadFile(filepath.Join(sess, shimsrv.LogFile))
	return string(b)
}

// TestClaudeThroughProxy runs claude against tele's own CONNECT proxy with
// the environment tele builds: Claude authenticates with the password in
// HTTPS_PROXY, and a wrong password is refused.
func TestClaudeThroughProxy(t *testing.T) {
	claude := claudetest.Require(t)
	const host = "api.tele-compat.invalid"
	for _, tc := range []struct {
		name    string
		offered string // the password Claude is given
		ok      bool
	}{
		{"authenticated", "secret", true},
		{"wrong password", "guess", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := claudetest.NewTLSAPI(t, host, claudetest.Say("through tele's proxy"))
			var lc net.ListenConfig
			ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var dialed []string
			var mu sync.Mutex
			proxy := &connectproxy.Server{
				Token: "secret",
				// The API name does not resolve: send it to the stand-in.
				Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
					mu.Lock()
					dialed = append(dialed, addr)
					mu.Unlock()
					var d net.Dialer
					return d.DialContext(ctx, network, api.Addr())
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = proxy.Serve(ctx, ln)
			}()
			defer func() {
				cancel()
				<-done
			}()
			env := claudeEnv(claudeEnvSpec{
				UserEnv:  []string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1"},
				SessDir:  "/.tele/0123456789abcdef",
				Home:     t.TempDir(),
				User:     "bob",
				ProxyURL: proxyURL(ln.Addr().String(), tc.offered),
			})
			// Without a session directory, point the CA variables at the
			// stand-in's certificate directly.
			env = append(env, "SSL_CERT_FILE="+api.CAFile(), "NODE_EXTRA_CA_CERTS="+api.CAFile(), "PATH=/usr/bin:/bin")
			r := claudetest.Run(t, claude, api, claudetest.Options{Prompt: "hi", Env: env, Args: []string{"--max-turns", "1"}})
			mu.Lock()
			defer mu.Unlock()
			if tc.ok {
				if r.Err != nil || r.Output.Result != "through tele's proxy" {
					t.Fatalf("claude: %v, result %q\nstderr: %s", r.Err, r.Output.Result, r.Stderr)
				}
				if !slices.Contains(dialed, host+":443") {
					t.Errorf("proxy dialed %q, want %s:443", dialed, host)
				}
				return
			}
			if r.Err == nil || len(dialed) != 0 {
				t.Fatalf("claude with a wrong proxy password: %v, proxy dialed %q; want a refusal before any dial", r.Err, dialed)
			}
		})
	}
}
