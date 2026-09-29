package telefs

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func lstat(t *testing.T, p string) *unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	return &st
}

func TestPOSIXRoundTrip(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	t.Run("create read write", func(t *testing.T) {
		writeFile(t, h.m("f"), "hello")
		if got := readFile(t, h.b("f")); got != "hello" {
			t.Fatalf("backing = %q", got)
		}
		if got := readFile(t, h.m("f")); got != "hello" {
			t.Fatalf("mount = %q", got)
		}
		fh, err := os.OpenFile(h.m("f"), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.WriteAt([]byte("J"), 0); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 5)
		if _, err := fh.ReadAt(buf, 0); err != nil {
			t.Fatal(err)
		}
		if err := fh.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := fh.Close(); err != nil {
			t.Fatal(err)
		}
		if string(buf) != "Jello" || readFile(t, h.b("f")) != "Jello" {
			t.Fatalf("after pwrite: mount %q backing %q", buf, readFile(t, h.b("f")))
		}
	})

	t.Run("append", func(t *testing.T) {
		writeFile(t, h.m("app"), "a")
		fh, err := os.OpenFile(h.m("app"), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.WriteString("b"); err != nil {
			t.Fatal(err)
		}
		if _, err := fh.WriteString("c"); err != nil {
			t.Fatal(err)
		}
		_ = fh.Close()
		if got := readFile(t, h.b("app")); got != "abc" {
			t.Fatalf("backing = %q", got)
		}
	})

	t.Run("truncate", func(t *testing.T) {
		writeFile(t, h.m("tr"), "0123456789")
		if err := os.Truncate(h.m("tr"), 4); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, h.b("tr")); got != "0123" {
			t.Fatalf("after truncate(2): %q", got)
		}
		fh, err := os.OpenFile(h.m("tr"), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := fh.Truncate(2); err != nil {
			t.Fatal(err)
		}
		_ = fh.Close()
		if got := readFile(t, h.b("tr")); got != "01" {
			t.Fatalf("after ftruncate: %q", got)
		}
		fh, err = os.OpenFile(h.m("tr"), os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Fatal(err)
		}
		_ = fh.Close()
		if st := lstat(t, h.m("tr")); st.Size != 0 {
			t.Fatalf("after O_TRUNC size = %d", st.Size)
		}
	})

	t.Run("rename", func(t *testing.T) {
		writeFile(t, h.m("r1"), "one")
		writeFile(t, h.m("r2"), "two")
		if err := os.Rename(h.m("r1"), h.m("r3")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(h.b("r1")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source still in backing: %v", err)
		}
		if got := readFile(t, h.m("r3")); got != "one" {
			t.Fatalf("renamed = %q", got)
		}
		err := unix.Renameat2(unix.AT_FDCWD, h.m("r3"), unix.AT_FDCWD, h.m("r2"), unix.RENAME_NOREPLACE)
		if !errors.Is(err, unix.EEXIST) {
			t.Fatalf("RENAME_NOREPLACE onto existing = %v, want EEXIST", err)
		}
		if err := unix.Renameat2(unix.AT_FDCWD, h.m("r3"), unix.AT_FDCWD, h.m("r2"), unix.RENAME_EXCHANGE); err != nil {
			t.Fatalf("RENAME_EXCHANGE: %v", err)
		}
		if readFile(t, h.b("r2")) != "one" || readFile(t, h.b("r3")) != "two" {
			t.Fatal("RENAME_EXCHANGE did not swap the files")
		}
		if readFile(t, h.m("r2")) != "one" || readFile(t, h.m("r3")) != "two" {
			t.Fatal("RENAME_EXCHANGE not visible through the mount")
		}
	})

	t.Run("unlink mkdir rmdir", func(t *testing.T) {
		writeFile(t, h.m("gone"), "x")
		if err := os.Remove(h.m("gone")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(h.b("gone")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unlinked file in backing: %v", err)
		}
		if err := os.Mkdir(h.m("d"), 0o750); err != nil {
			t.Fatal(err)
		}
		if st := lstat(t, h.b("d")); st.Mode&0o7777 != 0o750 || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			t.Fatalf("backing mode %o", st.Mode)
		}
		if err := os.Remove(h.m("d")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(h.b("d")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed dir in backing: %v", err)
		}
	})

	t.Run("symlink readlink", func(t *testing.T) {
		if err := os.Symlink("target/x", h.m("ln")); err != nil {
			t.Fatal(err)
		}
		got, err := os.Readlink(h.m("ln"))
		if err != nil || got != "target/x" {
			t.Fatalf("readlink = %q, %v", got, err)
		}
		if got, _ := os.Readlink(h.b("ln")); got != "target/x" {
			t.Fatalf("backing readlink = %q", got)
		}
		if st := lstat(t, h.m("ln")); st.Mode&unix.S_IFMT != unix.S_IFLNK {
			t.Fatalf("mode %o", st.Mode)
		}
		// A symlink to a file inside the mount is followed by the kernel.
		writeFile(t, h.m("lt"), "through link")
		if err := os.Symlink("lt", h.m("ln2")); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, h.m("ln2")); got != "through link" {
			t.Fatalf("read through symlink = %q", got)
		}
	})

	t.Run("hard link", func(t *testing.T) {
		writeFile(t, h.m("hl"), "linked")
		if err := os.Link(h.m("hl"), h.m("hl2")); err != nil {
			t.Fatal(err)
		}
		a, b := lstat(t, h.b("hl")), lstat(t, h.b("hl2"))
		if a.Ino != b.Ino {
			t.Fatal("backing entries are not hard links")
		}
		if ma, mb := lstat(t, h.m("hl")), lstat(t, h.m("hl2")); ma.Ino != mb.Ino || ma.Nlink != 2 {
			t.Fatalf("mount: ino %d/%d nlink %d", ma.Ino, mb.Ino, ma.Nlink)
		}
	})

	t.Run("chmod utimes", func(t *testing.T) {
		writeFile(t, h.m("attr"), "x")
		if err := os.Chmod(h.m("attr"), 0o640); err != nil {
			t.Fatal(err)
		}
		if st := lstat(t, h.b("attr")); st.Mode&0o7777 != 0o640 {
			t.Fatalf("backing mode %o", st.Mode)
		}
		if st := lstat(t, h.m("attr")); st.Mode&0o7777 != 0o640 {
			t.Fatalf("mount mode %o", st.Mode)
		}
		at := time.Unix(1_600_000_000, 123_000_000)
		mt := time.Unix(1_500_000_000, 456_000_000)
		if err := os.Chtimes(h.m("attr"), at, mt); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{h.b("attr"), h.m("attr")} {
			st := lstat(t, p)
			if got := time.Unix(st.Mtim.Unix()); !got.Equal(mt) {
				t.Fatalf("%s mtime %v, want %v", p, got, mt)
			}
			if got := time.Unix(st.Atim.Unix()); !got.Equal(at) {
				t.Fatalf("%s atime %v, want %v", p, got, at)
			}
		}
		// touch: UTIME_NOW uses the server's clock.
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, h.m("attr"), []unix.Timespec{{Nsec: unix.UTIME_NOW}, {Nsec: unix.UTIME_NOW}}, 0); err != nil {
			t.Fatal(err)
		}
		if st := lstat(t, h.b("attr")); time.Since(time.Unix(st.Mtim.Unix())) > time.Minute {
			t.Fatalf("UTIME_NOW mtime %v", time.Unix(st.Mtim.Unix()))
		}
	})

	t.Run("xattr", func(t *testing.T) {
		writeFile(t, h.b("xa"), "x")
		if err := unix.Setxattr(h.b("xa"), "user.probe", []byte("1"), 0); errors.Is(err, unix.ENOTSUP) {
			t.Skip("backing file system has no user xattrs")
		}
		if err := unix.Setxattr(h.m("xa"), "user.k", []byte("value"), 0); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, err := unix.Getxattr(h.b("xa"), "user.k", buf)
		if err != nil || string(buf[:n]) != "value" {
			t.Fatalf("backing getxattr = %q, %v", buf[:n], err)
		}
		if n, err := unix.Getxattr(h.m("xa"), "user.k", nil); err != nil || n != 5 {
			t.Fatalf("size query = %d, %v", n, err)
		}
		n, err = unix.Getxattr(h.m("xa"), "user.k", buf)
		if err != nil || string(buf[:n]) != "value" {
			t.Fatalf("mount getxattr = %q, %v", buf[:n], err)
		}
		if _, err := unix.Getxattr(h.m("xa"), "user.k", make([]byte, 2)); !errors.Is(err, unix.ERANGE) {
			t.Fatalf("small buffer = %v, want ERANGE", err)
		}
		n, err = unix.Listxattr(h.m("xa"), buf)
		if err != nil || !slices.Contains(strings.Split(string(buf[:n]), "\x00"), "user.k") {
			t.Fatalf("listxattr = %q, %v", buf[:n], err)
		}
		if err := unix.Setxattr(h.m("xa"), "user.k", []byte("v"), unix.XATTR_CREATE); !errors.Is(err, unix.EEXIST) {
			t.Fatalf("XATTR_CREATE on existing = %v, want EEXIST", err)
		}
		if err := unix.Removexattr(h.m("xa"), "user.k"); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.Getxattr(h.m("xa"), "user.k", buf); !errors.Is(err, unix.ENODATA) {
			t.Fatalf("getxattr after remove = %v, want ENODATA", err)
		}
	})

	t.Run("statfs access mkfifo", func(t *testing.T) {
		var got, want unix.Statfs_t
		if err := unix.Statfs(h.mnt, &got); err != nil {
			t.Fatal(err)
		}
		if err := unix.Statfs(h.backing, &want); err != nil {
			t.Fatal(err)
		}
		if got.Blocks != want.Blocks || got.Bsize != want.Bsize {
			t.Fatalf("statfs blocks %d bsize %d, want %d %d", got.Blocks, got.Bsize, want.Blocks, want.Bsize)
		}
		writeFile(t, h.m("acc"), "x")
		if err := unix.Access(h.m("acc"), unix.R_OK|unix.W_OK); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(h.m("fifo"), 0o600); err != nil {
			t.Fatal(err)
		}
		if st := lstat(t, h.b("fifo")); st.Mode&unix.S_IFMT != unix.S_IFIFO {
			t.Fatalf("backing mode %o", st.Mode)
		}
	})

	t.Run("large file", func(t *testing.T) {
		data := make([]byte, 5<<20+12345)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.m("big"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(h.b("big")); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("backing differs: %v", err)
		}
		if got, err := os.ReadFile(h.m("big")); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("mount differs: %v", err)
		}
	})

	t.Run("large directory", func(t *testing.T) {
		const n = 3000
		mkdirAll(t, h.b("many"))
		for i := range n {
			writeFile(t, h.b(fmt.Sprintf("many/file-%04d", i)), "")
		}
		ents, err := os.ReadDir(h.m("many"))
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != n {
			t.Fatalf("listed %d entries, want %d", len(ents), n)
		}
		testSeekdir(t, h.m("many"))
	})
}

