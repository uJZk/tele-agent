package fssvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func newSvc(t testing.TB) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	s, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, root
}

// call runs one request through ServeRequest over a pipe, like an FS
// stream whose header was read.
func call(t testing.TB, s *Service, req *proto.FSRequest) *proto.FSResponse {
	t.Helper()
	resp, err := serve(s, req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// serve is call for goroutines other than the test's.
func serve(s *Service, req *proto.FSRequest) (*proto.FSResponse, error) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- s.ServeRequest(context.Background(), b) }()
	defer func() { _ = a.Close() }()
	if err := proto.WriteFrame(a, req); err != nil {
		return nil, err
	}
	if req.Op == proto.FSForget {
		// No response: the server closes the stream when done.
		if _, err := io.Copy(io.Discard, a); err != nil {
			return nil, err
		}
		return nil, <-done
	}
	var resp proto.FSResponse
	if err := proto.ReadFrame(a, &resp, proto.MaxDataFrame); err != nil {
		return nil, err
	}
	return &resp, <-done
}

// ok runs req and fails the test unless it succeeds.
func ok(t testing.TB, s *Service, req *proto.FSRequest) *proto.FSResponse {
	t.Helper()
	resp := call(t, s, req)
	if resp.Errno != 0 {
		t.Fatalf("op %d %s/%s: %v", req.Op, req.Path, req.Name, unix.Errno(resp.Errno))
	}
	return resp
}

func wantErrno(t testing.TB, resp *proto.FSResponse, want syscall.Errno) {
	t.Helper()
	if syscall.Errno(resp.Errno) != want {
		t.Fatalf("errno %v, want %v", syscall.Errno(resp.Errno), want)
	}
}

func TestNewValidatesRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", "relative", "/nonexistent-fssvc-root", file, "/a/../b"} {
		if s, err := New(Config{Root: root}); err == nil {
			_ = s.Close()
			t.Errorf("New(%q) succeeded", root)
		}
	}
}

