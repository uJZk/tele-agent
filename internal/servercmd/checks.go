package servercmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/manifest"
	"github.com/ujzk/tele-agent/internal/preflight"
	"github.com/ujzk/tele-agent/internal/servercfg"
)

// The remote checks of docs/cli.md "预检与修复策略".

// Service is the systemd --user unit of tele server (docs/architecture.md
// "进程与角色").
const Service = "tele-server.service"

// minKernel is the oldest kernel the server side supports: the telefs
// service calls renameat2 (3.15) without a fallback.
var minKernel = [2]int{3, 15}

// Watches: the telefs service registers an inotify watch for each remote
// directory Claude looks into (docs/telefs.md "变更监视"), so the need is
// estimated from the directories under the home directory, where projects
// live. Counting stops at dirScanBudget.
const (
	minWatches     = 65536
	suggestWatches = 524288
	dirScanBudget  = 2 * time.Second
	dirScanLimit   = 1 << 20
	sysctlFile     = "/etc/sysctl.d/90-tele.conf"
)

// Manifest IDs of the changes install makes.
const (
	idBinary   = "binary"
	idConfig   = "config"
	idReload   = "systemd-daemon-reload"
	idUnit     = "systemd-unit"
	idEnable   = "systemd-enable"
	idLinger   = "linger"
	idFirewall = "firewall"
	idSysctl   = "sysctl-inotify"
	idNTP      = "ntp"
)

// installEnv is what the checks of one install share.
type installEnv struct {
	home     string
	userName string
	cfgPath  string
	unitPath string
	binPath  string // ~/.local/bin/tele
	exe      string // the running executable
	man      *manifest.Manifest
	// want is the configuration to install, nil to keep the current one.
	want *servercfg.Config
	// configChanged is set once the configuration is written, so that a
	// running service is restarted to load it.
	configChanged bool
}

func newInstallEnv(man *manifest.Manifest, cfgPath string) (*installEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	return &installEnv{
		home:     home,
		userName: u.Username,
		cfgPath:  cfgPath,
		unitPath: filepath.Join(cfgDir, "systemd", "user", Service),
		binPath:  filepath.Join(home, ".local", "bin", "tele"),
		exe:      exe,
		man:      man,
	}, nil
}

func (e *installEnv) checks() []preflight.Check {
	return []preflight.Check{
		{Name: "kernel", Run: e.checkKernel},
		{Name: "/proc", Run: e.checkProc},
		{Name: "inotify", Run: e.checkInotify},
		{Name: "binary", Run: e.checkBinary},
		{Name: "configuration", Run: e.checkConfig},
		{Name: "systemd service", Run: e.checkService},
		{Name: "linger", Run: e.checkLinger},
		{Name: "firewall", Run: e.checkFirewall},
		{Name: "inotify watches", Run: e.checkWatches},
		{Name: "clock", Run: e.checkClock},
		{Name: "tools", Run: e.checkTools},
	}
}

func (e *installEnv) checkKernel(context.Context) preflight.Result {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: err.Error()}
	}
	release := unix.ByteSliceToString(uts.Release[:])
	v, ok := kernelVersion(release)
	if !ok || v[0] < minKernel[0] || v[0] == minKernel[0] && v[1] < minKernel[1] {
		return preflight.Result{Status: preflight.Fatal, Detail: fmt.Sprintf("Linux %s; tele server needs %d.%d or later", release, minKernel[0], minKernel[1])}
	}
	return preflight.Result{Detail: "Linux " + release}
}

// kernelVersion parses the major and minor numbers of a kernel release.
func kernelVersion(release string) ([2]int, bool) {
	var v [2]int
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return v, false
	}
	for i := range 2 {
		end := strings.IndexFunc(parts[i], func(r rune) bool { return r < '0' || r > '9' })
		if end < 0 {
			end = len(parts[i])
		}
		n, err := strconv.Atoi(parts[i][:end])
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// checkProc: the telefs service reaches files through /proc/self/fd
// (docs/telefs.md "对象标识").
func (e *installEnv) checkProc(context.Context) preflight.Result {
	if _, err := os.ReadDir("/proc/self/fd"); err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: fmt.Sprintf("cannot read /proc/self/fd (%v); tele server needs /proc mounted", err)}
	}
	return preflight.Result{Detail: "mounted"}
}

func (e *installEnv) checkInotify(context.Context) preflight.Result {
	if _, err := os.Stat("/proc/sys/fs/inotify/max_user_watches"); err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: fmt.Sprintf("no /proc/sys/fs/inotify (%v); tele server needs a kernel with inotify", err)}
	}
	return preflight.Result{Detail: "available"}
}