// testSeekdir reads a directory in small chunks, remembers a cookie in the
// middle, and checks that seeking back to it lists the same entries again.
func testSeekdir(t *testing.T, dir string) {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	read := func() []string {
		buf := make([]byte, 4096)
		n, err := unix.Getdents(fd, buf)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
		return names
	}
	first := read()
	cookie, err := unix.Seek(fd, 0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	second := read()
	_ = read()
	if _, err := unix.Seek(fd, cookie, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	again := read()
	if len(first) == 0 || len(second) == 0 || !slices.Equal(second, again) {
		t.Fatalf("seekdir: second chunk %d entries, after seek %d; equal=%v", len(second), len(again), slices.Equal(second, again))
	}
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if again := read(); !slices.Equal(first, again) {
		t.Fatal("rewinddir lists different entries")
	}
}

func TestErrnoFidelity(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	mkdirAll(t, h.b("full/sub"))
	writeFile(t, h.b("file"), "x")
	mkdirAll(t, h.b("dir"))
	mkdirAll(t, h.b("other"))
	// A second file system inside the served tree, for EXDEV.
	mkdirAll(t, h.b("tmpfs"))
	if err := unix.Mount("tmpfs", h.b("tmpfs"), "tmpfs", 0, "size=1m"); err != nil {
		t.Fatalf("mount tmpfs: %v", err)
	}
	t.Cleanup(func() { _ = unix.Unmount(h.b("tmpfs"), unix.MNT_DETACH) })
	writeFile(t, h.b("tmpfs/x"), "x")

	long := strings.Repeat("n", 300)
	cases := []struct {
		name string
		op   func() error
		want syscall.Errno
	}{
		{"open missing", func() error { _, err := os.Open(h.m("missing")); return err }, unix.ENOENT},
		{"mkdir existing", func() error { return os.Mkdir(h.m("dir"), 0o755) }, unix.EEXIST},
		{"rmdir non-empty", func() error { return unix.Rmdir(h.m("full")) }, unix.ENOTEMPTY},
		// os.Rename refuses directory targets itself; use the syscall.
		{"rename onto non-empty dir", func() error { return unix.Rename(h.m("other"), h.m("full")) }, unix.ENOTEMPTY},
		{"unlink directory", func() error { return unix.Unlink(h.m("dir")) }, unix.EISDIR},
		{"rmdir file", func() error { return unix.Rmdir(h.m("file")) }, unix.ENOTDIR},
		{"lookup below file", func() error { _, err := os.Stat(h.m("file/x")); return err }, unix.ENOTDIR},
		{"name too long", func() error { _, err := os.Stat(h.m(long)); return err }, unix.ENAMETOOLONG},
		{"rename across remote file systems", func() error { return os.Rename(h.m("tmpfs/x"), h.m("x")) }, unix.EXDEV},
		{"readlink of a file", func() error { _, err := os.Readlink(h.m("file")); return err }, unix.EINVAL},
		{"getxattr missing", func() error { _, err := unix.Getxattr(h.m("file"), "user.none", nil); return err }, unix.ENODATA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.op()
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestUnrepresentableErrno checks that remote errnos the kernel refuses in a
// FUSE reply (512 and above, kernel-internal codes that NFS leaks) reach the
// caller as a representable errno. Handed on unchanged, the kernel rejects
// the reply and the caller waits uninterruptibly forever.
func TestUnrepresentableErrno(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	writeFile(t, h.b("xa"), "")
	h.opener.setHook(func(req *proto.FSRequest, forward func() *proto.FSResponse) *proto.FSResponse {
		switch {
		case req.Op == proto.FSLookup && req.Name == "enotsupp":
			return &proto.FSResponse{Errno: enotsupp}
		case req.Op == proto.FSLookup && req.Name == "badhandle":
			return &proto.FSResponse{Errno: 521}
		case req.Op == proto.FSGetxattr:
			return &proto.FSResponse{Errno: enotsupp}
		}
		return forward()
	})
	cases := []struct {
		name string
		op   func() error
		want syscall.Errno
	}{
		{"lookup ENOTSUPP", func() error { _, err := os.Lstat(h.m("enotsupp")); return err }, unix.EOPNOTSUPP},
		{"lookup EBADHANDLE", func() error { _, err := os.Lstat(h.m("badhandle")); return err }, unix.EIO},
		{"getxattr ENOTSUPP", func() error { _, err := unix.Getxattr(h.m("xa"), "user.k", nil); return err }, unix.EOPNOTSUPP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			h.returnsInTime(t, func() { err = tc.op() })
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// returnsInTime runs op, which must return promptly. If it does not, the
// caller may be stuck in the kernel waiting for a reply that never comes,
// which only aborting the FUSE connection ends: a forced unmount does that.
func (h *harness) returnsInTime(t *testing.T, op func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = unix.Unmount(h.mnt, unix.MNT_FORCE|unix.MNT_DETACH)
		<-done
		t.Fatal("system call on the mount did not return")
	}
}

// TestReaddirTypeOnlyEntries checks that an entry the server could list but
// not examine is listed, and that its stand-in attributes are not cached as
// the entry's: a stat looks it up for real.
func TestReaddirTypeOnlyEntries(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	mkdirAll(t, h.b("d"))
	writeFile(t, h.b("d/f"), "12345")
	h.opener.setHook(func(req *proto.FSRequest, forward func() *proto.FSResponse) *proto.FSResponse {
		resp := forward()
		if req.Op != proto.FSReaddir {
			return resp
		}
		for i := range resp.Entries {
			if e := &resp.Entries[i]; e.Name == "f" {
				e.Attr = proto.Attr{Dev: e.Attr.Dev, Ino: e.Attr.Ino, Mode: e.Attr.Mode & unix.S_IFMT}
				e.TypeOnly = true
			}
		}
		return resp
	})
	ents, err := os.ReadDir(h.m("d"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "f" || !ents[0].Type().IsRegular() {
		t.Fatalf("listing %v", ents)
	}
	if st := lstat(t, h.m("d/f")); st.Size != 5 || st.Mode != unix.S_IFREG|0o644 {
		t.Fatalf("stat after listing: size %d mode %o", st.Size, st.Mode)
	}
}

// TestCloseSyncsWrites checks that close(2) of a file written through the
// mount confirms that the remote host has the data on disk (docs/telefs.md
// "一致性") and reports a failure to put it there, while a close without
// writes costs no round trip.
func TestCloseSyncsWrites(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	var syncs atomic.Int32
	var fail atomic.Bool
	h.opener.setHook(func(req *proto.FSRequest, forward func() *proto.FSResponse) *proto.FSResponse {
		if req.Op == proto.FSFsync {
			syncs.Add(1)
			if fail.Load() {
				return &proto.FSResponse{Errno: uint32(unix.ENOSPC)}
			}
		}
		return forward()
	})
	writeFile(t, h.m("w"), "data")
	if n := syncs.Load(); n != 1 {
		t.Fatalf("%d syncs for one written file", n)
	}
	_ = readFile(t, h.m("w"))
	fh, err := os.OpenFile(h.m("w"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	if n := syncs.Load(); n != 1 {
		t.Fatalf("%d syncs after closes without writes", n)
	}

	fail.Store(true)
	fh, err = os.OpenFile(h.m("w"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("more"); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); !errors.Is(err, unix.ENOSPC) {
		t.Fatalf("close after a failed sync = %v, want ENOSPC", err)
	}
}