func TestFileOps(t *testing.T) {
	s, root := newSvc(t)

	d := ok(t, s, &proto.FSRequest{Op: proto.FSMkdir, Path: "/", Name: "d", Mode: 0o750})
	if d.Attr.Mode != unix.S_IFDIR|0o750 {
		t.Fatalf("mkdir mode %o", d.Attr.Mode)
	}
	cr := ok(t, s, &proto.FSRequest{Op: proto.FSCreate, Path: "/d", Name: "f", Flags: unix.O_RDWR, Mode: 0o640})
	if cr.Handle == 0 || cr.Attr.Mode != unix.S_IFREG|0o640 {
		t.Fatalf("create: handle %d mode %o", cr.Handle, cr.Attr.Mode)
	}
	h := cr.Handle
	if w := ok(t, s, &proto.FSRequest{Op: proto.FSWrite, Handle: h, Data: []byte("hello")}); w.Written != 5 {
		t.Fatalf("written %d", w.Written)
	}
	if r := ok(t, s, &proto.FSRequest{Op: proto.FSRead, Handle: h, Size: 100}); string(r.Data) != "hello" {
		t.Fatalf("read %q", r.Data)
	}
	if r := ok(t, s, &proto.FSRequest{Op: proto.FSRead, Handle: h, Offset: 3, Size: 1}); string(r.Data) != "l" {
		t.Fatalf("read at offset %q", r.Data)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSFsync, Handle: h})
	ok(t, s, &proto.FSRequest{Op: proto.FSFsync, Handle: h, Flags: 1})

	sa := ok(t, s, &proto.FSRequest{Op: proto.FSSetattr, Handle: h, SetAttr: &proto.SetAttr{Valid: proto.SetSize, Size: 2}})
	if sa.Attr.Size != 2 {
		t.Fatalf("ftruncate size %d", sa.Attr.Size)
	}
	sa = ok(t, s, &proto.FSRequest{Op: proto.FSSetattr, Path: "/d/f", SetAttr: &proto.SetAttr{Valid: proto.SetMode | proto.SetMtime | proto.SetAtime, Mode: 0o600, Mtime: 1_500_000_000_123_456_789, Atime: 1_400_000_000_000_000_000}})
	if sa.Attr.Mode&0o7777 != 0o600 || sa.Attr.Mtime != 1_500_000_000_123_456_789 || sa.Attr.Atime != 1_400_000_000_000_000_000 {
		t.Fatalf("setattr: mode %o mtime %d atime %d", sa.Attr.Mode, sa.Attr.Mtime, sa.Attr.Atime)
	}
	sa = ok(t, s, &proto.FSRequest{Op: proto.FSSetattr, Handle: h, SetAttr: &proto.SetAttr{Valid: proto.SetMtimeNow | proto.SetMode, Mode: 0o644}})
	if time.Since(time.Unix(0, sa.Attr.Mtime)) > time.Minute || sa.Attr.Mode&0o7777 != 0o644 {
		t.Fatalf("setattr now: mtime %v mode %o", time.Unix(0, sa.Attr.Mtime), sa.Attr.Mode)
	}
	byHandle := ok(t, s, &proto.FSRequest{Op: proto.FSGetattr, Handle: h})
	byPath := ok(t, s, &proto.FSRequest{Op: proto.FSGetattr, Path: "/d/f"})
	if *byHandle.Attr != *byPath.Attr {
		t.Fatalf("getattr by handle %+v, by path %+v", byHandle.Attr, byPath.Attr)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSRelease, Handle: h})
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSRead, Handle: h, Size: 1}), unix.EBADF)

	// O_APPEND appends whatever the offset; O_TRUNC truncates.
	op := ok(t, s, &proto.FSRequest{Op: proto.FSOpen, Path: "/d/f", Flags: unix.O_WRONLY | unix.O_APPEND})
	ok(t, s, &proto.FSRequest{Op: proto.FSWrite, Handle: op.Handle, Data: []byte("X")})
	ok(t, s, &proto.FSRequest{Op: proto.FSRelease, Handle: op.Handle})
	if got, _ := os.ReadFile(filepath.Join(root, "d/f")); string(got) != "heX" {
		t.Fatalf("after append %q", got)
	}
	op = ok(t, s, &proto.FSRequest{Op: proto.FSOpen, Path: "/d/f", Flags: unix.O_WRONLY | unix.O_TRUNC})
	ok(t, s, &proto.FSRequest{Op: proto.FSRelease, Handle: op.Handle})
	if st, _ := os.Stat(filepath.Join(root, "d/f")); st.Size() != 0 {
		t.Fatalf("after O_TRUNC size %d", st.Size())
	}

	sl := ok(t, s, &proto.FSRequest{Op: proto.FSSymlink, Path: "/d", Name: "l", Target: "f"})
	if sl.Attr.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatalf("symlink mode %o", sl.Attr.Mode)
	}
	if rl := ok(t, s, &proto.FSRequest{Op: proto.FSReadlink, Path: "/d/l"}); rl.Target != "f" {
		t.Fatalf("readlink %q", rl.Target)
	}
	ln := ok(t, s, &proto.FSRequest{Op: proto.FSLink, Path: "/d/f", Path2: "/d", Name2: "g"})
	if ln.Attr.Nlink != 2 || ln.Attr.Ino != byPath.Attr.Ino {
		t.Fatalf("link nlink %d ino %d", ln.Attr.Nlink, ln.Attr.Ino)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSRename, Path: "/d", Name: "g", Path2: "/", Name2: "h"})
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSRename, Path: "/", Name: "h", Path2: "/d", Name2: "f", Flags: unix.RENAME_NOREPLACE}), unix.EEXIST)
	ok(t, s, &proto.FSRequest{Op: proto.FSRename, Path: "/", Name: "h", Path2: "/d", Name2: "l", Flags: unix.RENAME_EXCHANGE})
	if lk := ok(t, s, &proto.FSRequest{Op: proto.FSLookup, Path: "/", Name: "h"}); lk.Attr.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatalf("after exchange /h mode %o", lk.Attr.Mode)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSUnlink, Path: "/", Name: "h"})
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSLookup, Path: "/", Name: "h"}), unix.ENOENT)

	mk := ok(t, s, &proto.FSRequest{Op: proto.FSMknod, Path: "/d", Name: "p", Mode: unix.S_IFIFO | 0o600})
	if mk.Attr.Mode != unix.S_IFIFO|0o600 {
		t.Fatalf("mknod mode %o", mk.Attr.Mode)
	}
	if st := ok(t, s, &proto.FSRequest{Op: proto.FSStatfs, Path: "/d"}); st.Statfs.Bsize == 0 || st.Statfs.Blocks == 0 {
		t.Fatalf("statfs %+v", st.Statfs)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSAccess, Path: "/d/f", Mask: unix.R_OK | unix.W_OK})
	ok(t, s, &proto.FSRequest{Op: proto.FSAccess, Path: "/d", Mask: unix.X_OK})
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSAccess, Path: "/nope", Mask: unix.R_OK}), unix.ENOENT)
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSRmdir, Path: "/", Name: "d"}), unix.ENOTEMPTY)
}

