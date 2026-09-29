package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/claudever"
	"github.com/ujzk/tele-agent/internal/cli"
	"github.com/ujzk/tele-agent/internal/doctor"
	"github.com/ujzk/tele-agent/internal/preflight"
	"github.com/ujzk/tele-agent/internal/sigexit"
	"github.com/ujzk/tele-agent/internal/view"
)

// Internal roles of the tele executable, selected by argv[0]
// (docs/architecture.md "进程与角色"). A ':' cannot appear in a shim name.
const (
	RoleSession = "tele:session"
	RoleView    = "tele:view"
	RoleLaunch  = "tele:launch"
)

// Usage is the command line synopsis.
const Usage = "usage: tele [--debug] [--log <file>] [--endpoint <endpoint>] [--claude <path>] <alias>[:<dir>] [claude args...]"

// ExitFailure is tele's exit status when it fails before Claude ran, or
// cannot tell how Claude ended.
const ExitFailure = 1

// sessionConfig is what stage 1 hands session main.
type sessionConfig struct {
	Alias string `json:"alias"`
	Dir   string `json:"dir"`
	// Endpoint, if set, overrides the configured endpoint.
	Endpoint string `json:"endpoint,omitempty"`
	// Claude is the absolute path of the claude executable.
	Claude string   `json:"claude"`
	Args   []string `json:"args"`
	Debug  bool     `json:"debug,omitempty"`
	// LogFile, if set, receives session main's log too; the log in the
	// session directory goes away with it.
	LogFile string `json:"log_file,omitempty"`
}

// parseArgs parses tele's command line (docs/cli.md "命令形式"): tele's own
// options come before the alias, everything after it belongs to Claude.
func parseArgs(args []string) (sessionConfig, error) {
	var c sessionConfig
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		opt := args[0]
		args = args[1:]
		name, val, hasVal := strings.Cut(opt, "=")
		value := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if len(args) == 0 {
				return "", fmt.Errorf("option %s needs a value", name)
			}
			v := args[0]
			args = args[1:]
			return v, nil
		}
		var err error
		switch name {
		case "--debug":
			if hasVal {
				return c, fmt.Errorf("option %s takes no value", name)
			}
			c.Debug = true
		case "--endpoint":
			c.Endpoint, err = value()
		case "--claude":
			c.Claude, err = value()
		case "--log":
			c.LogFile, err = value()
		default:
			return c, fmt.Errorf("unknown option %s; %s", name, Usage)
		}
		if err != nil {
			return c, err
		}
	}
	if len(args) == 0 {
		return c, errors.New(Usage)
	}
	t, err := cli.ParseTarget(args[0])
	if err != nil {
		return c, err
	}
	c.Alias, c.Dir, c.Args = t.Alias, t.Dir, args[1:]
	return c, nil
}

// Main runs "tele <alias>[:<dir>] ...", stage 1 of the launcher: it
// resolves the claude executable, then runs session main in a new user and
// mount namespace and ends the way Claude ended.
func Main(args []string) int {
	cfg, err := parseArgs(args)
	if err != nil {
		return failf("%v", err)
	}
	if cfg.Claude == "" {
		cfg.Claude = "claude"
	}
	claude, err := exec.LookPath(cfg.Claude)
	if err != nil {
		return failf("cannot find Claude Code (%v); install it or pass --claude <path>", err)
	}
	if cfg.Claude, err = absPath(claude); err != nil {
		return failf("%v", err)
	}
	if cfg.LogFile != "" {
		if cfg.LogFile, err = absPath(cfg.LogFile); err != nil {
			return failf("%v", err)
		}
	}
	if !firstRun(os.Stderr) {
		return ExitFailure
	}
	warnVersion(os.Stderr, cfg.Claude)
	b, err := json.Marshal(cfg)
	if err != nil {
		return failf("%v", err)
	}

	cmd := exec.CommandContext(context.Background(), "/proc/self/exe", string(b))
	cmd.Args[0] = RoleSession
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = view.SessionAttr()
	sigs := notifySignals()
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return failf("%v", usernsError(err))
	}
	return waitMirroring(cmd, sigs)
}

func absPath(p string) (string, error) {
	if strings.HasPrefix(p, "/") {
		return p, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd + "/" + p, nil
}

// firstRun runs the local checks of tele doctor before the first session
// (docs/cli.md "预检与修复策略"). Once they pass, a stamp in the cache
// directory skips them.
func firstRun(w io.Writer) bool {
	dir, err := os.UserCacheDir()
	if err != nil {
		return true // the session reports what fails
	}
	stamp := filepath.Join(dir, "tele", "doctor-passed")
	if _, err := os.Stat(stamp); err == nil {
		return true
	}
	var out bytes.Buffer
	r := &preflight.Runner{Mode: preflight.CheckOnly, Out: &out}
	if !r.Run(context.Background(), doctor.LocalChecks()) {
		_, _ = fmt.Fprintf(w, "tele: this host is not ready for tele:\n%s\nRun `tele doctor` to repair it.\n", out.String())
		return false
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err == nil {
		_ = os.WriteFile(stamp, nil, 0o600)
	}
	return true
}

// warnVersion warns about a Claude Code version tele was not verified with
// (docs/claude-code.md). Failing to tell the version only warns too.
func warnVersion(w io.Writer, claude string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, err := claudever.Version(ctx, claude)
	if err != nil {
		_, _ = fmt.Fprintf(w, "tele: warning: %v\n", err)
		return
	}
	if msg := claudever.Warning(v); msg != "" {
		_, _ = fmt.Fprintf(w, "tele: warning: %s\n", msg)
	}
}

// usernsError explains a failure to create the user namespace
// (docs/filesystem.md "命名空间的构建").
func usernsError(err error) error {
	switch {
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		return fmt.Errorf("cannot create a user namespace (%w); unprivileged user namespaces may be restricted, "+
			"for example by kernel.apparmor_restrict_unprivileged_userns on Ubuntu", err)
	case errors.Is(err, unix.ENOSPC):
		return fmt.Errorf("cannot create a user namespace (%w); user.max_user_namespaces may be 0", err)
	}
	return fmt.Errorf("start session: %w", err)
}

// notifySignals catches the signals tele passes on. The terminal delivers
// SIGINT and SIGQUIT to the whole foreground process group, Claude
// included, so they are caught only to keep tele alive; SIGTERM and SIGHUP
// are forwarded. Catching rather than ignoring leaves the child's
// dispositions at their defaults.
func notifySignals() chan os.Signal {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, unix.SIGINT, unix.SIGQUIT, unix.SIGTERM, unix.SIGHUP)
	return sigs
}

// waitMirroring waits for the started cmd, forwarding SIGTERM and SIGHUP,
// and returns its exit code, or ends this process with the signal that
// killed it.
func waitMirroring(cmd *exec.Cmd, sigs <-chan os.Signal) int {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sigs:
			if s == unix.SIGTERM || s == unix.SIGHUP {
				_ = cmd.Process.Signal(s) // it may have exited meanwhile
			}
		case err := <-done:
			return exitCode(cmd.ProcessState, err)
		}
	}
}

// exitCode returns the exit code of a finished process, or ends this one
// with the signal that killed it.
func exitCode(ps *os.ProcessState, err error) int {
	if ps == nil {
		return failf("%v", err)
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		signal.Reset()
		sigexit.Raise(ws.Signal())
		return 128 + int(ws.Signal()) // a signal whose default does not end processes
	}
	return ps.ExitCode()
}

func failf(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "tele: "+format+"\n", args...)
	return ExitFailure
}
