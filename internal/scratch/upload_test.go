package scratch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func tmpMapper(t *testing.T) (*Mapper, string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := New([]Area{{ID: proto.ScratchTmp, ClaudePath: "/c/tmp", LocalPath: tmp, RemotePath: "/r/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	return m, tmp
}

func paths(files []proto.ScratchFile) string {
	var p []string
	for _, f := range files {
		p = append(p, fmt.Sprintf("%s:%v", f.Path, f.Deleted))
	}
	return strings.Join(p, " ")
}

func TestUploadCommitAndRollback(t *testing.T) {
	m, tmp := tmpMapper(t)
	writeLocal(t, filepath.Join(tmp, "f"), "x")
	writeLocal(t, filepath.Join(tmp, "gone"), "y")
	upload(m)
	if err := os.Remove(filepath.Join(tmp, "gone")); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, filepath.Join(tmp, "f"), "changed")

	u1 := m.Uploads(proto.MaxDataFrame)
	if got := paths(u1.Files); got != "gone:true f:false" {
		t.Fatalf("first Uploads = %s", got)
	}
	// Files on their way to the target are not offered twice.
	if u := m.Uploads(proto.MaxDataFrame); len(u.Files) != 0 {
		t.Fatalf("Uploads while pending = %s", paths(u.Files))
	} else {
		u.Commit()
	}
	// A command that did not start leaves them to be offered again.
	u1.Rollback()
	u1.Commit() // no effect after Rollback
	u2 := m.Uploads(proto.MaxDataFrame)
	if got := paths(u2.Files); got != "gone:true f:false" {
		t.Fatalf("Uploads after Rollback = %s", got)
	}
	u2.Commit()
	if files := upload(m); len(files) != 0 {
		t.Fatalf("Uploads after Commit = %s", paths(files))
	}
}

func TestUploadBudget(t *testing.T) {
	m, tmp := tmpMapper(t)
	for i := range 20 {
		writeLocal(t, filepath.Join(tmp, fmt.Sprintf("f%02d", i)), strings.Repeat("x", 1000))
	}
	const budget = 5 * (1000 + 3 + entryOverhead)
	u := m.Uploads(budget)
	b, err := proto.Marshal(u.Files)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Files) != 5 || len(b) > budget {
		t.Fatalf("Uploads(%d) = %d files, %d bytes encoded", budget, len(u.Files), len(b))
	}
	u.Commit()
	if u := m.Uploads(0); len(u.Files) != 0 {
		t.Fatalf("Uploads(0) = %d files", len(u.Files))
	} else {
		u.Rollback()
	}
	if files := upload(m); len(files) != 15 {
		t.Fatalf("the rest: %d files, want 15", len(files))
	}
}

func TestUploadEntryLimit(t *testing.T) {
	m, tmp := tmpMapper(t)
	const n = maxUploadEntries + 10
	for i := range n {
		if err := os.WriteFile(filepath.Join(tmp, fmt.Sprint(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if files := upload(m); len(files) != maxUploadEntries {
		t.Fatalf("first Uploads = %d files, want %d", len(files), maxUploadEntries)
	}
	if files := upload(m); len(files) != 10 {
		t.Fatalf("second Uploads = %d files, want 10", len(files))
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

func TestSharedAreasOnlyOwnFiles(t *testing.T) {
	local := t.TempDir()
	env := filepath.Join(local, "session-env")
	snap := filepath.Join(local, "snap")
	m, err := New([]Area{
		{ID: proto.ScratchSnapshots, ClaudePath: "/h/.claude/shell-snapshots", LocalPath: snap, RemotePath: "/r/s"},
		{ID: proto.ScratchSessionEnv, ClaudePath: "/h/.claude/session-env", LocalPath: env, RemotePath: "/r/e"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Other local sessions write their own files there.
	writeLocal(t, filepath.Join(env, "other-session", "hook-1.sh"), "export GITHUB_TOKEN=secret\n")
	writeLocal(t, filepath.Join(snap, "snapshot-bash-other.sh"), "other")
	if files := upload(m); len(files) != 0 {
		t.Fatalf("Uploads of other sessions' files = %s", paths(files))
	}
	// A file this session's target produced is synced both ways.
	if err := m.Apply([]proto.ScratchFile{{Area: proto.ScratchSessionEnv, Path: "mine/hook-1.sh", Data: []byte("export A=1\n")}}); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, filepath.Join(env, "mine", "hook-1.sh"), "export A=2\n")
	if got := paths(upload(m)); got != "mine/hook-1.sh:false" {
		t.Fatalf("Uploads after a local change = %s", got)
	}
	// So is an entry this session's commands name, from then on.
	writeLocal(t, filepath.Join(env, "claimed", "hook-1.sh"), "export B=1\n")
	for _, s := range []string{
		"ls /h/.claude/session-env /h/.claude/session-env/ /h/.claude/session-env/..",
		"CLAUDE_ENV_FILE=/h/.claude/session-env/claimed/hook-2.sh",
	} {
		m.Rewrite(s)
	}
	if got := paths(upload(m)); got != "claimed/hook-1.sh:false" {
		t.Fatalf("Uploads of a claimed entry = %s", got)
	}
	if err := os.RemoveAll(filepath.Join(env, "claimed")); err != nil {
		t.Fatal(err)
	}
	if got := paths(upload(m)); got != "claimed/hook-1.sh:true" {
		t.Fatalf("Uploads after removing a claimed entry = %s", got)
	}
}

func TestHold(t *testing.T) {
	m, tmp := tmpMapper(t)
	name := filepath.Join(tmp, "tasks", "t1.output")
	writeLocal(t, name, "")
	out, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	release, err := m.Hold(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteString("line1\n"); err != nil {
		t.Fatal(err)
	}
	if files := upload(m); len(files) != 0 {
		t.Fatalf("Uploads of a held file = %s", paths(files))
	}
	// A copy reported by the target does not replace the file being
	// written.
	if err := m.Apply([]proto.ScratchFile{{Area: proto.ScratchTmp, Path: "tasks/t1.output", Data: []byte("stale")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteString("line2\n"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(name); err != nil || string(data) != "line1\nline2\n" {
		t.Fatalf("held file = %q, %v", data, err)
	}
	release()
	release()
	if got := paths(upload(m)); got != "tasks/t1.output:false" {
		t.Fatalf("Uploads after release = %s", got)
	}

	// Files that are not regular files are ignored.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	release, err = m.Hold(w)
	if err != nil {
		t.Fatal(err)
	}
	release()
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

func TestUnreadableDirectoryNotUploadedAsRemoved(t *testing.T) {
	base := t.TempDir()
	tmp := filepath.Join(base, "tmp")
	sub := filepath.Join(tmp, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(tmp, "top"), filepath.Join(sub, "inner")} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{filepath.Dir(base), base, tmp} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New([]Area{{ID: proto.ScratchTmp, ClaudePath: "/c/tmp", LocalPath: tmp, RemotePath: "/r/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(sub, 0o755) }()
	var (
		files   []proto.ScratchFile
		readErr error
	)
	unprivileged(t, func() {
		_, readErr = os.ReadDir(sub)
		files = upload(m)
	})
	if !errors.Is(readErr, fs.ErrPermission) {
		t.Fatalf("reading the directory: %v, want a permission error", readErr)
	}
	if len(files) != 0 {
		t.Fatalf("Uploads with an unreadable directory = %s", paths(files))
	}
	// Once readable again, a real removal is uploaded.
	if err := os.Chmod(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sub, "inner")); err != nil {
		t.Fatal(err)
	}
	if got := paths(upload(m)); got != "sub/inner:true" {
		t.Fatalf("Uploads after the removal = %s", got)
	}
}