func TestXattrOps(t *testing.T) {
	s, root := newSvc(t)
	p := filepath.Join(root, "x")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(p, "user.probe", []byte("1"), 0); errors.Is(err, unix.ENOTSUP) {
		t.Skip("no user xattrs on the test file system")
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSSetxattr, Path: "/x", Name2: "user.k", Data: []byte("value")})
	if r := ok(t, s, &proto.FSRequest{Op: proto.FSGetxattr, Path: "/x", Name2: "user.k"}); r.Size != 5 {
		t.Fatalf("size query %d", r.Size)
	}
	if r := ok(t, s, &proto.FSRequest{Op: proto.FSGetxattr, Path: "/x", Name2: "user.k", Size: 64}); string(r.Data) != "value" {
		t.Fatalf("getxattr %q", r.Data)
	}
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSGetxattr, Path: "/x", Name2: "user.k", Size: 2}), unix.ERANGE)
	r := ok(t, s, &proto.FSRequest{Op: proto.FSListxattr, Path: "/x", Size: 256})
	if !slices.Contains(strings.Split(string(r.Data), "\x00"), "user.k") {
		t.Fatalf("listxattr %q", r.Data)
	}
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSSetxattr, Path: "/x", Name2: "user.k", Data: []byte("v"), Flags: unix.XATTR_CREATE}), unix.EEXIST)
	ok(t, s, &proto.FSRequest{Op: proto.FSRemovexattr, Path: "/x", Name2: "user.k"})
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSGetxattr, Path: "/x", Name2: "user.k"}), unix.ENODATA)
}