// serviceBin is the executable the service runs: ~/.local/bin/tele, unless
// tele runs from a location the user cannot write to, such as a system
// package.
func (e *installEnv) serviceBin() string {
	if unix.Access(filepath.Dir(e.exe), unix.W_OK) != nil {
		return e.exe
	}
	return e.binPath
}

func (e *installEnv) checkBinary(context.Context) preflight.Result {
	bin := e.serviceBin()
	if bin == e.exe || sameContent(bin, e.exe) {
		return preflight.Result{Detail: bin}
	}
	return preflight.Result{
		Status: preflight.Fixable,
		Detail: "install " + e.exe + " as " + bin,
		Fix: func(_ context.Context, rec preflight.Recorder) error {
			b, err := os.ReadFile(e.exe)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil { //nolint:gosec // ~/.local/bin is conventionally 0755
				return err
			}
			return rec.WriteFile(idBinary, "installed "+bin, bin, b, 0o755)
		},
	}
}

func sameContent(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil || fa.Size() != fb.Size() {
		return false
	}
	if os.SameFile(fa, fb) {
		return true
	}
	ba, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	bb, err := os.ReadFile(b)
	return err == nil && bytes.Equal(ba, bb)
}

func (e *installEnv) checkConfig(context.Context) preflight.Result {
	cur, _, err := servercfg.Load(e.cfgPath)
	switch {
	case e.want != nil && (cur == nil || *cur != *e.want):
		detail := "write the new key to " + e.cfgPath
		if cur != nil && cur.PSK == e.want.PSK {
			detail = "listen on " + e.want.Listen
		}
		return preflight.Result{Status: preflight.Fixable, Detail: detail, Fix: e.writeConfig}
	case err == nil:
		return preflight.Result{Detail: e.cfgPath + ", listening on " + cur.Listen}
	case errors.Is(err, servercfg.ErrNotConfigured):
		return preflight.Result{Status: preflight.Fatal, Detail: "not paired: run `tele server install` with the pairing string from `tele host add`"}
	}
	fi, serr := os.Stat(e.cfgPath)
	if serr == nil && fi.Mode().Perm()&0o077 != 0 {
		return preflight.Result{
			Status: preflight.Fixable,
			Detail: "make " + e.cfgPath + " private (mode 0600)",
			Fix: func(context.Context, preflight.Recorder) error {
				return os.Chmod(e.cfgPath, 0o600)
			},
		}
	}
	return preflight.Result{Status: preflight.Fatal, Detail: err.Error()}
}

// writeConfig installs e.want. The configuration is recorded as created,
// never backed up: uninstall deletes it to revoke the key.
func (e *installEnv) writeConfig(_ context.Context, rec preflight.Recorder) error {
	if err := rec.Record(manifest.Entry{ID: idConfig, Desc: "configured tele server (deleting it revokes the key)", Path: e.cfgPath}); err != nil {
		return err
	}
	if err := servercfg.Save(e.cfgPath, e.want); err != nil {
		return err
	}
	e.configChanged = true
	return nil
}

// unit returns the systemd unit. KillMode=mixed sends SIGTERM to the
// server alone, which ends its sessions itself (docs/cli.md "其它命令");
// the rest of the cgroup is killed only after the timeout.
func (e *installEnv) unit() string {
	return `[Unit]
Description=tele server
Documentation=https://github.com/ujzk/tele-agent

[Service]
ExecStart=` + systemdQuote(e.serviceBin()) + ` server run
Restart=on-failure
RestartSec=5s
KillMode=mixed

[Install]
WantedBy=default.target
`
}

// systemdQuote quotes a path for a systemd command line.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

func systemctl(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if msg := oneLine(errb.String()); msg != "" {
			return out.String(), fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), msg)
		}
		return out.String(), fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// userManager reports whether a systemd user manager serves this user.
func userManager(ctx context.Context) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("no systemctl")
	}
	_, err := systemctl(ctx, "show", "--property=Version")
	return err
}

