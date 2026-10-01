package claudetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	calls = append(append(calls, "execve"), cloneCalls...)
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

// strace pads the pid column to a fixed width in some versions, so the
// pid is followed by one or more spaces.
var (
	callRE    = regexp.MustCompile(`^(\d+) +([a-z0-9_]+)\((.*)$`)
	resumedRE = regexp.MustCompile(`^(\d+) +<\.\.\. ([a-z0-9_]+) resumed>(.*)$`)
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

// cloneCalls create processes and threads; the trace follows them to
// tell Claude's own children apart.
var cloneCalls = []string{"clone", "clone3", "fork", "vfork"}

func isClone(name string) bool { return slices.Contains(cloneCalls, name) }

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

// ExecArgv returns the argv of an execve call, or nil if strace cut it
// short.
func (c Call) ExecArgv() []string {
	_, rest, ok := strings.Cut(c.Line, `", [`)
	if !ok {
		return nil
	}
	var argv []string
	for {
		s, n, ok := unquote(rest)
		if !ok {
			return nil
		}
		argv = append(argv, s)
		rest = rest[n:]
		switch {
		case strings.HasPrefix(rest, ", "):
			rest = rest[2:]
		case strings.HasPrefix(rest, "]"):
			return argv
		default:
			return nil // "..." after a truncated list
		}
	}
}

// unquote decodes the C string literal strace prints at the start of s,
// returning it and the length of the literal; a literal strace truncated
// ("..." follows) is not ok.
func unquote(s string) (string, int, bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", 0, false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			if strings.HasPrefix(s[i+1:], "...") {
				return "", 0, false
			}
			return b.String(), i + 1, true
		case '\\':
			i++
			if i == len(s) {
				return "", 0, false
			}
			if e, ok := cEscapes[s[i]]; ok {
				b.WriteByte(e)
				continue
			}
			if s[i] == 'x' && i+2 < len(s) {
				v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
				if err != nil {
					return "", 0, false
				}
				b.WriteByte(byte(v))
				i += 2
				continue
			}
			j := i
			for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			if j == i {
				return "", 0, false
			}
			v, _ := strconv.ParseUint(s[i:j], 8, 8)
			b.WriteByte(byte(v))
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, false
}

var cEscapes = map[byte]byte{
	'"': '"', '\\': '\\', 'n': '\n', 't': '\t', 'r': '\r', 'v': '\v', 'f': '\f', 'a': '\a', 'b': '\b',
}
