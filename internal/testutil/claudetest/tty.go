package claudetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Session is an interactive claude on a pseudo-terminal, for features
// that -p does not have: slash commands, key bindings, notifications.
type Session struct {
	t      testing.TB
	master *os.File
	cmd    *exec.Cmd
	done   chan struct{} // closed when the output reader is done

	stopOnce sync.Once

	mu  sync.Mutex
	out bytes.Buffer // guarded by mu
}

// TTYOptions configure an interactive claude.
type TTYOptions struct {
	Options
	// Config is merged into ~/.claude.json, which Start writes so that
	// Claude skips its first-run dialogs.
	Config map[string]any
}

// ttyCols and ttyRows are the terminal size: wide enough that no line the
// tests look for wraps.
const ttyCols, ttyRows = 200, 50

// Start runs claude interactively against api in o.Dir (fresh when empty),
// with the environment Run builds. The session is killed when t ends.
func Start(t testing.TB, claude string, api *API, o TTYOptions) *Session {
	t.Helper()
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	if o.Home == "" {
		o.Home = t.TempDir()
	}
	cfg := map[string]any{
		"hasCompletedOnboarding": true,
		"theme":                  "dark",
		"numStartups":            5,
		"customApiKeyResponses":  map[string]any{"approved": []string{apiKey}, "rejected": []string{}},
		"projects":               map[string]any{o.Dir: map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true}},
	}
	maps.Copy(cfg, o.Config)
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.Home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	master, slave, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), claude, append(append([]string{}, PinnedArgs...), o.Args...)...)
	cmd.Dir = o.Dir
	cmd.Env = append(append(baseEnv(api, o.Home), "TERM=xterm-256color"), o.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	err = cmd.Start()
	_ = slave.Close() // claude has its own copies
	if err != nil {
		_ = master.Close()
		t.Fatal(err)
	}
	s := &Session{t: t, master: master, cmd: cmd, done: make(chan struct{})}
	go s.read()
	t.Cleanup(s.Stop)
	return s
}

// read collects the output until the terminal is gone. Claude blocks on a
// full terminal unless someone reads.
func (s *Session) read() {
	defer close(s.done)
	buf := make([]byte, 64<<10)
	for {
		n, err := s.master.Read(buf)
		s.mu.Lock()
		s.out.Write(buf[:n])
		s.mu.Unlock()
		if err != nil {
			return // EIO once claude and its children closed the terminal
		}
	}
}

// Stop kills claude and everything it started, which is in its session
// and process group, and waits for it, so that a trace is complete.
func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		_ = unix.Kill(-s.cmd.Process.Pid, unix.SIGKILL)
		_ = s.cmd.Wait()
		_ = s.master.Close()
		<-s.done
	})
}

// Output returns what claude wrote to the terminal so far, escape
// sequences included.
func (s *Session) Output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

// Type sends keys as typed, after the session is ready for input.
func (s *Session) Type(keys string) {
	s.t.Helper()
	if _, err := io.WriteString(s.master, keys); err != nil {
		s.t.Fatalf("claudetest: type %q: %v", keys, err)
	}
}

// Command runs a slash command: it types it, waits until Claude shows it,
// and submits it. A command submitted at once may be taken for a prompt.
func (s *Session) Command(cmd string) {
	s.t.Helper()
	s.Type(cmd)
	s.WaitText(cmd)
	time.Sleep(500 * time.Millisecond) // the completion menu settles
	s.Type("\r")
}

// waitTimeout bounds each wait for Claude.
const waitTimeout = time.Minute

// WaitFor waits until cond holds for the output, polling it; what names
// the condition in the failure.
func (s *Session) WaitFor(what string, cond func(out string) bool) {
	s.t.Helper()
	if !s.Poll(waitTimeout, func() bool { return cond(s.Output()) }) {
		s.t.Fatalf("claudetest: timed out waiting for %s; screen:\n%s", what, Text(s.Output()))
	}
}

// WaitText waits until the screen shows text. Whitespace is ignored:
// Claude moves the cursor instead of writing some spaces.
func (s *Session) WaitText(text string) {
	s.t.Helper()
	s.WaitFor(fmt.Sprintf("%q on the screen", text), func(out string) bool {
		return strings.Contains(squash(Text(out)), squash(text))
	})
}

func squash(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// Ready waits until Claude shows its input prompt.
func (s *Session) Ready() {
	s.t.Helper()
	s.WaitFor("the input prompt", func(out string) bool { return strings.Contains(out, "❯") })
	time.Sleep(time.Second) // input typed earlier than this can be dropped
}

// Poll calls cond until it holds or d passes, and reports whether it held.
func (s *Session) Poll(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); ; {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		select {
		case <-s.done:
			return cond()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Text returns out without terminal escape sequences, roughly as shown.
func Text(out string) string {
	var b strings.Builder
	for i := 0; i < len(out); i++ {
		c := out[i]
		if c != 0x1b {
			if c >= 0x20 || c == '\n' {
				b.WriteByte(c)
			}
			continue
		}
		i = skipEscape(out, i)
	}
	return b.String()
}

// skipEscape returns the index of the last byte of the escape sequence
// starting at out[i].
func skipEscape(out string, i int) int {
	if i+1 >= len(out) {
		return i
	}
	switch out[i+1] {
	case '[': // CSI: parameters, then a final byte in 0x40-0x7e
		for j := i + 2; j < len(out); j++ {
			if out[j] >= 0x40 && out[j] <= 0x7e {
				return j
			}
		}
		return len(out) - 1
	case ']', 'P', '_': // OSC, DCS, APC: up to BEL or ST
		for j := i + 2; j < len(out); j++ {
			if out[j] == 0x07 {
				return j
			}
			if out[j] == 0x1b && j+1 < len(out) && out[j+1] == '\\' {
				return j + 1
			}
		}
		return len(out) - 1
	default:
		return i + 1
	}
}

// openPTY allocates a pseudo-terminal of ttyCols x ttyRows.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open pty master: %w", err)
	}
	fd := int(master.Fd())
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err == nil {
		err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0)
	}
	if err == nil {
		err = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: ttyRows, Col: ttyCols})
	}
	if err == nil {
		slave, err = os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|unix.O_NOCTTY, 0)
	}
	if err != nil {
		_ = master.Close()
		return nil, nil, errors.Join(errors.New("set up pty"), err)
	}
	return master, slave, nil
}
