// Package sigexit ends the process the way another process ended when a
// signal killed it: the shim reproduces the remote command's end, and tele
// reproduces Claude's (docs/exec.md "shim 与会话主进程").
package sigexit

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Raise terminates the process with sig. It returns only if sig's
// default action does not terminate a process (e.g. SIGWINCH, SIGCHLD) or
// would merely stop it.
//
// The Go runtime's own handler cannot be relied on after signal.Reset: it
// ignores SIGUSR1/SIGUSR2, turns SIGQUIT into a goroutine dump and exit
// status 2, and treats SIGSEGV as a crash. So the kernel disposition is set
// to SIG_DFL directly, the signal is unblocked on this thread, and sent to
// this thread, where the default action takes effect before tgkill returns.
func Raise(sig unix.Signal) {
	if !terminatesByDefault(sig) {
		return
	}
	// The process that died dumped its own core, if any; this one must
	// not drop a core of itself into the user's working directory.
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{})

	runtime.LockOSThread() // never unlocked: the process ends on this thread
	setDefaultAction(sig)
	var set unix.Sigset_t
	set.Val[(sig-1)/64] |= 1 << ((uint(sig) - 1) % 64)
	_ = unix.PthreadSigmask(unix.SIG_UNBLOCK, &set, nil) // EINVAL impossible for SIG_UNBLOCK
	_ = unix.Tgkill(unix.Getpid(), unix.Gettid(), sig)   // cannot fail for our own thread
}

// terminatesByDefault reports whether sig's default action ends the
// process. The stop signals would suspend the shim forever instead.
func terminatesByDefault(sig unix.Signal) bool {
	switch sig {
	case unix.SIGCHLD, unix.SIGCONT, unix.SIGURG, unix.SIGWINCH, // ignored
		unix.SIGSTOP, unix.SIGTSTP, unix.SIGTTIN, unix.SIGTTOU: // stop
		return false
	default:
		return sig >= 1 && sig <= 64
	}
}

// setDefaultAction installs SIG_DFL for sig with rt_sigaction(2). x/sys has
// no wrapper. An all-zero struct sigaction means SIG_DFL, no flags, empty
// mask whatever the architecture's field order; the buffer is larger than
// any architecture's struct. The last argument is sizeof(kernel sigset_t)
// for 64 signals. Failure (SIGKILL, SIGSTOP) leaves raising to do the rest.
func setDefaultAction(sig unix.Signal) {
	var act [8]uint64
	//nolint:gosec // G103: the pointer is to a local buffer that outlives the raw syscall
	_, _, _ = unix.RawSyscall6(unix.SYS_RT_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&act)), 0, 8, 0, 0)
}
