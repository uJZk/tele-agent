package launcher

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/cli"
	"github.com/ujzk/tele-agent/internal/connectproxy"
	"github.com/ujzk/tele-agent/internal/fssvc"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/relay"
	"github.com/ujzk/tele-agent/internal/resume"
	"github.com/ujzk/tele-agent/internal/rexec"
	"github.com/ujzk/tele-agent/internal/scratch"
	"github.com/ujzk/tele-agent/internal/shimsrv"
	"github.com/ujzk/tele-agent/internal/sysprompt"
	"github.com/ujzk/tele-agent/internal/telefs"
	"github.com/ujzk/tele-agent/internal/teleswitch"
	"github.com/ujzk/tele-agent/internal/view"
)

// Timeouts of session main.
const (
	// connectTimeout bounds establishing the session with tele server.
	connectTimeout = time.Minute
	// cacheTTL is how long the kernel caches telefs entries and
	// attributes while changes are pushed (docs/telefs.md "一致性").
	cacheTTL = 10 * time.Minute
	// unmountTimeout bounds unmounting telefs at the end; a mount still
	// in use by a leftover process is detached instead.
	unmountTimeout = 10 * time.Second
)

// sessionRoot is where the session directory appears in the remote view
// (docs/claude-code.md "注入的环境").
const sessionRoot = "/.tele"

// shimNames are the shims in <sess>/bin (docs/exec.md "shim"): the
// remote programs, and the local exec proxies of localProgs.
var (
	shimNames  = []string{"bash", "sh", "tele-exec", "rg", "git", "uname"}
	localProgs = map[string]bool{"ps": true}
)

// SessionMain is session main, stage 2 of the launcher
// (docs/cli.md "启动流程"). It runs in the user and mount namespace stage 1
// created, with the capabilities in view.Caps, and ends the way Claude
// ended. arg is the sessionConfig as JSON.
func SessionMain(arg string) int {
	var cfg sessionConfig
	if err := json.Unmarshal([]byte(arg), &cfg); err != nil {
		return failf("session: %v", err)
	}
	sigs := notifySignals()
	defer signal.Stop(sigs)
	s := &session{cfg: cfg}
	ps, err := s.run(sigs)
	s.teardown()
	if err != nil {
		return failf("%v", err)
	}
	return exitCode(ps, nil)
}

// session is the state of session main. Cleanups run in reverse order.
type session struct {
	cfg      sessionConfig
	log      *slog.Logger
	cleanups []func()
}

func (s *session) onExit(f func()) { s.cleanups = append(s.cleanups, f) }

func (s *session) teardown() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
}

// paths are the places of one session.
type paths struct {
	sid       string
	local     string // the session directory in session main's view
	claude    string // the session directory in the remote view
	mnt       string // where telefs is mounted in session main's view
	localHome string
	home      string // the target user's HOME
	workdir   string
}