func parseProps(s string) map[string]string {
	m := map[string]string{}
	for line := range strings.SplitSeq(s, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

func (e *installEnv) checkService(ctx context.Context) preflight.Result {
	if err := userManager(ctx); err != nil {
		return preflight.Result{Status: preflight.Warn, Detail: fmt.Sprintf(
			"no systemd user manager (%v); run `tele server run` in the foreground, or as a system service with User=%s", err, e.userName)}
	}
	var todo []string
	unit := e.unit()
	cur, err := os.ReadFile(e.unitPath)
	unitChanged := err != nil || string(cur) != unit
	if unitChanged {
		todo = append(todo, "install "+e.unitPath)
	}
	out, err := systemctl(ctx, "show", "--property=UnitFileState,ActiveState", Service)
	if err != nil {
		return preflight.Result{Status: preflight.Fatal, Detail: err.Error()}
	}
	props := parseProps(out)
	enabled := props["UnitFileState"] == "enabled"
	active := props["ActiveState"] == "active"
	if !enabled {
		todo = append(todo, "enable")
	}
	switch {
	case !active:
		todo = append(todo, "start (state: "+props["ActiveState"]+"; see `journalctl --user -u "+Service+"`)")
	case unitChanged || e.configChanged:
		todo = append(todo, "restart")
	}
	if len(todo) == 0 {
		return preflight.Result{Detail: Service + " enabled and running"}
	}
	return preflight.Result{
		Status: preflight.Fixable,
		Detail: strings.Join(todo, ", "),
		Fix: func(ctx context.Context, rec preflight.Recorder) error {
			// Recorded first, so that rollback reloads last.
			if err := rec.Record(manifest.Entry{ID: idReload, Desc: "reloaded systemd", Undo: "systemctl --user daemon-reload"}); err != nil {
				return err
			}
			if unitChanged {
				if err := rec.WriteFile(idUnit, "installed "+e.unitPath, e.unitPath, []byte(unit), 0o644); err != nil {
					return err
				}
				if _, err := systemctl(ctx, "daemon-reload"); err != nil {
					return err
				}
			}
			if !enabled {
				if _, err := systemctl(ctx, "enable", Service); err != nil {
					return err
				}
			}
			if err := rec.Record(manifest.Entry{ID: idEnable, Desc: "enabled and started " + Service, Undo: "systemctl --user disable --now " + Service}); err != nil {
				return err
			}
			if _, err := systemctl(ctx, "restart", Service); err != nil {
				return err
			}
			e.configChanged = false
			// A server that cannot start (the port is taken) fails
			// within moments; the check that follows reports it.
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			return nil
		},
	}
}

// checkLinger: without linger, the user manager, and the service with it,
// stops when the user's last session ends.
func (e *installEnv) checkLinger(ctx context.Context) preflight.Result {
	if userManager(ctx) != nil {
		return preflight.Result{Status: preflight.Warn, Detail: "no systemd user manager"}
	}
	if _, err := os.Stat(filepath.Join("/var/lib/systemd/linger", e.userName)); err == nil {
		return preflight.Result{Detail: "enabled"}
	}
	return preflight.Result{
		Status: preflight.Consent,
		// polkit's set-self-linger is usually allowed in an active
		// session and may ask for authentication over SSH.
		Detail:      "not enabled: the service stops when " + e.userName + " logs out",
		Commands:    []string{"loginctl enable-linger " + preflight.ShellQuote(e.userName)},
		Undo:        &manifest.Entry{ID: idLinger, Desc: "enabled linger", Undo: "loginctl disable-linger " + preflight.ShellQuote(e.userName), Consent: true},
		Declined:    preflight.Warn,
		Consequence: "tele server stops when " + e.userName + " logs out",
	}
}

func (e *installEnv) listenPort() string {
	listen := ""
	if e.want != nil {
		listen = e.want.Listen
	} else if cur, _, err := servercfg.Load(e.cfgPath); err == nil {
		listen = cur.Listen
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	return port
}

func (e *installEnv) checkFirewall(ctx context.Context) preflight.Result {
	port := e.listenPort()
	if port == "" {
		return preflight.Result{Status: preflight.Warn, Detail: "no listening port configured"}
	}
	spec := port + "/tcp"
	res := preflight.Result{Declined: preflight.Warn, Consequence: "clients cannot connect until TCP port " + port + " is open"}
	switch {
	case firewalldRunning(ctx):
		out, _ := exec.CommandContext(ctx, "firewall-cmd", "--query-port="+spec).Output()
		if strings.TrimSpace(string(out)) == "yes" {
			return preflight.Result{Detail: "firewalld allows " + spec}
		}
		res.Status = preflight.Consent
		res.Detail = "firewalld does not allow " + spec
		res.Commands = []string{"sudo firewall-cmd --permanent --add-port=" + spec, "sudo firewall-cmd --reload"}
		res.Undo = &manifest.Entry{ID: idFirewall, Desc: "opened " + spec + " in firewalld", Consent: true,
			Undo: "sudo firewall-cmd --permanent --remove-port=" + spec + " && sudo firewall-cmd --reload"}
		return res
	case ufwActive():
		// Reading ufw's rules needs root, so only a rule tele added is known.
		if e.man.Has(idFirewall) {
			return preflight.Result{Detail: "ufw: tele allowed " + spec}
		}
		res.Status = preflight.Consent
		res.Detail = "ufw is active and may block " + spec
		res.Commands = []string{"sudo ufw allow " + spec}
		res.Undo = &manifest.Entry{ID: idFirewall, Desc: "opened " + spec + " in ufw", Consent: true, Undo: "sudo ufw delete allow " + spec}
		return res
	}
	return preflight.Result{Detail: "no firewalld or ufw; make sure cloud security groups and other firewalls let TCP port " + port + " in"}
}

func firewalldRunning(ctx context.Context) bool {
	if _, err := exec.LookPath("firewall-cmd"); err != nil {
		return false
	}
	out, _ := exec.CommandContext(ctx, "firewall-cmd", "--state").Output()
	return strings.TrimSpace(string(out)) == "running"
}

func ufwActive() bool {
	if _, err := exec.LookPath("ufw"); err != nil {
		return false
	}
	b, err := os.ReadFile("/etc/ufw/ufw.conf")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ENABLED="); ok {
			return strings.Trim(v, `"' `) == "yes"
		}
	}
	return false
}

func (e *installEnv) checkWatches(ctx context.Context) preflight.Result {
	b, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return preflight.Result{Status: preflight.Warn, Detail: err.Error()}
	}
	cur, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return preflight.Result{Status: preflight.Warn, Detail: "cannot parse fs.inotify.max_user_watches"}
	}
	need := max(minWatches, 2*countDirs(ctx, e.home))
	if cur >= need {
		return preflight.Result{Detail: fmt.Sprintf("fs.inotify.max_user_watches = %d", cur)}
	}
	set := max(suggestWatches, need)
	return preflight.Result{
		Status: preflight.Consent,
		Detail: fmt.Sprintf("fs.inotify.max_user_watches = %d, want at least %d", cur, need),
		Commands: []string{
			preflight.WriteRootFile(sysctlFile, fmt.Sprintf("# Written by tele server install.\nfs.inotify.max_user_watches = %d\n", set)),
			"sudo sysctl --system >/dev/null",
		},
		Undo:        &manifest.Entry{ID: idSysctl, Desc: "raised fs.inotify.max_user_watches", Consent: true, Undo: "sudo rm -f " + sysctlFile + " && sudo sysctl --system >/dev/null"},
		Declined:    preflight.Warn,
		Consequence: "change notification may run out of watches and fall back to short cache TTLs",
	}
}

