package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/hostcfg"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
	"github.com/ujzk/tele-agent/internal/testutil/servertest"
)

// startTele runs "tele dev:<dir>" interactively on a terminal against the
// server at endpoint ep, with home as the local HOME and env added to the
// environment. dir must be the absolute path of <dir> on the target, so
// that Claude trusts it.
func startTele(t *testing.T, tele, claude string, api *claudetest.API, ep, home, dir, rel string, env []string) *claudetest.Session {
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
	log := filepath.Join(t.TempDir(), "session.log")
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(log)
			t.Logf("session log:\n%s", b)
		}
	})
	wrapper := claudetest.WriteScript(t, t.TempDir(), "tele-dev",
		`exec '`+tele+`' --debug --log '`+log+`' --claude '`+claude+`' 'dev:`+rel+`' "$@"`+"\n")
	return claudetest.Start(t, wrapper, api, claudetest.TTYOptions{Options: claudetest.Options{
		Dir: dir, Home: home,
		Env: append([]string{"XDG_CONFIG_HOME=" + cfg, "XDG_CACHE_HOME=" + t.TempDir()}, env...),
	}})
}

// TestTeleEditor opens files of both kinds in the user's editor under
// tele: the prompt and the user's memory file, which are local, and the
// project's memory file, which is the target's. The editor runs here, on
// the terminal, on a copy in the session directory, and its changes reach
// each file where it lives (docs/claude-code.md "外部编辑器与 IDE 探测").
func TestTeleEditor(t *testing.T) {
	tele, claude := requireViewSwitch(t)
	rhome := t.TempDir()
	work := filepath.Join(rhome, "proj")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "CLAUDE.md"), []byte("remote memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := &proto.TargetInfo{Hostname: "ed", User: "bob", Home: rhome, Shell: "/bin/sh", LoginPath: "/usr/local/bin:/usr/bin:/bin"}
	ep := servertest.StartRoot(t, "s3cret", target, "/")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("local memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "editor")
	ed := claudetest.WriteScript(t, t.TempDir(), "ed", `for f; do :; done
printf '%s|%s|%s\n' "$*" "$(tty)" "$(head -n1 "$f")" >> '`+log+`'
printf 'from the editor\n' >> "$f"
`)
	api := claudetest.NewAPI(t)
	s := startTele(t, tele, claude, api, ep.String(), home, work, "proj", []string{"VISUAL=" + ed + " -x"})
	s.Ready()

	s.Type("draft prompt")
	s.WaitText("draft prompt")
	s.Type("\x07") // ctrl+g
	s.WaitText("from the editor")
	s.Type("\x15")

	memory := func(keys string) {
		menus := strings.Count(s.Output(), "Enter to confirm")
		s.Command("/memory")
		s.WaitFor("the memory menu", func(out string) bool { return strings.Count(out, "Enter to confirm") > menus })
		s.Type(keys)
	}
	memory("\r") // ~/.claude/CLAUDE.md
	s.WaitText("Opened")
	memory("\x1b[B\r") // down, enter: ./CLAUDE.md
	if !s.Poll(30*time.Second, func() bool { b, _ := os.ReadFile(log); return strings.Count(string(b), "\n") == 3 }) {
		t.Fatalf("the editor did not run three times; screen:\n%s", claudetest.Text(s.Output()))
	}
	s.Stop()

	b, _ := os.ReadFile(log)
	runs := strings.Split(strings.TrimSpace(string(b)), "\n")
	firstLines := []string{"draft prompt", "local memory", "remote memory"}
	for i, run := range runs {
		f := strings.Split(run, "|")
		if len(f) != 3 || !strings.HasPrefix(f[0], "-x /") || !strings.HasPrefix(f[1], "/dev/pts/") || f[2] != firstLines[i] {
			t.Errorf("editor run %d: %q, want options, a terminal and the file's contents", i, f)
		}
		if copyPath := strings.TrimPrefix(f[0], "-x "); strings.HasPrefix(copyPath, rhome) || strings.HasPrefix(copyPath, home) {
			t.Errorf("editor run %d got %s, not a copy", i, copyPath)
		}
	}
	for file, want := range map[string]string{
		filepath.Join(home, ".claude", "CLAUDE.md"): "local memory\nfrom the editor\n",
		filepath.Join(work, "CLAUDE.md"):            "remote memory\nfrom the editor\n",
	} {
		if b, err := os.ReadFile(file); err != nil || string(b) != want {
			t.Errorf("%s holds %q, %v, want %q", file, b, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(rhome, ".claude", "CLAUDE.md")); err == nil {
		t.Error("the user's memory file was written on the target")
	}
}