// run starts everything, runs Claude and waits for it.
func (s *session) run(sigs <-chan os.Signal) (*os.ProcessState, error) {
	// Nothing session main mounts may reach the namespace it came from.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return nil, fmt.Errorf("make mounts private: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.onExit(cancel)

	ep, token, err := loadHost(s.cfg.Alias, s.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	cctx, ccancel := context.WithTimeout(ctx, connectTimeout)
	sink := newLogSink()
	rs, err := connect(cctx, ep, token, resume.Config{Logger: slog.New(sink).With("layer", "resume")})
	ccancel()
	if err != nil {
		return nil, fmt.Errorf("connect to %q: %w; run \"tele doctor %s\" to check the connection", s.cfg.Alias, err, s.cfg.Alias)
	}
	s.onExit(func() { _ = rs.Close() })

	p, err := s.layout(rs)
	if err != nil {
		return nil, err
	}
	sd, args, err := s.sessionDir(rs, p)
	if err != nil {
		return nil, err
	}
	s.log = sd.Log
	sink.Set(sd.Log.Handler())
	if err := s.shims(p); err != nil {
		return nil, err
	}
	fsys, err := s.mountTelefs(ctx, rs, p)
	if err != nil {
		return nil, err
	}
	proxyAddr, proxyToken, err := s.startProxy(ctx)
	if err != nil {
		return nil, err
	}
	lib := filepath.Join(p.local, libDir, "teleswitch.so")
	env := claudeEnv(claudeEnvSpec{
		UserEnv:  os.Environ(),
		SessDir:  p.claude,
		Home:     p.home,
		User:     rs.Target.User,
		ProxyURL: proxyURL(proxyAddr, proxyToken),
		Switch:   append(teleswitch.Env(3, p.workdir), view.PreloadEnv+"="+lib),
	})
	if err := s.startShimServer(ctx, rs, fsys, p, env); err != nil {
		return nil, err
	}
	ns, err := s.buildView(ctx, sd, p)
	if err != nil {
		return nil, err
	}
	return s.runClaude(env, args, ns, sigs)
}

// layout works out the session's paths and checks the working directory.
func (s *session) layout(rs *remoteSession) (paths, error) {
	p := paths{sid: rs.ID, home: rs.Target.Home, claude: path.Join(sessionRoot, rs.ID)}
	var err error
	if p.workdir, err = cli.ResolveDir(s.cfg.Dir, rs.Target.Home); err != nil {
		return p, err
	}
	if err := checkRemoteDir(rs.Mux, p.workdir); err != nil {
		return p, err
	}
	if p.localHome, err = os.UserHomeDir(); err != nil {
		return p, fmt.Errorf("local home directory: %w", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return p, fmt.Errorf("local cache directory: %w", err)
	}
	base := filepath.Join(cache, "tele", "s")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return p, fmt.Errorf("create %s: %w", base, err)
	}
	if err := checkExecutable(base); err != nil {
		return p, err
	}
	p.local = filepath.Join(base, rs.ID)
	p.mnt = p.local + ".root"
	return p, nil
}

// checkRemoteDir checks that dir is a directory on the target host
// (docs/cli.md "命令形式": it is never created).
func checkRemoteDir(o telefs.Opener, dir string) error {
	c, err := o.Open(proto.StreamFS)
	if err != nil {
		return fmt.Errorf("check %s: %w", dir, err)
	}
	defer func() { _ = c.Close() }()
	if err := proto.WriteFrame(c, &proto.FSRequest{Op: proto.FSGetattr, Path: dir}); err != nil {
		return fmt.Errorf("check %s: %w", dir, err)
	}
	var resp proto.FSResponse
	if err := proto.ReadFrame(c, &resp, proto.MaxDataFrame); err != nil {
		return fmt.Errorf("check %s: %w", dir, err)
	}
	switch {
	case resp.Errno != 0:
		return fmt.Errorf("remote directory %s: %w", dir, unix.Errno(resp.Errno))
	case resp.Attr == nil || resp.Attr.Mode&unix.S_IFMT != unix.S_IFDIR:
		return fmt.Errorf("remote path %s is not a directory", dir)
	}
	return nil
}

// checkExecutable refuses a directory on a noexec file system: the dynamic
// linker could not map the preload library from it (docs/filesystem.md
// "已知陷阱").
func checkExecutable(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", dir, err)
	}
	if st.Flags&unix.ST_NOEXEC != 0 {
		return fmt.Errorf("%s is on a noexec file system, which cannot hold the session directory; set XDG_CACHE_HOME to a directory elsewhere", dir)
	}
	return nil
}

// sessionDir prepares the session directory and returns Claude's
// arguments, which gain the appended system prompt (docs/claude-code.md
// "附加系统提示词").
func (s *session) sessionDir(rs *remoteSession, p paths) (*sessionDir, []string, error) {
	rest, prompts, files, err := sysprompt.SplitAppendArgs(s.cfg.Args)
	if err != nil {
		return nil, nil, err
	}
	var contents [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("--append-system-prompt-file: %w", err)
		}
		contents = append(contents, b)
	}
	level := slog.LevelInfo
	if s.cfg.Debug {
		level = slog.LevelDebug
	}
	var logCopy io.Writer
	if s.cfg.LogFile != "" {
		f, err := os.OpenFile(s.cfg.LogFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("--log: %w", err)
		}
		s.onExit(func() { _ = f.Close() })
		logCopy = f
	}
	sd, err := prepareSessionDir(sessDirSpec{
		LogCopy:      logCopy,
		Dir:          p.local,
		UserEnv:      os.Environ(),
		Token:        []byte(rand.Text()),
		SystemPrompt: sysprompt.Compose(sysprompt.Render(s.cfg.Alias, rs.Target, p.workdir), prompts, contents),
		LogLevel:     level,
		Warn:         os.Stderr,
	})
	if err != nil {
		return nil, nil, err
	}
	s.onExit(func() { _ = os.RemoveAll(p.local) })
	s.onExit(func() { _ = sd.Close() })
	sd.Log.Info("session started", "alias", s.cfg.Alias, "sid", p.sid)
	// Before the user's arguments: after a "--" it would be an argument.
	args := append([]string{"--append-system-prompt-file", path.Join(p.claude, systemPromptFile)}, rest...)
	return sd, args, nil
}

