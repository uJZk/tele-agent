// Package helperproc lets a test binary re-execute itself as a helper
// process, for code that must run in a fresh process: new namespaces,
// shims invoked under another argv[0], signal delivery.
//
// A test package registers its helpers in TestMain before m.Run:
//
//	func TestMain(m *testing.M) {
//		helperproc.Register("mount-fuse", mountFUSEHelper)
//		helperproc.Dispatch()
//		os.Exit(m.Run())
//	}
//
// and starts one with helperproc.Command.
package helperproc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

const envName = "TELE_TEST_HELPER"

var helpers = map[string]func(args []string) int{}

// Register makes fn runnable as helper name. It must be called before
// Dispatch, from TestMain.
func Register(name string, fn func(args []string) int) {
	if _, dup := helpers[name]; dup {
		panic(fmt.Sprintf("helperproc: duplicate helper %q", name))
	}
	helpers[name] = fn
}

// Dispatch runs the requested helper and exits, if this process was started
// by Command; otherwise it returns so that the tests run.
func Dispatch() {
	name := os.Getenv(envName)
	if name == "" {
		return
	}
	fn, ok := helpers[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "helperproc: unknown helper %q\n", name)
		os.Exit(2)
	}
	os.Exit(fn(os.Args[1:]))
}

// Exec returns a command that runs helper name with args in a new process
// of the current executable, for helpers that start helpers themselves.
// Unlike Command it needs no test; the caller owns the process.
func Exec(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	if _, ok := helpers[name]; !ok {
		return nil, fmt.Errorf("helperproc: helper %q not registered", name)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("helperproc: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(os.Environ(), envName+"="+name)
	cmd.Stderr = os.Stderr
	return cmd, nil
}

// Command returns a command that runs helper name with args in a new
// process of the current test binary. The process is killed when the test
// ends. The caller may adjust the command (SysProcAttr, ExtraFiles, Env)
// before starting it; Env must keep the entries it has.
func Command(t testing.TB, name string, args ...string) *exec.Cmd {
	t.Helper()
	if _, ok := helpers[name]; !ok {
		t.Fatalf("helperproc: helper %q not registered", name)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("helperproc: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), exe, args...)
	cmd.Env = append(os.Environ(), envName+"="+name)
	cmd.Stderr = os.Stderr
	return cmd
}
