package telefs

import (
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// replaceByRename replaces backing file rel the way editors and sed -i do:
// the old file moves to rel+".old" and a new one takes its name.
func replaceByRename(t *testing.T, h *harness, rel, data string) {
	t.Helper()
	writeFile(t, h.b(rel+".tmp"), data)
	if err := os.Rename(h.b(rel), h.b(rel+".old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(h.b(rel+".tmp"), h.b(rel)); err != nil {
		t.Fatal(err)
	}
}

func requireUserXattr(t *testing.T, p string) {
	t.Helper()
	if err := unix.Setxattr(p, "user.probe", []byte("1"), 0); errors.Is(err, unix.ENOTSUP) {
		t.Skip("backing file system has no user xattrs")
	}
}

// TestOpenFileAfterRemoteReplace covers system calls on an open descriptor
// after a remote tool replaced the file by rename. The kernel passes no
// file handle for fchmod, futimens or the f*xattr calls; they must still
// act on the open file, not on the new one at its path.
func TestOpenFileAfterRemoteReplace(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	writeFile(t, h.b("f"), "old")
	requireUserXattr(t, h.b("f"))
	h.sync()
	fh, err := os.OpenFile(h.m("f"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	fd := int(fh.Fd())
	replaceByRename(t, h, "f", "new file")
	h.sync()

	if err := unix.Fchmod(fd, 0o600); err != nil {
		t.Fatalf("fchmod: %v", err)
	}
	mt := []unix.Timespec{unix.NsecToTimespec(1_500_000_000_000_000_000), unix.NsecToTimespec(1_500_000_000_000_000_000)}
	if err := unix.UtimesNanoAt(fd, "", mt, unix.AT_EMPTY_PATH); err != nil {
		t.Fatalf("futimens: %v", err)
	}
	if err := unix.Fsetxattr(fd, "user.k", []byte("v"), 0); err != nil {
		t.Fatalf("fsetxattr: %v", err)
	}
	buf := make([]byte, 16)
	if n, err := unix.Fgetxattr(fd, "user.k", buf); err != nil || string(buf[:n]) != "v" {
		t.Fatalf("fgetxattr = %q, %v", buf[:n], err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Size != 3 || st.Ino != lstat(t, h.b("f.old")).Ino {
		t.Fatalf("fstat: size %d ino %d, %v", st.Size, st.Ino, err)
	}

	old, cur := lstat(t, h.b("f.old")), lstat(t, h.b("f"))
	if old.Mode&0o7777 != 0o600 || old.Mtim.Nano() != 1_500_000_000_000_000_000 {
		t.Errorf("open file: mode %o mtime %d", old.Mode&0o7777, old.Mtim.Nano())
	}
	if cur.Mode&0o7777 != 0o644 || cur.Mtim.Nano() == 1_500_000_000_000_000_000 {
		t.Errorf("file now at the path changed: mode %o mtime %d", cur.Mode&0o7777, cur.Mtim.Nano())
	}
	if _, err := unix.Getxattr(h.b("f"), "user.k", buf); !errors.Is(err, unix.ENODATA) {
		t.Errorf("xattr reached the file now at the path: %v", err)
	}
}

// TestOpenFileAfterUnlink checks that an open file keeps working after it
// was unlinked through the mount, as on a local file system.
func TestOpenFileAfterUnlink(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	writeFile(t, h.m("u"), "data")
	fh, err := os.OpenFile(h.m("u"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	if err := os.Remove(h.m("u")); err != nil {
		t.Fatal(err)
	}
	fd := int(fh.Fd())
	if err := unix.Fchmod(fd, 0o600); err != nil {
		t.Fatalf("fchmod after unlink: %v", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatalf("fstat after unlink: %v", err)
	}
	if st.Nlink != 0 || st.Size != 4 || st.Mode&0o7777 != 0o600 {
		t.Fatalf("fstat after unlink: nlink %d size %d mode %o", st.Nlink, st.Size, st.Mode&0o7777)
	}
}

// TestStalePathRetried checks path-based calls on a dentry the kernel still
// maps to the replaced object (no change was pushed): the server refuses to
// act on another object under the old identity (ESTALE), and the kernel
// repeats the call after a fresh lookup, which reaches the new object.
func TestStalePathRetried(t *testing.T) {
	h := newHarness(t, harnessOpts{noWatch: true, ttl: time.Hour})
	writeFile(t, h.b("f"), "old")
	requireUserXattr(t, h.b("f"))
	if err := os.Symlink("a", h.b("l")); err != nil {
		t.Fatal(err)
	}
	w := startFakeWatch(h)
	w.send(h, proto.WatchEvent{Seq: 1, Epoch: 1})
	for _, rel := range []string{"f", "l"} {
		_ = lstat(t, h.m(rel))
	}
	replaceByRename(t, h, "f", "new")
	if err := os.Symlink("b", h.b("l.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(h.b("l.tmp"), h.b("l")); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(h.m("f"), 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := unix.Setxattr(h.m("f"), "user.k", []byte("v"), 0); err != nil {
		t.Fatalf("setxattr: %v", err)
	}
	if got, err := os.Readlink(h.m("l")); err != nil || got != "b" {
		t.Fatalf("readlink = %q, %v", got, err)
	}
	if got := lstat(t, h.b("f")).Mode & 0o7777; got != 0o600 {
		t.Errorf("new file mode %o", got)
	}
	if got := lstat(t, h.b("f.old")).Mode & 0o7777; got != 0o644 {
		t.Errorf("replaced file mode changed to %o", got)
	}
	if _, err := unix.Getxattr(h.b("f.old"), "user.k", nil); !errors.Is(err, unix.ENODATA) {
		t.Errorf("xattr reached the replaced file: %v", err)
	}
}