// shims installs the preload library and the shims: <sess>/tele is the
// tele executable, bind-mounted so that it exists in the remote view, and
// every shim is a relative symlink to it.
func (s *session) shims(p paths) error {
	lib, err := teleswitch.Library()
	if err != nil {
		return err
	}
	if err := writeNew(filepath.Join(p.local, libDir, "teleswitch.so"), lib); err != nil {
		return fmt.Errorf("write teleswitch: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the tele executable: %w", err)
	}
	tele := filepath.Join(p.local, "tele")
	if err := writeNew(tele, nil); err != nil {
		return err
	}
	if err := unix.Mount(exe, tele, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind the tele executable: %w", err)
	}
	s.onExit(func() { _ = unix.Unmount(tele, unix.MNT_DETACH) })
	for _, name := range shimNames {
		if err := os.Symlink("../tele", filepath.Join(p.local, binDir, name)); err != nil {
			return err
		}
	}
	for name := range localProgs {
		if err := os.Symlink("../tele", filepath.Join(p.local, binDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// placeholders are the mount points of the local set (docs/filesystem.md
// "本地集合").
func placeholders(p paths, managed bool) []telefs.Placeholder {
	ps := []telefs.Placeholder{
		{Path: "/proc", Dir: true}, {Path: "/sys", Dir: true}, {Path: "/dev", Dir: true},
		{Path: path.Join(p.home, ".claude"), Dir: true},
		{Path: p.claude, Dir: true},
		{Path: "/bin/sh"},
	}
	if managed {
		ps = append(ps, telefs.Placeholder{Path: managedDir, Dir: true})
	}
	return ps
}

// managedDir holds Claude's managed policy, which must come from this
// machine.
const managedDir = "/etc/claude-code"

// mountTelefs mounts the target's "/" and starts the change watch.
func (s *session) mountTelefs(ctx context.Context, rs *remoteSession, p paths) (*telefs.FS, error) {
	if err := os.MkdirAll(filepath.Join(p.localHome, ".claude"), 0o700); err != nil {
		return nil, fmt.Errorf("create ~/.claude: %w", err)
	}
	local, err := fssvc.New(fssvc.Config{Root: p.localHome, NoWatch: true, Logger: s.log.With("svc", "local-fs")})
	if err != nil {
		return nil, err
	}
	s.onExit(func() { _ = local.Close() })
	if err := os.Mkdir(p.mnt, 0o700); err != nil {
		return nil, fmt.Errorf("create the telefs mount point: %w", err)
	}
	s.onExit(func() { _ = os.Remove(p.mnt) })
	//nolint:contextcheck // the mount lives until unmount, which teardown does
	fsys, err := telefs.Mount(p.mnt, telefs.Config{
		Opener:       rs.Mux,
		Placeholders: placeholders(p, isDir(managedDir)),
		LocalNames:   []telefs.LocalNames{{Dir: p.home, Prefix: ".claude.json", Opener: local}},
		UID:          uint32(os.Getuid()),
		GID:          uint32(os.Getgid()),
		AttrTimeout:  cacheTTL,
		EntryTimeout: cacheTTL,
		Logger:       s.log.With("svc", "telefs"),
	})
	if err != nil {
		return nil, err
	}
	s.onExit(func() { s.unmount(fsys, p.mnt) })
	w, err := rs.Mux.Open(proto.StreamWatch)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	wctx, stop := context.WithCancel(ctx)
	go func() {
		defer close(done)
		if err := fsys.RunWatch(wctx, w); err != nil && wctx.Err() == nil {
			s.log.Warn("change watch ended; caching briefly from now on", "err", err)
		}
	}()
	s.onExit(func() {
		stop()
		_ = w.Close()
		<-done
	})
	return fsys, nil
}

// unmount unmounts telefs. A mount a leftover process still uses is
// detached: session main is about to exit anyway.
func (s *session) unmount(fsys *telefs.FS, mnt string) {
	done := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(unmountTimeout)
		for {
			err := fsys.Unmount()
			if err == nil || time.Now().After(deadline) {
				done <- err
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	select {
	case err := <-done:
		if err == nil {
			return
		}
		s.log.Warn("unmount telefs", "err", err)
	case <-time.After(2 * unmountTimeout):
		s.log.Warn("unmount telefs: timed out")
	}
	_ = unix.Unmount(mnt, unix.MNT_DETACH)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// startProxy starts the CONNECT proxy for Claude's outgoing connections
// (docs/filesystem.md "本地集合").
func (s *session) startProxy(ctx context.Context) (string, string, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("start the proxy: %w", err)
	}
	token := rand.Text()
	srv := &connectproxy.Server{
		Token: token,
		// The user's own settings, not the ones tele gives Claude.
		Upstream: connectproxy.UpstreamFromEnv(os.Getenv),
		Logger:   s.log.With("svc", "proxy"),
	}
	pctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(pctx, ln)
	}()
	s.onExit(func() {
		stop()
		<-done
	})
	return ln.Addr().String(), token, nil
}

// startShimServer serves the shims: the scratch areas, the relay and the
// shim server on the session's abstract socket.
func (s *session) startShimServer(ctx context.Context, rs *remoteSession, fsys *telefs.FS, p paths, env []string) error {
	cfgDir := path.Join(p.home, ".claude")
	localCfg := filepath.Join(p.localHome, ".claude")
	areas := []scratch.Area{
		{ID: proto.ScratchTmp, ClaudePath: path.Join(p.claude, tmpDir), LocalPath: filepath.Join(p.local, tmpDir)},
		{ID: proto.ScratchSnapshots, ClaudePath: path.Join(cfgDir, "shell-snapshots"), LocalPath: filepath.Join(localCfg, "shell-snapshots")},
		{ID: proto.ScratchSessionEnv, ClaudePath: path.Join(cfgDir, "session-env"), LocalPath: filepath.Join(localCfg, "session-env")},
	}
	for i := range areas {
		areas[i].RemotePath = path.Join(rs.ScratchDir, areas[i].ID.Dir())
		if err := os.MkdirAll(areas[i].LocalPath, 0o700); err != nil {
			return fmt.Errorf("create scratch area: %w", err)
		}
	}
	m, err := scratch.New(areas)
	if err != nil {
		return err
	}
	m.Logger = s.log.With("svc", "scratch")
	token, err := os.ReadFile(filepath.Join(p.local, shimsrv.TokenFile))
	if err != nil {
		return err
	}
	rl := relay.New(relay.Config{
		SessDir:    p.claude,
		Baseline:   env,
		LocalProgs: localProgs,
		Exec:       &rexec.Client{Opener: rs.Mux, Barrier: fsys, Logger: s.log.With("svc", "exec")},
		Scratch:    m,
		Logger:     s.log.With("svc", "relay"),
	})
	ln, err := shimsrv.Listen(p.sid)
	if err != nil {
		return err
	}
	sctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv := &shimsrv.Server{Token: token, UID: os.Getuid(), Handler: rl, Logger: s.log.With("svc", "shims")}
		if err := srv.Serve(sctx, ln); err != nil {
			s.log.Warn("shim server", "err", err)
		}
	}()
	s.onExit(func() {
		stop()
		<-done
		// Output of background commands may still flow; ending the
		// session ends it, as it ends the commands (execsvc).
		_ = rs.Close()
		rl.Wait()
	})
	return nil
}

// buildView builds the remote view and returns its mount namespace.
func (s *session) buildView(ctx context.Context, sd *sessionDir, p paths) (*os.File, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	binds := []view.Bind{
		{Source: "/proc", Target: "/proc"}, {Source: "/sys", Target: "/sys"}, {Source: "/dev", Target: "/dev"},
		{Source: filepath.Join(p.localHome, ".claude"), Target: path.Join(p.home, ".claude")},
		{Source: p.local, Target: p.claude},
		{Source: exe, Target: "/bin/sh"},
	}
	if isDir(managedDir) {
		binds = append(binds, view.Bind{Source: managedDir, Target: managedDir})
	}
	helper := exec.CommandContext(ctx, "/proc/self/exe")
	helper.Args[0] = RoleView
	helper.Stderr = sd.log
	ns, err := view.Build(ctx, view.Spec{Root: p.mnt, Binds: binds}, helper)
	if err != nil {
		return nil, err
	}
	s.onExit(func() { _ = ns.Close() })
	return ns, nil
}

// runClaude starts Claude through the launch stage and waits for it,
// forwarding SIGTERM and SIGHUP.
func (s *session) runClaude(env, args []string, ns *os.File, sigs <-chan os.Signal) (*os.ProcessState, error) {
	cmd := exec.CommandContext(context.Background(), "/proc/self/exe", append([]string{s.cfg.Claude}, args...)...)
	cmd.Args[0] = RoleLaunch
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{ns} // fd 3: teleswitch.Env
	cmd.SysProcAttr = view.LaunchAttr()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Claude: %w", err)
	}
	s.log.Info("claude started", "pid", cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case sig := <-sigs:
			if sig == unix.SIGTERM || sig == unix.SIGHUP {
				_ = cmd.Process.Signal(sig) // it may have exited meanwhile
			}
		case err := <-done:
			var ee *exec.ExitError
			if err != nil && !errors.As(err, &ee) {
				return nil, err
			}
			s.log.Info("claude exited", "status", cmd.ProcessState.String())
			if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Exited() && ws.ExitStatus() == teleswitch.ExitCode {
				s.log.Warn("claude may have failed to switch to the remote view; see its message above")
			}
			return cmd.ProcessState, nil
		}
	}
}
