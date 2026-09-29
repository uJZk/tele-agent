package teleswitch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/shim"
	"github.com/ujzk/tele-agent/internal/testutil/helperproc"
	"github.com/ujzk/tele-agent/internal/testutil/privtest"
	"github.com/ujzk/tele-agent/internal/testutil/switchlib"
)

func TestMain(m *testing.M) {
	helperproc.Register("switch", switchHelper)
	helperproc.Register("hold-ns", holdNSHelper)
	helperproc.Dispatch()
	os.Exit(m.Run())
}

// TestConstantsMatchC checks the constants shared with csrc/teleswitch.c.
func TestConstantsMatchC(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("csrc", "teleswitch.c"))
	if err != nil {
		t.Fatal(err)
	}
	defs := map[string]string{}
	re := regexp.MustCompile(`(?m)^#define (TS_[A-Z_]+) (\S+)`)
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		defs[m[1]] = strings.Trim(m[2], `"`)
	}
	want := map[string]string{
		"TS_ENV_FD":     EnvFD,
		"TS_ENV_DIR":    EnvDir,
		"TS_ENV_PREFIX": EnvPrefix,
		"TS_ENV_CHECK":  EnvCheck,
		"TS_EXIT_CODE":  strconv.Itoa(ExitCode),
		"TS_DIAG_FD":    strconv.Itoa(DiagFD),
	}
	for k, v := range want {
		if defs[k] != v {
			t.Errorf("%s = %q in C, %q in Go", k, defs[k], v)
		}
	}
	if EnvCheck != shim.SessionEnv {
		t.Errorf("EnvCheck %q is not the shim's session variable %q", EnvCheck, shim.SessionEnv)
	}
	for _, e := range []string{EnvFD, EnvDir} {
		if !strings.HasPrefix(e, EnvPrefix) {
			t.Errorf("%s lacks the prefix %s", e, EnvPrefix)
		}
	}
}

// switchResult is what the switch helper reports.
type switchResult struct {
	Code   int
	Stdout string
	Stderr string
}

// TestSwitch preloads the library into a shell and checks that the shell
// runs in the target mount namespace, in the working directory, without
// the switch variables and without capabilities; and that every failure
// ends the process before the shell runs.
func TestSwitch(t *testing.T) {
	privtest.RequireUserNS(t)
	lib := switchlib.Build(t)
	dir := t.TempDir()
	// The shell prints where it runs, what the target view holds, the
	// switch variables left in its environment, and its capabilities,
	// all with builtins so that no other program is involved.
	const script = `echo "pwd=$(pwd)"; for f in *; do echo "entry=$f"; done; ` +
		`env | while read -r l; do case $l in LD_PRELOAD=*|TELE_SWITCH_*) echo "leaked=$l";; esac; done; ` +
		`while read -r k v; do case $k in CapEff:|CapPrm:|CapAmb:) echo "$k$v";; esac; done < /proc/self/status`

	for _, tc := range []struct {
		name   string
		env    string // extra environment, overriding the defaults
		ok     bool
		reason string // expected in the diagnostic
	}{
		{name: "switch", ok: true},
		{name: "descriptor not open", env: EnvFD + "=9", reason: "setns failed, errno 9"},
		{name: "not a namespace", env: EnvFD + "=0", reason: "setns failed"},
		{name: "no descriptor", env: EnvFD + "=", reason: "reading"},
		{name: "session directory missing", env: EnvCheck + "=" + dir + "/nope", reason: "finding the session directory"},
		{name: "working directory missing", env: EnvDir + "=" + dir + "/nope", reason: "chdir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := helperproc.Command(t, "switch", lib, dir, tc.env, script)
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
				UidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
				GidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}},
				AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_CHROOT},
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("helper: %v", err)
			}
			var r switchResult
			if err := json.Unmarshal(out, &r); err != nil {
				t.Fatalf("helper output %q: %v", out, err)
			}
			if !tc.ok {
				if r.Code != ExitCode || r.Stdout != "" || !strings.Contains(r.Stderr, "tele: teleswitch: ") || !strings.Contains(r.Stderr, tc.reason) {
					t.Fatalf("exit %d, stdout %q, stderr %q; want exit %d before the shell ran, naming %q", r.Code, r.Stdout, r.Stderr, ExitCode, tc.reason)
				}
				return
			}
			if r.Code != 0 {
				t.Fatalf("exit %d, stderr %q", r.Code, r.Stderr)
			}
			for _, want := range []string{"pwd=" + dir, "entry=sess", "CapEff:0000000000000000", "CapPrm:0000000000000000", "CapAmb:0000000000000000"} {
				if !strings.Contains(r.Stdout, want+"\n") {
					t.Errorf("output lacks %q:\n%s", want, r.Stdout)
				}
			}
			if strings.Contains(r.Stdout, "leaked=") {
				t.Errorf("switch variables left in the environment:\n%s", r.Stdout)
			}
		})
	}
}

// switchHelper runs in a user and mount namespace of its own: it builds
// the target mount namespace in a child, then runs /bin/sh -c script with
// the library preloaded, and prints how the shell ended as JSON.
// Arguments: library, directory, extra environment entry, script.
func switchHelper(args []string) int {
	lib, dir, extra, script := args[0], args[1], args[2], args[3]
	caps := []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_CHROOT}
	ctx := context.Background()
	hold, err := helperproc.Exec(ctx, "hold-ns", dir)
	if err != nil {
		return fail(err)
	}
	hold.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS, AmbientCaps: caps}
	stdin, err := hold.StdinPipe()
	if err != nil {
		return fail(err)
	}
	stdout, err := hold.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	if err := hold.Start(); err != nil {
		return fail(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = hold.Wait()
	}()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		return fail(fmt.Errorf("hold-ns: %q, %w", line, err))
	}
	ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/mnt", hold.Process.Pid))
	if err != nil {
		return fail(err)
	}
	defer func() { _ = ns.Close() }()

	sh := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	sh.Env = append([]string{"PATH=/usr/bin:/bin", "LD_PRELOAD=" + lib, EnvCheck + "=" + dir + "/sess"}, Env(3, dir)...)
	if extra != "" {
		sh.Env = append(sh.Env, extra) // os/exec keeps the last value
	}
	sh.ExtraFiles = []*os.File{ns}
	sh.SysProcAttr = &syscall.SysProcAttr{AmbientCaps: caps}
	var so, se bytes.Buffer
	sh.Stdout, sh.Stderr = &so, &se
	err = sh.Run()
	r := switchResult{Stdout: so.String(), Stderr: se.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.Code = ee.ExitCode()
	case err != nil:
		return fail(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		return fail(err)
	}
	return 0
}

// holdNSHelper runs in a new mount namespace: it mounts an empty tmpfs on
// the directory given, creates "sess" in it, reports "ready" and keeps the
// namespace alive until its stdin closes.
func holdNSHelper(args []string) int {
	dir := args[0]
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sess"), 0o700); err != nil {
		return fail(err)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n') // EOF when the parent is done
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, err)
	return 1
}
