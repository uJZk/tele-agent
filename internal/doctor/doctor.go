// Package doctor implements "tele doctor": the local checks, and with an
// alias the connection to that host (docs/cli.md "预检与修复策略").
package doctor

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/claudever"
	"github.com/ujzk/tele-agent/internal/client"
	"github.com/ujzk/tele-agent/internal/hostcfg"
	"github.com/ujzk/tele-agent/internal/preflight"
	"github.com/ujzk/tele-agent/internal/resume"
	"github.com/ujzk/tele-agent/internal/sstransport"
	"github.com/ujzk/tele-agent/internal/view"
)

// Usage is the synopsis of "tele doctor".
const Usage = `usage: tele doctor [--claude <path>] [--yes | --check | --print-commands] [<alias>]`

// RoleProbe is the internal role that tries what session main does first
// in its namespaces (docs/architecture.md "进程与角色").
const RoleProbe = "tele:userns-probe"

// Clock skew: SS2022 refuses connections beyond maxSkew; warnSkew leaves
// room for drift.
const (
	maxSkew  = 30 * time.Second
	warnSkew = 10 * time.Second
)

// connectTimeout bounds the connection check.
const connectTimeout = 15 * time.Second

// Paths of the AppArmor profile.
const (
	apparmorProfile  = "/etc/apparmor.d/tele"
	apparmorRestrict = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
)

//go:embed tele.apparmor
var apparmorTemplate string

// Streams are the process's standard streams; tests replace them.
type Streams struct {
	// Terminal, if set, is where consent is asked and answered.
	Terminal io.ReadWriter
	Out, Err io.Writer
}

// Main runs "tele doctor <args>" and returns the exit status.
func Main(args []string, st Streams) int {
	err := run(context.Background(), args, st)
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(st.Err, "tele doctor: %v\n%s\n", err, Usage)
		return 2
	default:
		fmt.Fprintf(st.Err, "tele doctor: %v\n", err)
		return 1
	}
}

type usageError struct{ error }

func run(ctx context.Context, args []string, st Streams) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	claude := fs.String("claude", "claude", "the Claude Code executable")
	yes := fs.Bool("yes", false, "consent to every change that needs consent")
	check := fs.Bool("check", false, "only check")
	printCmds := fs.Bool("print-commands", false, "print the commands that need consent, run nothing")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	mode := preflight.Repair
	n := 0
	for _, f := range []struct {
		set bool
		m   preflight.Mode
	}{{*yes, preflight.Yes}, {*check, preflight.CheckOnly}, {*printCmds, preflight.PrintCommands}} {
		if f.set {
			n++
			mode = f.m
		}
	}
	if n > 1 {
		return usageError{errors.New("--yes, --check and --print-commands exclude each other")}
	}
	if fs.NArg() > 1 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(1))}
	}
	checks := append(LocalChecks(), preflight.Check{Name: "Claude Code", Run: func(ctx context.Context) preflight.Result {
		return checkClaude(ctx, *claude)
	}})
	if fs.NArg() == 1 {
		alias := fs.Arg(0)
		if _, err := hostcfg.Load(alias); err != nil {
			return err
		}
		checks = append(checks, preflight.Check{Name: "host " + alias, Run: func(ctx context.Context) preflight.Result {
			return checkHost(ctx, alias)
		}})
	}
	r := &preflight.Runner{Mode: mode, Out: st.Out, Terminal: st.Terminal}
	if !r.Run(ctx, checks) {
		return errors.New("some checks failed; see above")
	}
	return nil
}

// LocalChecks are the checks a session needs on the local host.
func LocalChecks() []preflight.Check {
	return []preflight.Check{
		{Name: "/dev/fuse", Run: checkFuse},
		{Name: "user namespaces", Run: checkUserns},
	}
}

func checkFuse(context.Context) preflight.Result {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: fmt.Sprintf("%v; load the module with `sudo modprobe fuse`", err)}
	}
	if err := unix.Access("/dev/fuse", unix.R_OK|unix.W_OK); err == nil {
		return preflight.Result{Detail: "readable and writable"}
	}
	// Distributions create it with mode 0666; the udev rule keeps the
	// mode across reboots.
	return preflight.Result{
		Status: preflight.Consent,
		Detail: "not readable and writable by this user",
		Commands: []string{
			preflight.WriteRootFile("/etc/udev/rules.d/60-tele-fuse.rules", `KERNEL=="fuse", MODE="0666"`),
			"sudo chmod 0666 /dev/fuse",
		},
		Declined:    preflight.Fatal,
		Consequence: "tele cannot mount the remote file system",
	}
}

// checkUserns runs the probe in the namespaces session main runs in.
func checkUserns(ctx context.Context) preflight.Result {
	err := probeUserns(ctx)
	if err == nil {
		return preflight.Result{Detail: "available"}
	}
	if b, rerr := os.ReadFile(apparmorRestrict); rerr == nil && strings.TrimSpace(string(b)) == "1" {
		return apparmorFix(err)
	}
	if b, rerr := os.ReadFile("/proc/sys/user/max_user_namespaces"); rerr == nil && strings.TrimSpace(string(b)) == "0" {
		return preflight.Result{Status: preflight.Fatal, Detail: "user.max_user_namespaces is 0; raise it, for example with `sudo sysctl user.max_user_namespaces=65536`"}
	}
	if b, rerr := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); rerr == nil && strings.TrimSpace(string(b)) == "0" {
		return preflight.Result{Status: preflight.Fatal, Detail: "kernel.unprivileged_userns_clone is 0; enable it with `sudo sysctl kernel.unprivileged_userns_clone=1`"}
	}
	return preflight.Result{Status: preflight.Fatal, Detail: err.Error()}
}

