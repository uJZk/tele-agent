package claudetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Trace is a claude wrapper that records system calls with strace.
type Trace struct {
	// Claude is the wrapper to pass to Run in place of claude.
	Claude string
	out    string
}

// NewTrace returns a wrapper that runs claude under strace -f, recording
// the given system calls and the clones needed to tell Claude's own
// children from theirs. It skips t when strace is not installed.
func NewTrace(t testing.TB, claude string, calls ...string) *Trace {
	t.Helper()
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skipf("strace not found: %v", err)
	}
	dir := t.TempDir()
	tr := &Trace{out: filepath.Join(dir, "strace.txt")}
	calls = append(calls, "execve", "clone", "clone3", "fork", "vfork")
	tr.Claude = WriteScript(t, dir, "claude-strace",
		`exec `+strace+` -f -s 4096 -o '`+tr.out+`' -e trace=`+strings.Join(calls, ",")+` '`+claude+`' "$@"`+"\n")
	return tr
}

// Call is one recorded system call.
type Call struct {
	PID  int
	Name string
	// Line is the call as strace printed it, arguments and result.
	Line string
	// Own is set for calls of the Claude process itself (any thread),
	// Child for calls of processes Claude started directly.
	Own, Child bool
}

var (
	callRE    = regexp.MustCompile(`^(\d+) ([a-z0-9_]+)\((.*)$`)
	resumedRE = regexp.MustCompile(`^(\d+) <\.\.\. ([a-z0-9_]+) resumed>(.*)$`)
	resultRE  = regexp.MustCompile(`= (\d+)$`)
)

// Calls parses the trace, once claude exited.
func (tr *Trace) Calls(t testing.TB) []Call {
	t.Helper()
	b, err := os.ReadFile(tr.out)
	if err != nil {
		t.Fatalf("read strace output: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	parent := map[int]int{}
	thread := map[int]bool{}
	pendingThread := map[int]bool{}
	var calls []Call
	root := 0
	for _, l := range lines {
		name, pid, rest, resumed := parseLine(l)
		if name == "" {
			continue
		}
		if root == 0 {
			root = pid
		}
		if isClone(name) {
			thr := strings.Contains(rest, "CLONE_THREAD")
			if m := resultRE.FindStringSubmatch(l); m != nil {
				child, _ := strconv.Atoi(m[1]) // matched digits
				parent[child] = pid
				thread[child] = thr || (resumed && pendingThread[pid])
				delete(pendingThread, pid)
			} else if !resumed {
				pendingThread[pid] = thr
			}
			continue
		}
		if resumed {
			// Complete the unfinished half with the result.
			for i := len(calls) - 1; i >= 0; i-- {
				if calls[i].PID == pid && calls[i].Name == name {
					calls[i].Line += rest
					break
				}
			}
			continue
		}
		calls = append(calls, Call{PID: pid, Name: name, Line: rest})
	}
	proc := func(tid int) int {
		for thread[tid] {
			tid = parent[tid]
		}
		return tid
	}
	for i := range calls {
		p := proc(calls[i].PID)
		calls[i].Own = p == root
		calls[i].Child = !calls[i].Own && proc(parent[p]) == root
	}
	return calls
}

func parseLine(l string) (name string, pid int, rest string, resumed bool) {
	if m := resumedRE.FindStringSubmatch(l); m != nil {
		pid, _ = strconv.Atoi(m[1]) // matched digits
		return m[2], pid, m[3], true
	}
	if m := callRE.FindStringSubmatch(l); m != nil {
		pid, _ = strconv.Atoi(m[1]) // matched digits
		return m[2], pid, m[3], false
	}
	return "", 0, "", false
}

func isClone(name string) bool {
	switch name {
	case "clone", "clone3", "fork", "vfork":
		return true
	}
	return false
}

// Failed reports whether the call returned an error.
func (c Call) Failed() bool {
	return strings.Contains(c.Line, "= -1 ")
}

// ExecPath returns the program path of an execve call.
func (c Call) ExecPath() string {
	if !strings.HasPrefix(c.Line, `"`) {
		return ""
	}
	p, _, _ := strings.Cut(c.Line[1:], `"`)
	return p
}
