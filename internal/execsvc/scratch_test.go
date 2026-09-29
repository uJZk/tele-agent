package execsvc

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// startBlocked starts a command that waits until release is called.
func startBlocked(t *testing.T, h *harness, start *proto.ExecStart) (s *stream, release func()) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	start.Argv = sh(`read x < "$1"; `+start.Argv[0], fifo)
	s = h.start(t, start)
	s.send(&proto.ExecFrame{Op: proto.ExecStdinEOF})
	s.untilStarted(&outcome{})
	return s, func() {
		t.Helper()
		if err := os.WriteFile(fifo, []byte("go\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUploadsNotReportedByOtherCommands(t *testing.T) {
	h := newHarness(t, nil)
	tmp := filepath.Join(h.scratch, "tmp")
	a, release := startBlocked(t, h, &proto.ExecStart{Argv: []string{"echo mine > tmp/a-out"}, Dir: h.scratch})

	// Another command uploads files while A runs: a new one, a replaced
	// one it then changes itself, and a removal.
	if err := os.WriteFile(filepath.Join(tmp, "doomed"), []byte("d"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := run(t, h, &proto.ExecStart{Argv: sh("echo changed > tmp/changed-after"), Dir: h.scratch, Scratch: []proto.ScratchFile{
		{Area: proto.ScratchTmp, Path: "tasks/t1.output", Data: []byte("partial output")},
		{Area: proto.ScratchTmp, Path: "changed-after", Data: []byte("uploaded")},
		{Area: proto.ScratchTmp, Path: "doomed", Deleted: true},
	}})
	want := []proto.ScratchFile{{Area: proto.ScratchTmp, Path: "changed-after", Mode: 0o644, Data: []byte("changed\n")}}
	if len(o.exit.Scratch) == 1 {
		o.exit.Scratch[0].Mode = 0o644 // the shell's umask
	}
	if fmt.Sprint(o.exit.Scratch) != fmt.Sprint(want) {
		t.Fatalf("uploading command reported %+v, want %+v", o.exit.Scratch, want)
	}

	release()
	oa := a.collect()
	var got []string
	for _, f := range oa.exit.Scratch {
		got = append(got, f.Path)
	}
	// A reports its own file and the change made by the other command,
	// but neither the uploads nor the uploaded removal.
	if strings.Join(got, " ") != "a-out changed-after" {
		t.Fatalf("A reported %v, want [a-out changed-after]", got)
	}
}

func TestScratchReturnBounded(t *testing.T) {
	h := newHarness(t, nil)
	tmp := filepath.Join(h.scratch, "tmp")
	long := strings.Repeat("n", 230)
	const n = maxReturnEntries + 500
	for i := range n {
		if err := os.WriteFile(filepath.Join(tmp, fmt.Sprintf("%s%05d", long, i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The removals alone would exceed the frame; the small file Claude
	// waits for must still come back.
	o := run(t, h, &proto.ExecStart{Argv: sh(`find tmp -type f -delete && pwd > tmp/cwd`), Dir: h.scratch})
	if o.exit.Err != nil || o.exit.Code != 0 {
		t.Fatalf("exit %+v", o.exit)
	}
	var deleted int
	var cwd bool
	for _, f := range o.exit.Scratch {
		switch {
		case f.Deleted:
			deleted++
		case f.Path == "cwd":
			cwd = true
		}
	}
	if !cwd || len(o.exit.Scratch) != maxReturnEntries || deleted != maxReturnEntries-1 {
		t.Fatalf("returned %d files, %d removals, cwd %v", len(o.exit.Scratch), deleted, cwd)
	}
	b, err := proto.Marshal(&proto.ExecFrame{Op: proto.ExecExit, Exit: o.exit})
	if err != nil || len(b) > proto.MaxDataFrame {
		t.Fatalf("exit frame %d bytes, %v", len(b), err)
	}
}

func TestEntryOverhead(t *testing.T) {
	for _, pathLen := range []int{1, 23, 24, 255, 256, 65535, 65536} {
		for _, dataLen := range []int{0, 23, 24, 255, 256, 65535, 65536, proto.ScratchFileMax} {
			f := proto.ScratchFile{
				Area: proto.ScratchSessionEnv,
				Path: strings.Repeat("p", pathLen),
				Mode: 0o777,
				Data: make([]byte, dataLen),
			}
			for _, deleted := range []bool{false, true} {
				f.Deleted = deleted
				b, err := proto.Marshal(&f)
				if err != nil {
					t.Fatal(err)
				}
				if over := len(b) - pathLen - dataLen; over > entryOverhead {
					t.Errorf("path %d data %d: %d bytes of overhead, entryOverhead is %d", pathLen, dataLen, over, entryOverhead)
				}
			}
		}
	}
}

func TestScratchAreaRecreated(t *testing.T) {
	h := newHarness(t, nil)
	o := run(t, h, &proto.ExecStart{Argv: sh(`rm -rf tmp session-env`), Dir: h.scratch})
	if o.exit.Code != 0 {
		t.Fatalf("exit %+v", o.exit)
	}
	o = run(t, h, &proto.ExecStart{
		Argv:    sh(`cat tmp/in; echo x > tmp/out`),
		Dir:     h.scratch,
		Scratch: []proto.ScratchFile{{Area: proto.ScratchTmp, Path: "in", Data: []byte("uploaded\n")}},
	})
	if o.exit.Err != nil || o.stdout.String() != "uploaded\n" {
		t.Fatalf("exit %+v, stdout %q", o.exit, o.stdout.String())
	}
	if len(o.exit.Scratch) != 1 || o.exit.Scratch[0].Path != "out" {
		t.Fatalf("scratch %+v, want out", o.exit.Scratch)
	}
	// A command that removes and recreates an area still reports what it
	// writes there.
	o = run(t, h, &proto.ExecStart{Argv: sh(`rm -rf tmp && mkdir tmp && echo y > tmp/out2`), Dir: h.scratch})
	var got []string
	for _, f := range o.exit.Scratch {
		got = append(got, fmt.Sprintf("%s deleted=%v", f.Path, f.Deleted))
	}
	if strings.Join(got, ", ") != "in deleted=true, out deleted=true, out2 deleted=false" {
		t.Fatalf("scratch %v", got)
	}
}

func TestScratchAreaSymlinkNotFollowed(t *testing.T) {
	h := newHarness(t, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := run(t, h, &proto.ExecStart{Argv: sh(`rm -rf tmp && ln -s "$1" tmp`, outside), Dir: h.scratch})
	for _, f := range o.exit.Scratch {
		if f.Path == "secret" {
			t.Fatalf("file behind a symlinked area returned: %+v", f)
		}
	}
}

func TestUploadFailureDoesNotFailCommand(t *testing.T) {
	h := newHarness(t, nil)
	if err := os.MkdirAll(filepath.Join(h.scratch, "tmp", "dir", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	o := run(t, h, &proto.ExecStart{Argv: sh(`cat tmp/ok`), Dir: h.scratch, Scratch: []proto.ScratchFile{
		{Area: proto.ScratchTmp, Path: "dir", Data: []byte("a file where a directory is")},
		{Area: proto.ScratchTmp, Path: "ok", Data: []byte("ok\n")},
	}})
	if o.exit.Err != nil || o.exit.Code != 0 || o.stdout.String() != "ok\n" {
		t.Fatalf("exit %+v, stdout %q", o.exit, o.stdout.String())
	}
}

// unprivileged runs fn on a thread whose filesystem uid is nobody's when
// the test runs as root, so that permission bits apply to it. The thread
// ends with fn (docs/coding-standards.md "系统调用、命名空间与进程").
func unprivileged(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread exits with its fsuid
		if os.Geteuid() == 0 {
			// setfsuid clears the thread's DAC override capabilities; it
			// reports no errors, so read the value back.
			_ = unix.Setfsuid(65534)
			if cur, _ := unix.SetfsuidRetUid(-1); cur != 65534 {
				done <- fmt.Errorf("fsuid is %d after setfsuid", cur)
				return
			}
		}
		fn()
		done <- nil
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// chmodAll sets mode on each path.
func chmodAll(t *testing.T, mode os.FileMode, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnreadableDirectoryNotReportedDeleted(t *testing.T) {
	base := t.TempDir()
	h := newHarness(t, func(c *Config) { c.ScratchDir = filepath.Join(base, "scratch") })
	tmp := filepath.Join(h.scratch, "tmp")
	sub := filepath.Join(tmp, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(tmp, "top"), filepath.Join(sub, "inner")} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dirs := []string{filepath.Dir(base), base, h.scratch}
	for _, a := range proto.ScratchAreas {
		dirs = append(dirs, filepath.Join(h.scratch, a.Dir()))
	}
	chmodAll(t, 0o755, dirs...)

	roots := h.svc.openAreas()
	before := scan(roots)
	roots.close()
	chmodAll(t, 0, sub)
	defer chmodAll(t, 0o755, sub)
	var changed, deleted []scratchKey
	var covered bool
	unprivileged(t, func() {
		roots := h.svc.openAreas()
		defer roots.close()
		after := scan(roots)
		covered = after.covers(scratchKey{proto.ScratchTmp, "sub/inner"})
		changed, deleted = h.svc.diff(before, after, 0)
	})
	if !covered || len(changed) != 0 || len(deleted) != 0 {
		t.Fatalf("unreadable directory: covered %v, changed %v, deleted %v", covered, changed, deleted)
	}
}

func TestScratchUploadDirectory(t *testing.T) {
	// A SessionStart hook appends to CLAUDE_ENV_FILE in a directory that
	// holds no file yet (docs/claude-code.md "scratch 文件").
	h := newHarness(t, nil)
	envFile := filepath.Join(h.scratch, "session-env", "sid", "sessionstart-hook-0.sh")
	o := run(t, h, &proto.ExecStart{
		Argv:    sh(`echo 'export A=1' >> "$1"`, envFile),
		Scratch: []proto.ScratchFile{{Area: proto.ScratchSessionEnv, Path: "sid", Dir: true}},
	})
	if o.exit.Code != 0 {
		t.Fatalf("exit %+v, stderr %q", o.exit, o.stderr.String())
	}
	if len(o.exit.Scratch) != 1 || o.exit.Scratch[0].Path != "sid/sessionstart-hook-0.sh" || string(o.exit.Scratch[0].Data) != "export A=1\n" {
		t.Fatalf("returned %+v, want the hook's file", o.exit.Scratch)
	}
}