func TestErrnos(t *testing.T) {
	s, root := newSvc(t)
	for _, d := range []string{"dir", "full/sub"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dirH := ok(t, s, &proto.FSRequest{Op: proto.FSOpendir, Path: "/dir"}).Handle
	fileH := ok(t, s, &proto.FSRequest{Op: proto.FSOpen, Path: "/file"}).Handle

	cases := []struct {
		name string
		req  proto.FSRequest
		want syscall.Errno
	}{
		{"lookup missing", proto.FSRequest{Op: proto.FSLookup, Path: "/", Name: "missing"}, unix.ENOENT},
		{"lookup below file", proto.FSRequest{Op: proto.FSLookup, Path: "/file", Name: "x"}, unix.ENOTDIR},
		{"mkdir existing", proto.FSRequest{Op: proto.FSMkdir, Path: "/", Name: "dir", Mode: 0o755}, unix.EEXIST},
		{"create exclusive existing", proto.FSRequest{Op: proto.FSCreate, Path: "/", Name: "file", Flags: unix.O_WRONLY | unix.O_EXCL, Mode: 0o644}, unix.EEXIST},
		{"rmdir non-empty", proto.FSRequest{Op: proto.FSRmdir, Path: "/", Name: "full"}, unix.ENOTEMPTY},
		{"unlink directory", proto.FSRequest{Op: proto.FSUnlink, Path: "/", Name: "dir"}, unix.EISDIR},
		{"rmdir file", proto.FSRequest{Op: proto.FSRmdir, Path: "/", Name: "file"}, unix.ENOTDIR},
		{"opendir file", proto.FSRequest{Op: proto.FSOpendir, Path: "/file"}, unix.ENOTDIR},
		{"readlink file", proto.FSRequest{Op: proto.FSReadlink, Path: "/file"}, unix.EINVAL},
		{"bad handle", proto.FSRequest{Op: proto.FSRead, Handle: 999, Size: 1}, unix.EBADF},
		{"read directory handle", proto.FSRequest{Op: proto.FSRead, Handle: dirH, Size: 1}, unix.EISDIR},
		{"readdir file handle", proto.FSRequest{Op: proto.FSReaddir, Handle: fileH}, unix.ENOTDIR},
		{"write read-only handle", proto.FSRequest{Op: proto.FSWrite, Handle: fileH, Data: []byte("x")}, unix.EBADF},
		{"release twice", proto.FSRequest{Op: proto.FSRelease, Handle: 12345}, unix.EBADF},
		{"relative path", proto.FSRequest{Op: proto.FSGetattr, Path: "file"}, unix.EINVAL},
		{"unclean path", proto.FSRequest{Op: proto.FSGetattr, Path: "/dir/../file"}, unix.EINVAL},
		{"NUL in path", proto.FSRequest{Op: proto.FSGetattr, Path: "/fi\x00le"}, unix.EINVAL},
		{"dot-dot name", proto.FSRequest{Op: proto.FSLookup, Path: "/dir", Name: ".."}, unix.EINVAL},
		{"slash in name", proto.FSRequest{Op: proto.FSCreate, Path: "/", Name: "a/b", Mode: 0o644}, unix.EINVAL},
		{"setattr without attributes", proto.FSRequest{Op: proto.FSSetattr, Path: "/file"}, unix.EINVAL},
		{"unknown rename flag", proto.FSRequest{Op: proto.FSRename, Path: "/", Name: "file", Path2: "/", Name2: "x", Flags: 1 << 20}, unix.EINVAL},
		{"empty symlink target", proto.FSRequest{Op: proto.FSSymlink, Path: "/", Name: "l"}, unix.EINVAL},
		{"empty xattr name", proto.FSRequest{Op: proto.FSGetxattr, Path: "/file"}, unix.EINVAL},
		{"mknod directory", proto.FSRequest{Op: proto.FSMknod, Path: "/", Name: "n", Mode: unix.S_IFDIR | 0o755}, unix.EINVAL},
		{"negative read offset", proto.FSRequest{Op: proto.FSRead, Handle: fileH, Offset: -1, Size: 1}, unix.EINVAL},
		{"access with bad mask", proto.FSRequest{Op: proto.FSAccess, Path: "/file", Mask: 8}, unix.EINVAL},
		{"unknown op", proto.FSRequest{Op: 200}, unix.ENOSYS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErrno(t, call(t, s, &tc.req), tc.want)
		})
	}
}

// TestNoFollow checks lstat semantics: operations on a symlink act on the
// symlink or fail, and never reach its target.
func TestNoFollow(t *testing.T) {
	s, root := newSvc(t)
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	if a := ok(t, s, &proto.FSRequest{Op: proto.FSGetattr, Path: "/link"}); a.Attr.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatalf("getattr follows: mode %o", a.Attr.Mode)
	}
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSOpen, Path: "/link", Flags: unix.O_RDONLY}), unix.ELOOP)
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSSetattr, Path: "/link", SetAttr: &proto.SetAttr{Valid: proto.SetSize}}), unix.ELOOP)
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSSetattr, Path: "/link", SetAttr: &proto.SetAttr{Valid: proto.SetMode, Mode: 0o600}}), unix.EOPNOTSUPP)
	// user.* attributes are not allowed on symlinks; the target must not
	// get one instead.
	resp := call(t, s, &proto.FSRequest{Op: proto.FSSetxattr, Path: "/link", Name2: "user.k", Data: []byte("v")})
	if resp.Errno == 0 {
		t.Fatal("setxattr on a symlink succeeded")
	}
	if _, err := unix.Getxattr(target, "user.k", nil); err == nil {
		t.Fatal("setxattr reached the symlink target")
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSSetattr, Path: "/link", SetAttr: &proto.SetAttr{Valid: proto.SetMtime, Mtime: 1_000_000_000_000_000_000}})
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("target changed: %v %d %v", after.Mode(), after.Size(), after.ModTime())
	}
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(root, "link"), &st); err != nil || st.Mtim.Nano() != 1_000_000_000_000_000_000 {
		t.Fatalf("symlink mtime %d, %v", st.Mtim.Nano(), err)
	}
}

