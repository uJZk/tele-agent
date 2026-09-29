package view

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/ujzk/tele-agent/internal/fssvc"
	"github.com/ujzk/tele-agent/internal/telefs"
	"github.com/ujzk/tele-agent/internal/teleswitch"
	"github.com/ujzk/tele-agent/internal/testutil/helperproc"
	"github.com/ujzk/tele-agent/internal/testutil/privtest"
	"github.com/ujzk/tele-agent/internal/testutil/switchlib"
)

func TestMain(m *testing.M) {
	helperproc.Register("session", sessionHelper)
	helperproc.Register("view", func([]string) int { return HelperMain() })
	helperproc.Register("launch", LaunchMain)
	helperproc.Dispatch()
	goleak.VerifyTestMain(m)
}

const (
	sid     = "0123456789abcdef"
	sessDir = "/.tele/" + sid
	home    = "/home/bob"
)

// fixture is what the session helper works with.
type fixture struct {
	Remote string // the target's "/", served by fssvc
	Local  string // local sources of the binds
	Mnt    string // where telefs is mounted
	Lib    string // teleswitch
	Script string
	Switch bool // preload teleswitch
}

type outcome struct {
	Code   int
	Stdout string
	Stderr string
}

// TestRemoteView builds the remote view on telefs in a user namespace, as
// session main does, and runs a shell in it through the launch stage and
// teleswitch. It covers docs/filesystem.md "待验证的假设": pivot_root to a
// FUSE root and bind mounts in a nested mount namespace, all in a user
// namespace.
func TestRemoteView(t *testing.T) {
	privtest.RequireUserNS(t)
	privtest.RequireFUSE(t)
	fx := newFixture(t)
	fx.Lib = switchlib.Build(t)
	fx.Switch = true
	// Builtins only: the view has no libraries for new programs, and the
	// shell loaded its own before the switch.
	fx.Script = `echo "pwd=$(pwd)"; read -r h < /etc/hostname; echo "hostname=$h"; ` +
		`read -r m < ` + home + `/.claude/marker; echo "claude=$m"; ` +
		`read -r k < ` + sessDir + `/token; echo "token=$k"; ` +
		`test -f /bin/sh && echo "sh=file"; ` +
		`read -r l < /proc/self/status && echo "proc=$l"; ` +
		`echo x > /dev/null && echo "dev=ok"; ` +
		`echo written > /tmp/out && echo "tmp=ok"`
	r := runSession(t, fx)
	if r.Code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
	}
	for _, want := range []string{
		"pwd=" + home, "hostname=remote", "claude=local", "token=secret",
		"sh=file", "proc=Name:", "dev=ok", "tmp=ok",
	} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("output lacks %q:\n%s\nstderr:\n%s", want, r.Stdout, r.Stderr)
		}
	}
	if b, err := os.ReadFile(filepath.Join(fx.Remote, "tmp", "out")); err != nil || string(b) != "written\n" {
		t.Errorf("remote /tmp/out = %q, %v; want the shell's write", b, err)
	}
}