// apparmorFix installs the profile that allows user namespaces for this
// executable, rather than lifting the restriction for all programs.
func apparmorFix(probeErr error) preflight.Result {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: fmt.Sprintf("AppArmor restricts user namespaces (%v), and the tele executable cannot be located: %v", probeErr, err)}
	}
	if _, err := exec.LookPath("apparmor_parser"); err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: fmt.Sprintf("AppArmor restricts user namespaces (%v), and apparmor_parser is missing", probeErr)}
	}
	return preflight.Result{
		Status: preflight.Consent,
		Detail: "AppArmor restricts unprivileged user namespaces; install a profile that allows them for " + exe,
		Commands: []string{
			preflight.WriteRootFile(apparmorProfile, ApparmorProfile(exe)),
			"sudo apparmor_parser -r " + apparmorProfile,
		},
		Declined:    preflight.Fatal,
		Consequence: "tele cannot build its namespaces",
	}
}

// ApparmorProfile returns the profile for the executable at exe.
func ApparmorProfile(exe string) string {
	// A quoted AppArmor path still treats these as glob and escape
	// characters.
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`, "{", `\{`, "}", `\}`, "^", `\^`)
	return strings.ReplaceAll(apparmorTemplate, "@TELE@", r.Replace(exe))
}

// probeUserns runs RoleProbe the way the launcher runs session main.
func probeUserns(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/proc/self/exe")
	cmd.Args[0] = RoleProbe
	cmd.SysProcAttr = view.SessionAttr()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// ProbeMain is RoleProbe: it changes mount propagation, which needs
// CAP_SYS_ADMIN in the new user namespace. Ubuntu's AppArmor restriction
// lets the namespace be created but takes that capability away.
func ProbeMain() int {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Fprintf(os.Stderr, "cannot mount in a user namespace: %v\n", err)
		return 1
	}
	return 0
}

func checkClaude(ctx context.Context, claude string) preflight.Result {
	path, err := exec.LookPath(claude)
	if err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: fmt.Sprintf("cannot find Claude Code (%v); install it or pass --claude <path>", err)}
	}
	v, err := claudever.Version(ctx, path)
	if err != nil {
		return preflight.Result{Status: preflight.Warn, Detail: err.Error()}
	}
	if msg := claudever.Warning(v); msg != "" {
		return preflight.Result{Status: preflight.Warn, Detail: msg}
	}
	return preflight.Result{Detail: v + " (verified)"}
}

// checkHost connects to alias and reports the clock skew.
func checkHost(ctx context.Context, alias string) preflight.Result {
	h, err := hostcfg.Load(alias)
	if err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: err.Error()}
	}
	if h.PendingToken != "" {
		return preflight.Result{Status: preflight.Fatal, Detail: fmt.Sprintf("pairing not confirmed; run `tele host confirm %s`", alias)}
	}
	eps, token, err := h.ResolveAll("")
	if err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	s, err := client.ConnectDial(ctx, client.Rotate(eps), token, resume.Config{})
	if err != nil {
		detail := fmt.Sprintf("cannot connect to %s: %v", h.Endpoint, err)
		if errors.Is(err, sstransport.ErrNoResponse) {
			detail += "; check the clocks of both hosts (" + localClock(ctx) + ") and that the server was paired with this key"
		} else {
			detail += "; check that tele server runs there (`tele server install --check`) and that its port is reachable"
		}
		return preflight.Result{Status: preflight.Fatal, Detail: detail}
	}
	_ = s.Close()
	return skewResult(s)
}

func skewResult(s *client.Session) preflight.Result {
	t := s.Target
	skew := s.ClockSkew.Round(100 * time.Millisecond)
	detail := fmt.Sprintf("%s@%s, %s %s, rtt %v, clock skew %v", t.User, t.Hostname, t.OSPrettyName, t.Arch, s.RTT.Round(time.Millisecond), skew)
	switch abs := max(skew, -skew); {
	case abs >= maxSkew:
		return preflight.Result{Status: preflight.Fatal, Detail: detail + fmt.Sprintf("; SS2022 refuses connections beyond %v: synchronize the clocks (NTP)", maxSkew)}
	case abs >= warnSkew:
		return preflight.Result{Status: preflight.Warn, Detail: detail + fmt.Sprintf("; close to the %v SS2022 allows: synchronize the clocks (NTP)", maxSkew)}
	}
	return preflight.Result{Detail: detail}
}

// localClock describes whether the local clock is synchronized.
func localClock(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "timedatectl", "show", "--property=NTPSynchronized", "--value").Output()
	switch strings.TrimSpace(string(out)) {
	case "yes":
		return "the local clock is synchronized"
	case "no":
		return "the local clock is NOT synchronized"
	}
	if err != nil {
		return "cannot tell whether the local clock is synchronized"
	}
	return "local clock synchronization unknown"
}