func TestHandleSurvivesUnlink(t *testing.T) {
	s, _ := newSvc(t)
	cr := ok(t, s, &proto.FSRequest{Op: proto.FSCreate, Path: "/", Name: "tmp", Flags: unix.O_RDWR, Mode: 0o600})
	ok(t, s, &proto.FSRequest{Op: proto.FSUnlink, Path: "/", Name: "tmp"})
	ok(t, s, &proto.FSRequest{Op: proto.FSWrite, Handle: cr.Handle, Data: []byte("abc")})
	if a := ok(t, s, &proto.FSRequest{Op: proto.FSGetattr, Handle: cr.Handle}); a.Attr.Size != 3 || a.Attr.Nlink != 0 {
		t.Fatalf("size %d nlink %d", a.Attr.Size, a.Attr.Nlink)
	}
	a := ok(t, s, &proto.FSRequest{Op: proto.FSSetattr, Handle: cr.Handle, SetAttr: &proto.SetAttr{Valid: proto.SetSize | proto.SetMode, Size: 1, Mode: 0o640}})
	if a.Attr.Size != 1 || a.Attr.Mode&0o7777 != 0o640 {
		t.Fatalf("setattr by handle: size %d mode %o", a.Attr.Size, a.Attr.Mode)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSRelease, Handle: cr.Handle})
	if n := s.handles.count(); n != 0 {
		t.Fatalf("%d handles left", n)
	}
}

// TestCreateModeIgnoresServerUmask checks that the mode the client sends,
// already masked by the caller's umask, is applied exactly.
func TestCreateModeIgnoresServerUmask(t *testing.T) {
	old := unix.Umask(0o077)
	defer unix.Umask(old)
	s, root := newSvc(t)
	if s.umask != 0o077 {
		t.Fatalf("service read umask %o", s.umask)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSRelease, Handle: ok(t, s, &proto.FSRequest{Op: proto.FSCreate, Path: "/", Name: "f", Flags: unix.O_WRONLY, Mode: 0o644}).Handle})
	ok(t, s, &proto.FSRequest{Op: proto.FSMkdir, Path: "/", Name: "d", Mode: 0o755})
	ok(t, s, &proto.FSRequest{Op: proto.FSMknod, Path: "/", Name: "p", Mode: unix.S_IFIFO | 0o644})
	for name, want := range map[string]os.FileMode{"f": 0o644, "d": 0o755, "p": 0o644} {
		st, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s mode %o, want %o", name, got, want)
		}
	}
	// An existing file keeps its mode when opened with O_CREAT.
	if err := os.Chmod(filepath.Join(root, "f"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSRelease, Handle: ok(t, s, &proto.FSRequest{Op: proto.FSCreate, Path: "/", Name: "f", Flags: unix.O_WRONLY, Mode: 0o666}).Handle})
	if st, _ := os.Lstat(filepath.Join(root, "f")); st.Mode().Perm() != 0o600 {
		t.Errorf("existing file mode changed to %o", st.Mode().Perm())
	}
}