// TestLaunchViewWithoutSwitch runs a shell through the launch stage without
// teleswitch: it stays in the local view, where /proc is empty, which is
// what makes a Claude whose preload was ignored abort.
func TestLaunchViewWithoutSwitch(t *testing.T) {
	privtest.RequireUserNS(t)
	privtest.RequireFUSE(t)
	fx := newFixture(t)
	fx.Script = `if test -e /proc/self/status; then echo proc=present; else echo proc=empty; fi; echo "pwd=$(pwd)"`
	r := runSession(t, fx)
	if r.Code != 0 || !strings.Contains(r.Stdout, "proc=empty") || strings.Contains(r.Stdout, "pwd="+home) {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the local view with an empty /proc", r.Code, r.Stdout, r.Stderr)
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	fx := fixture{Remote: t.TempDir(), Local: t.TempDir(), Mnt: t.TempDir()}
	for _, d := range []string{"etc", "usr/bin", "tmp", "home/bob"} {
		if err := os.MkdirAll(filepath.Join(fx.Remote, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A merged-/usr target: /bin is a symlink, which must not send the
	// bind of /bin/sh to the local /usr/bin.
	if err := os.Symlink("usr/bin", filepath.Join(fx.Remote, "bin")); err != nil {
		t.Fatal(err)
	}
	for p, data := range map[string]string{
		"remote:etc/hostname":   "remote\n",
		"local:claude/marker":   "local\n",
		"local:sess/token":      "secret\n",
		"local:sh":              "shim\n",
		"remote:usr/bin/other":  "x",
		"remote:home/bob/.bash": "x",
	} {
		where, rel, _ := strings.Cut(p, ":")
		base := fx.Remote
		if where == "local" {
			base = fx.Local
		}
		full := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

// runSession runs the session helper in a user and mount namespace of its
// own and returns how the shell ended.
func runSession(t *testing.T, fx fixture) outcome {
	t.Helper()
	b, err := json.Marshal(fx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := helperproc.Command(t, "session", string(b))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
		AmbientCaps: Caps,
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("session helper: %v", err)
	}
	var r outcome
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("session helper output %q: %v", out, err)
	}
	return r
}

// sessionHelper plays session main: it mounts telefs over the remote root,
// builds the remote view and runs the script through the launch stage.
func sessionHelper(args []string) int {
	var fx fixture
	if err := json.Unmarshal([]byte(args[0]), &fx); err != nil {
		return fail(err)
	}
	r, err := session(fx)
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		return fail(err)
	}
	return 0
}

func session(fx fixture) (outcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	svc, err := fssvc.New(fssvc.Config{Root: fx.Remote})
	if err != nil {
		return outcome{}, err
	}
	defer func() { _ = svc.Close() }()
	fsys, err := telefs.Mount(fx.Mnt, telefs.Config{
		Opener: svc,
		Placeholders: []telefs.Placeholder{
			{Path: "/proc", Dir: true}, {Path: "/sys", Dir: true}, {Path: "/dev", Dir: true},
			{Path: home + "/.claude", Dir: true}, {Path: sessDir, Dir: true}, {Path: "/bin/sh"},
		},
		UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
		AttrTimeout: time.Second, EntryTimeout: time.Second,
	})
	if err != nil {
		return outcome{}, err
	}
	defer unmount(fsys)

	helper, err := helperproc.Exec(ctx, "view")
	if err != nil {
		return outcome{}, err
	}
	ns, err := Build(ctx, Spec{Root: fx.Mnt, Binds: []Bind{
		{Source: "/proc", Target: "/proc"}, {Source: "/sys", Target: "/sys"}, {Source: "/dev", Target: "/dev"},
		{Source: filepath.Join(fx.Local, "claude"), Target: home + "/.claude"},
		{Source: filepath.Join(fx.Local, "sess"), Target: sessDir},
		{Source: filepath.Join(fx.Local, "sh"), Target: "/bin/sh"},
	}}, helper)
	if err != nil {
		return outcome{}, err
	}
	defer func() { _ = ns.Close() }()

	launch, err := helperproc.Exec(ctx, "launch", "/bin/sh", "-c", fx.Script)
	if err != nil {
		return outcome{}, err
	}
	launch.SysProcAttr = LaunchAttr()
	launch.Env = append(launch.Env, "PATH=/usr/bin:/bin", teleswitch.EnvCheck+"="+sessDir)
	if fx.Switch {
		launch.Env = append(launch.Env, PreloadEnv+"="+fx.Lib)
		launch.Env = append(launch.Env, teleswitch.Env(3, home)...)
	}
	launch.ExtraFiles = []*os.File{ns}
	var so, se bytes.Buffer
	launch.Stdout, launch.Stderr = &so, &se
	err = launch.Run()
	r := outcome{Stdout: so.String(), Stderr: se.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.Code = ee.ExitCode()
	case err != nil:
		return outcome{}, err
	}
	return r, nil
}

// unmount unmounts telefs, retrying while the kernel still holds it busy.
func unmount(f *telefs.FS) {
	deadline := time.Now().Add(10 * time.Second)
	for f.Unmount() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, err)
	return 1
}
