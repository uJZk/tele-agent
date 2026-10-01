package sigexit

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/testutil/helperproc"
)

// raisedExit is the status the raise helper exits with when Raise returns.
const raisedExit = 7

func TestMain(m *testing.M) {
	helperproc.Register("raise", raiseHelper)
	helperproc.Dispatch()
	os.Exit(m.Run())
}

// raiseHelper raises the signal named by args[0] in the state a shim is in
// by then: the Go runtime's handlers installed for it and reset again.
func raiseHelper(args []string) int {
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	sig := unix.Signal(n)
	signal.Notify(make(chan os.Signal, 1), sig)
	signal.Reset(sig)
	Raise(sig)
	return raisedExit
}

func TestRaise(t *testing.T) {
	tests := []struct {
		sig  unix.Signal
		dies bool
	}{
		{unix.SIGTERM, true},
		{unix.SIGINT, true},
		{unix.SIGHUP, true},
		{unix.SIGPIPE, true},
		{unix.SIGKILL, true},
		// After signal.Reset the Go runtime ignores SIGPIPE, SIGUSR1 and
		// real-time signals, turns SIGQUIT into a goroutine dump and
		// SIGSEGV into a crash report, both with exit status 2.
		{unix.SIGUSR1, true},
		{unix.SIGQUIT, true},
		{unix.SIGSEGV, true},
		{unix.Signal(40), true}, // a real-time signal
		{unix.SIGWINCH, false},
		{unix.SIGCHLD, false},
		{unix.SIGTSTP, false},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(int(tt.sig)), func(t *testing.T) {
			cmd := helperproc.Command(t, "raise", strconv.Itoa(int(tt.sig)))
			cmd.Dir = t.TempDir() // a core dump would land here
			err := cmd.Run()
			var ee *exec.ExitError
			if err != nil && !errors.As(err, &ee) {
				t.Fatal(err)
			}
			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok {
				t.Fatalf("wait status %T", cmd.ProcessState.Sys())
			}
			if !tt.dies {
				if !ws.Exited() || ws.ExitStatus() != raisedExit {
					t.Fatalf("Raise(%v) did not return: %v", tt.sig, cmd.ProcessState)
				}
				return
			}
			if !ws.Signaled() || ws.Signal() != tt.sig {
				t.Fatalf("helper ended with %v, want killed by %v", cmd.ProcessState, tt.sig)
			}
			if ws.CoreDump() {
				t.Errorf("helper dumped core on %v", tt.sig)
			}
		})
	}
}

func TestTerminatesByDefault(t *testing.T) {
	for _, sig := range []unix.Signal{0, 65, -1} {
		if terminatesByDefault(sig) {
			t.Errorf("terminatesByDefault(%d) = true", sig)
		}
	}
}