// TestMkdirKeepsSetgid checks that restoring the mode of a new directory
// keeps the S_ISGID it inherits from a setgid parent, as a native mkdir
// does.
func TestMkdirKeepsSetgid(t *testing.T) {
	old := unix.Umask(0o022)
	defer unix.Umask(old)
	s, root := newSvc(t)
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chmod(shared, 0o2775); err != nil {
		t.Fatal(err)
	}
	// The client's umask 002 lets group write through; the server's 022
	// would not.
	a := ok(t, s, &proto.FSRequest{Op: proto.FSMkdir, Path: "/shared", Name: "d", Mode: 0o775}).Attr
	if a.Mode&0o7777 != 0o2775 {
		t.Fatalf("mkdir in a setgid directory: mode %o, want 2775", a.Mode&0o7777)
	}
	if st := lstatT(t, filepath.Join(shared, "d")); st.Mode&0o7777 != 0o2775 {
		t.Fatalf("backing mode %o", st.Mode&0o7777)
	}
}

func lstatT(t *testing.T, p string) *unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

// nodeID returns the identity of real path p.
func nodeID(t *testing.T, p string) *proto.NodeID {
	t.Helper()
	st := lstatT(t, p)
	return &proto.NodeID{Dev: st.Dev, Ino: st.Ino}
}

// TestNodeIdentity checks that a request carrying the identity of the
// object the client means fails with ESTALE, and changes nothing, once the
// path names another object; and that requests on an open handle still
// reach the replaced object.
func TestNodeIdentity(t *testing.T) {
	s, root := newSvc(t)
	p := func(rel string) string { return filepath.Join(root, rel) }
	if err := os.WriteFile(p("f"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(p("f"), "user.probe", []byte("1"), 0); errors.Is(err, unix.ENOTSUP) {
		t.Skip("no user xattrs on the test file system")
	}
	if err := os.Symlink("a", p("l")); err != nil {
		t.Fatal(err)
	}
	oldF, oldL := nodeID(t, p("f")), nodeID(t, p("l"))
	h := ok(t, s, &proto.FSRequest{Op: proto.FSOpen, Path: "/f", Flags: unix.O_RDONLY}).Handle
	// Replace both by rename.
	if err := os.WriteFile(p("f.tmp"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p("f"), p("f.old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p("f.tmp"), p("f")); err != nil {
		t.Fatal(err)
	}
	// The new symlink exists before the old one is freed, so that it
	// cannot reuse its inode number.
	if err := os.Symlink("b", p("l.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p("l.tmp"), p("l")); err != nil {
		t.Fatal(err)
	}

	stale := []proto.FSRequest{
		{Op: proto.FSGetattr, Path: "/f", Node: oldF},
		{Op: proto.FSSetattr, Path: "/f", Node: oldF, SetAttr: &proto.SetAttr{Valid: proto.SetMode | proto.SetMtime, Mode: 0o600, Mtime: 1}},
		{Op: proto.FSSetattr, Path: "/f", Node: oldF, SetAttr: &proto.SetAttr{Valid: proto.SetSize}},
		{Op: proto.FSAccess, Path: "/f", Node: oldF, Mask: unix.R_OK},
		{Op: proto.FSSetxattr, Path: "/f", Node: oldF, Name2: "user.k", Data: []byte("v")},
		{Op: proto.FSGetxattr, Path: "/f", Node: oldF, Name2: "user.probe"},
		{Op: proto.FSListxattr, Path: "/f", Node: oldF},
		{Op: proto.FSRemovexattr, Path: "/f", Node: oldF, Name2: "user.probe"},
		{Op: proto.FSReadlink, Path: "/l", Node: oldL},
	}
	for i := range stale {
		if resp := call(t, s, &stale[i]); resp.Errno != uint32(unix.ESTALE) {
			t.Errorf("op %d on a replaced node: errno %v, want ESTALE", stale[i].Op, unix.Errno(resp.Errno))
		}
	}
	st := lstatT(t, p("f"))
	if st.Mode&0o7777 != 0o644 || st.Size != 3 || st.Mtim.Nano() == 1 {
		t.Fatalf("stale requests changed the new file: mode %o size %d", st.Mode&0o7777, st.Size)
	}
	if _, err := unix.Getxattr(p("f"), "user.k", nil); !errors.Is(err, unix.ENODATA) {
		t.Fatalf("stale setxattr reached the new file: %v", err)
	}

	// The current identity works.
	cur := nodeID(t, p("f"))
	ok(t, s, &proto.FSRequest{Op: proto.FSSetattr, Path: "/f", Node: cur, SetAttr: &proto.SetAttr{Valid: proto.SetMode, Mode: 0o640}})
	if rl := ok(t, s, &proto.FSRequest{Op: proto.FSReadlink, Path: "/l", Node: nodeID(t, p("l"))}); rl.Target != "b" {
		t.Fatalf("readlink %q", rl.Target)
	}

	// A handle reaches the replaced file.
	ok(t, s, &proto.FSRequest{Op: proto.FSSetattr, Handle: h, SetAttr: &proto.SetAttr{Valid: proto.SetMode, Mode: 0o600}})
	ok(t, s, &proto.FSRequest{Op: proto.FSSetxattr, Handle: h, Name2: "user.k", Data: []byte("v")})
	if r := ok(t, s, &proto.FSRequest{Op: proto.FSGetxattr, Handle: h, Name2: "user.k", Size: 16}); string(r.Data) != "v" {
		t.Fatalf("getxattr by handle %q", r.Data)
	}
	if r := ok(t, s, &proto.FSRequest{Op: proto.FSListxattr, Handle: h, Size: 256}); !slices.Contains(strings.Split(string(r.Data), "\x00"), "user.k") {
		t.Fatalf("listxattr by handle %q", r.Data)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSRemovexattr, Handle: h, Name2: "user.probe"})
	if st := lstatT(t, p("f.old")); st.Mode&0o7777 != 0o600 {
		t.Fatalf("replaced file mode %o", st.Mode&0o7777)
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSRelease, Handle: h})
}

// TestAccessIsKernelAccurate checks FSAccess against answers that only the
// kernel knows: a check emulated from the mode bits would get them wrong.
func TestAccessIsKernelAccurate(t *testing.T) {
	s, root := newSvc(t)
	f := filepath.Join(root, "immutable")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("immutable", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// A symlink itself is accessible whatever its target.
	ok(t, s, &proto.FSRequest{Op: proto.FSAccess, Path: "/link", Mask: unix.R_OK | unix.W_OK | unix.X_OK})
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSAccess, Path: "/immutable", Mask: unix.X_OK}), unix.EACCES)

	if !setImmutable(t, f, true) {
		t.Skip("cannot make files immutable here (chattr +i)")
	}
	defer setImmutable(t, f, false)
	var want syscall.Errno
	if !errors.As(unix.Access(f, unix.W_OK), &want) {
		t.Fatal("access(W_OK) of an immutable file succeeds natively")
	}
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSAccess, Path: "/immutable", Mask: unix.W_OK}), want)
}