// countDirs counts the directories under root, stopping at dirScanLimit or
// after dirScanBudget.
func countDirs(ctx context.Context, root string) int {
	ctx, cancel := context.WithTimeout(ctx, dirScanBudget)
	defer cancel()
	n := 0
	errStop := errors.New("stop")
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			n++
			if n >= dirScanLimit || ctx.Err() != nil {
				return errStop
			}
		}
		return nil
	})
	return n
}

// checkClock: SS2022 refuses connections beyond 30 s of clock skew. The
// skew itself is measured by the client (`tele doctor <alias>`).
func (e *installEnv) checkClock(ctx context.Context) preflight.Result {
	if _, err := exec.LookPath("timedatectl"); err != nil {
		return preflight.Result{Status: preflight.Warn, Detail: "cannot check clock synchronization (no timedatectl); SS2022 needs the clocks within 30 s"}
	}
	var errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "timedatectl", "show", "--property=NTP,NTPSynchronized")
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		if msg := oneLine(errb.String()); msg != "" {
			err = errors.New(msg)
		}
		return preflight.Result{Status: preflight.Warn, Detail: "cannot check clock synchronization: " + err.Error()}
	}
	props := parseProps(string(out))
	switch {
	case props["NTPSynchronized"] == "yes":
		return preflight.Result{Detail: "synchronized"}
	case props["NTP"] == "yes":
		return preflight.Result{Status: preflight.Warn, Detail: "NTP is enabled but not synchronized yet"}
	}
	return preflight.Result{
		Status:      preflight.Consent,
		Detail:      "NTP is not enabled",
		Commands:    []string{"sudo timedatectl set-ntp true"},
		Undo:        &manifest.Entry{ID: idNTP, Desc: "enabled NTP", Consent: true, Undo: "sudo timedatectl set-ntp false"},
		Declined:    preflight.Warn,
		Consequence: "the clock may drift beyond the 30 s SS2022 allows",
	}
}

// checkTools: Claude's Bash, Grep, Glob and git run these on the host
// (docs/exec.md "shim").
func (e *installEnv) checkTools(context.Context) preflight.Result {
	var missing []string
	for _, t := range []string{"bash", "rg", "git"} {
		if _, err := exec.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return preflight.Result{Status: preflight.Warn, Detail: "missing " + strings.Join(missing, ", ") + "; the Claude features that use them fail"}
	}
	return preflight.Result{Detail: "bash, rg, git"}
}

// oneLine joins the lines of a command's error output for the checklist.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(s), "\n", " ; ")), " ")
}
