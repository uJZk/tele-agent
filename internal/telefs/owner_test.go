package telefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/fssvc"
	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/testutil/helperproc"
	"github.com/ujzk/tele-agent/internal/testutil/privtest"
)

func TestOwnerPresentation(t *testing.T) {
	const uid, gid = 12345, 23456
	h := newHarness(t, harnessOpts{uid: uid, gid: gid, placeholders: []Placeholder{{Path: "/ph/f"}}})
	writeFile(t, h.m("file"), "x")
	mkdirAll(t, h.m("dir"))
	if err := os.Symlink("file", h.m("link")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"", "file", "dir", "link", "ph", "ph/f"} {
		st := lstat(t, h.m(rel))
		if st.Uid != uid || st.Gid != gid {
			t.Errorf("%q presented as %d:%d, want %d:%d", rel, st.Uid, st.Gid, uid, gid)
		}
	}
	// The server creates files as its own user.
	if st := lstat(t, h.b("file")); st.Uid != uint32(os.Geteuid()) {
		t.Errorf("backing file owned by %d", st.Uid)
	}
	// Only a chown to the presented owner is accepted, as a no-op.
	if err := os.Lchown(h.m("file"), uid, gid); err != nil {
		t.Errorf("chown to presented owner: %v", err)
	}
	if err := os.Lchown(h.m("file"), 1, 1); !errors.Is(err, unix.EPERM) {
		t.Errorf("chown to another owner = %v, want EPERM", err)
	}
}

// TestRootOwnedWritable answers whether a FUSE mount without
// default_permissions, made by root, can be written when it presents every
// node as owned by root: it can, because uid 0 is mapped in the initial
// user namespace.
func TestRootOwnedWritable(t *testing.T) {
	h := newHarness(t, harnessOpts{uid: 0, gid: 0})
	writeFile(t, h.m("f"), "a")
	f, err := os.OpenFile(h.m("f"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("b"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	mkdirAll(t, h.m("d/e"))
	if err := os.Rename(h.m("f"), h.m("d/e/g")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.b("d/e/g")); got != "ab" {
		t.Fatalf("backing = %q", got)
	}
	if st := lstat(t, h.m("")); st.Uid != 0 || st.Gid != 0 {
		t.Fatalf("root presented as %d:%d", st.Uid, st.Gid)
	}
}

// userNSUID is the uid of the userns helper inside its namespace; host
// root maps to it, like an unprivileged user mapped to itself.
const userNSUID = 1000

// TestUserNSOwner mounts telefs in a user namespace where uid 0 is
// unmapped, like session main does (docs/filesystem.md section 4), and
// checks the owner contract: nodes presented with the caller's uid are
// writable; nodes presented with an unmapped owner are not (the kernel's
// HAS_UNMAPPED_ID check); and the root inode is writable only after the
// mount point was stat'ed once (docs/filesystem.md section 6).
func TestUserNSOwner(t *testing.T) {
	privtest.RequireRoot(t)
	privtest.RequireFUSE(t)
	privtest.RequireUserNS(t)
	cases := []struct {
		name        string
		presentUID  int
		refreshRoot bool
		want        syscall.Errno
	}{
		{"presented as the caller", userNSUID, true, 0},
		{"presented as unmapped root", 0, true, unix.EACCES},
		{"root inode not refreshed", userNSUID, false, unix.EACCES},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backing, mnt := t.TempDir(), t.TempDir()
			cmd := helperproc.Command(t, "userns-owner", backing, mnt, strconv.Itoa(tc.presentUID), strconv.FormatBool(tc.refreshRoot))
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
				UidMappings: []syscall.SysProcIDMap{{ContainerID: userNSUID, HostID: 0, Size: 1}},
				GidMappings: []syscall.SysProcIDMap{{ContainerID: userNSUID, HostID: 0, Size: 1}},
				AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN},
			}
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Run(); err != nil {
				t.Fatalf("helper: %v\n%s", err, out.String())
			}
			got := strings.TrimSpace(out.String())
			if want := fmt.Sprintf("create=%d", int(tc.want)); got != want {
				t.Fatalf("helper reported %q, want %q", got, want)
			}
		})
	}
}

// userNSOwnerHelper runs inside the user namespace: it serves args[0] with
// fssvc, mounts telefs on args[1] presenting uid/gid args[2], and reports
// the errno of creating a file in the mount's root.
func userNSOwnerHelper(args []string) int {
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: backing mountpoint uid refresh")
		return 2
	}
	uid, err := strconv.Atoi(args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	refresh, err := strconv.ParseBool(args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	errno, err := userNSCreate(args[0], args[1], uint32(uid), refresh)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("create=%d\n", int(errno))
	return 0
}

func userNSCreate(backing, mnt string, uid uint32, refresh bool) (syscall.Errno, error) {
	svc, err := fssvc.New(fssvc.Config{Root: backing})
	if err != nil {
		return 0, err
	}
	defer func() { _ = svc.Close() }()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	ca, err := fileConn(fds[0])
	if err != nil {
		return 0, err
	}
	cb, err := fileConn(fds[1])
	if err != nil {
		return 0, err
	}
	cli, err := mux.Client(ca)
	if err != nil {
		return 0, err
	}
	defer func() { _ = cli.Close() }()
	srv, err := mux.Server(cb)
	if err != nil {
		return 0, err
	}
	defer func() { _ = srv.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			st, err := srv.Accept()
			if err != nil {
				return
			}
			go func() {
				if kind, err := mux.ReadKind(st); err == nil && kind == proto.StreamFS {
					_ = svc.ServeRequest(ctx, st)
					return
				}
				_ = st.Close()
			}()
		}
	}()

	f, err := mount(mnt, Config{Opener: cli, UID: uid, GID: uid, AttrTimeout: time.Hour, EntryTimeout: time.Hour}, refresh)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Unmount() }()
	fd, err := unix.Open(mnt+"/created", unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o644)
	if err == nil {
		_ = unix.Close(fd)
		return 0, nil
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return 0, err
	}
	return errno, nil
}