// fsImmutableFL is FS_IMMUTABLE_FL of linux/fs.h, which x/sys lacks.
const fsImmutableFL = 0x10

// setImmutable sets or clears FS_IMMUTABLE_FL on p (chattr +i), reporting
// whether the file system and the caller's privileges allow it.
func setImmutable(t *testing.T, p string, on bool) bool {
	t.Helper()
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	flags, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return false
	}
	if on {
		flags |= fsImmutableFL
	} else {
		flags &^= fsImmutableFL
	}
	return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(flags)) == nil
}

func TestReaddirPaging(t *testing.T) {
	s, root := newSvc(t)
	const n = 1000
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("d/f%04d", i)), bytes.Repeat([]byte("x"), i), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := ok(t, s, &proto.FSRequest{Op: proto.FSOpendir, Path: "/d"}).Handle

	var all []proto.DirEntry
	var pages [][]proto.DirEntry
	var off int64
	for {
		r := ok(t, s, &proto.FSRequest{Op: proto.FSReaddir, Handle: h, Offset: off, Size: 100})
		if len(r.Entries) > 100 {
			t.Fatalf("page of %d entries", len(r.Entries))
		}
		for _, e := range r.Entries {
			if e.Offset == 0 {
				t.Fatalf("entry %s has offset 0", e.Name)
			}
		}
		pages = append(pages, r.Entries)
		all = append(all, r.Entries...)
		if r.EOF {
			break
		}
		if len(r.Entries) == 0 {
			t.Fatal("empty page without EOF")
		}
		off = r.Entries[len(r.Entries)-1].Offset
	}
	names := map[string]bool{}
	for _, e := range all {
		if names[e.Name] {
			t.Fatalf("duplicate %s", e.Name)
		}
		names[e.Name] = true
		if e.Name == "." || e.Name == ".." {
			continue
		}
		var i int
		if _, err := fmt.Sscanf(e.Name, "f%04d", &i); err != nil {
			t.Fatalf("unexpected entry %q", e.Name)
		}
		if e.Attr.Size != int64(i) || e.Attr.Mode != unix.S_IFREG|0o644 {
			t.Fatalf("%s: size %d mode %o", e.Name, e.Attr.Size, e.Attr.Mode)
		}
	}
	if len(names) != n+2 || !names["."] || !names[".."] {
		t.Fatalf("listed %d names", len(names))
	}

	// Seek back to the cookie before the third page, then rewind.
	resume := pages[1][len(pages[1])-1].Offset
	r := ok(t, s, &proto.FSRequest{Op: proto.FSReaddir, Handle: h, Offset: resume, Size: 100})
	if !slices.EqualFunc(r.Entries, pages[2], func(a, b proto.DirEntry) bool { return a.Name == b.Name && a.Offset == b.Offset }) {
		t.Fatal("seeking to a cookie lists different entries")
	}
	r = ok(t, s, &proto.FSRequest{Op: proto.FSReaddir, Handle: h, Size: 100})
	if len(r.Entries) != len(pages[0]) || r.Entries[0].Name != pages[0][0].Name {
		t.Fatal("rewind lists different entries")
	}
	ok(t, s, &proto.FSRequest{Op: proto.FSReleasedir, Handle: h})
}

