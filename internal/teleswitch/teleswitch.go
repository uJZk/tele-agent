// Package teleswitch holds the preload library that moves the Claude
// process into the remote view before its main runs (docs/filesystem.md
// "teleswitch"), and the conventions for starting a process with it.
//
// The library is C (csrc/teleswitch.c), built by make for the target
// architecture and embedded from lib/. A binary built without make lacks
// it; Library then fails, and tele refuses to start Claude.
package teleswitch

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strconv"
)

// Shared with csrc/teleswitch.c; TestConstantsMatchC checks that both agree.
const (
	// EnvFD names the descriptor of the remote view's mount namespace.
	EnvFD = "TELE_SWITCH_FD"
	// EnvDir names the working directory in the remote view.
	EnvDir = "TELE_SWITCH_DIR"
	// EnvPrefix starts every variable meant for teleswitch alone; they
	// are removed from the environment with LD_PRELOAD.
	EnvPrefix = "TELE_SWITCH_"
	// EnvCheck names a path that exists only in the remote view: the
	// session directory (shim.SessionEnv). Finding it confirms the switch.
	EnvCheck = "TELE_SESSION"
	// ExitCode is the exit status of a process whose switch failed.
	ExitCode = 121
	// DiagFD receives the one-line diagnostic of a failed switch.
	DiagFD = 2
)

//go:embed lib
var libs embed.FS

// ErrMissing reports a tele binary built without the library.
var ErrMissing = errors.New("teleswitch: this tele binary was built without the teleswitch library; build it with make")

// FileName is the name of the library for architecture goarch.
func FileName(goarch string) string {
	return "teleswitch-" + goarch + ".so"
}

// Library returns the library for the architecture tele runs on.
func Library() ([]byte, error) {
	b, err := fs.ReadFile(libs, "lib/"+FileName(runtime.GOARCH))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrMissing
	}
	if err != nil {
		return nil, fmt.Errorf("teleswitch: %w", err)
	}
	return b, nil
}

// Env returns the variables that make a process switch: the library at
// lib preloaded, the mount namespace at descriptor fd, and dir as the
// working directory there. The process's environment must also hold
// EnvCheck.
func Env(lib string, fd int, dir string) []string {
	return []string{
		"LD_PRELOAD=" + lib,
		EnvFD + "=" + strconv.Itoa(fd),
		EnvDir + "=" + dir,
	}
}