func TestParseDirents(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "bb", strings.Repeat("c", 255)} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, 8192)
	n, err := unix.Getdents(fd, buf)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	_, _, want = unix.ParseDirent(buf[:n], -1, want)
	var got []string
	for _, d := range parseDirents(buf[:n]) {
		if d.name != "." && d.name != ".." {
			got = append(got, d.name)
		}
		if d.off == 0 {
			t.Fatalf("%s has offset 0", d.name)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("parsed %q, want %q", got, want)
	}
}

func FuzzParseDirents(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 24))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, d := range parseDirents(b) {
			if d.name == "" || strings.IndexByte(d.name, 0) >= 0 {
				t.Fatalf("bad name %q", d.name)
			}
		}
	})
}

// FuzzServeRequest executes arbitrary requests against a temporary root.
// Symlinks and device nodes are left out: with symlinks the fuzzer could
// redirect later requests outside the root (Root is not a security
// boundary), and device nodes would let it reach real devices.
func FuzzServeRequest(f *testing.F) {
	s, _ := newSvc(f)
	seeds := []proto.FSRequest{
		{Op: proto.FSMkdir, Path: "/", Name: "d", Mode: 0o755},
		{Op: proto.FSCreate, Path: "/d", Name: "f", Flags: unix.O_RDWR, Mode: 0o644},
		{Op: proto.FSWrite, Handle: 1, Data: []byte("x")},
		{Op: proto.FSReaddir, Handle: 1, Size: 10},
		{Op: proto.FSRename, Path: "/d", Name: "f", Path2: "/", Name2: "g", Flags: unix.RENAME_NOREPLACE},
		{Op: proto.FSSetattr, Path: "/g", SetAttr: &proto.SetAttr{Valid: proto.SetSize | proto.SetMode, Size: 3, Mode: 0o600}},
		{Op: proto.FSForget, Path: "/d"},
	}
	for i := range seeds {
		b, err := proto.Marshal(&seeds[i])
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var req proto.FSRequest
		if proto.Unmarshal(b, &req) != nil {
			return
		}
		if req.Op == proto.FSSymlink || req.Op == proto.FSMknod {
			return
		}
		resp := call(t, s, &req)
		if req.Op != proto.FSForget && resp == nil {
			t.Fatal("no response")
		}
	})
}
