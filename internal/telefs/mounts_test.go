package telefs

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// mounted reports whether a mount sits on path p in this mount namespace.
func mounted(t *testing.T, p string) bool {
	t.Helper()
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Field 5 is the mount point (octal escapes only for whitespace,
		// which the test paths do not contain).
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && fields[4] == p {
			return true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

// bindMount bind-mounts src onto dst and removes it when the test ends if
// it is still there.
func bindMount(t *testing.T, src, dst string) {
	t.Helper()
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind %s on %s: %v", src, dst, err)
	}
	t.Cleanup(func() {
		for mounted(t, dst) {
			if err := unix.Unmount(dst, unix.MNT_DETACH); err != nil {
				t.Errorf("unmount %s: %v", dst, err)
				return
			}
		}
	})
	if !mounted(t, dst) {
		t.Fatalf("bind mount on %s not in mountinfo", dst)
	}
}

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestEntryInvalidationDetachesMounts records the kernel behavior that
// makes placeholders necessary (docs/filesystem.md section 6): a FUSE entry
// invalidation (FUSE_NOTIFY_INVAL_ENTRY, go-fuse NotifyEntry) runs
// d_invalidate, which detaches every mount on the dentry or below it. If
// this test starts failing, the kernel changed and the restriction in
// docs/telefs.md section 4 may be lifted.
func TestEntryInvalidationDetachesMounts(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	mkdirAll(t, h.b("a/b"))
	mkdirAll(t, h.b("c/d"))
	src := t.TempDir()
	writeFile(t, src+"/marker", "local")

	cases := []struct {
		name   string
		target string // bind mount point, relative to the mount
		parent string // directory whose entry is invalidated
		entry  string
	}{
		{name: "entry of the mount point", target: "a/b", parent: "a", entry: "b"},
		{name: "entry of an ancestor", target: "c/d", parent: "", entry: "c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := h.m(tc.target)
			bindMount(t, src, dst)
			if got := readFile(t, dst+"/marker"); got != "local" {
				t.Fatalf("marker through bind mount = %q", got)
			}
			if errno := h.node(tc.parent).NotifyEntry(tc.entry); errno != 0 {
				t.Fatalf("NotifyEntry: %v", errno)
			}
			if mounted(t, dst) {
				t.Fatalf("mount on %s survived an entry invalidation of %q", tc.target, tc.entry)
			}
		})
	}
}

// TestPlaceholderMountsSurvive checks that nothing telefs does on its own
// detaches mounts on placeholders: remote changes around and above them,
// full invalidations, and revalidation while the server is unreachable.
func TestPlaceholderMountsSurvive(t *testing.T) {
	h := newHarness(t, harnessOpts{placeholders: []Placeholder{
		{Path: "/p/q", Dir: true},
		{Path: "/p/f"},
		{Path: "/top", Dir: true},
	}})
	src := t.TempDir()
	writeFile(t, src+"/marker", "local")
	srcFile := t.TempDir() + "/file"
	writeFile(t, srcFile, "local file")

	binds := map[string]string{h.m("p/q"): src, h.m("p/f"): srcFile, h.m("top"): src}
	for dst, s := range binds {
		bindMount(t, s, dst)
	}
	check := func(stage string) {
		t.Helper()
		for dst := range binds {
			if !mounted(t, dst) {
				t.Fatalf("%s: mount on %s detached", stage, dst)
			}
		}
		if got := readFile(t, h.m("p/q/marker")); got != "local" {
			t.Fatalf("%s: marker = %q", stage, got)
		}
		if got := readFile(t, h.m("p/f")); got != "local file" {
			t.Fatalf("%s: bound file = %q", stage, got)
		}
	}

	// Look around so that the kernel caches entries next to the
	// placeholders and the server watches their directories.
	mkdirAll(t, h.b("p"))
	writeFile(t, h.b("p/other"), "x")
	h.sync()
	if _, err := os.ReadDir(h.m("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(h.m("p")); err != nil {
		t.Fatal(err)
	}
	check("setup")

	steps := []struct {
		name string
		do   func()
	}{
		{"create entries with placeholder names", func() {
			mkdirAll(t, h.b("p/q"))
			writeFile(t, h.b("p/f"), "remote")
			mkdirAll(t, h.b("top"))
		}},
		{"remove them", func() {
			for _, p := range []string{"p/q", "p/f", "top"} {
				if err := os.RemoveAll(h.b(p)); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"change the ancestor's attributes", func() {
			if err := os.Chmod(h.b("p"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"move the ancestor away", func() {
			if err := os.Rename(h.b("p"), h.b("p2")); err != nil {
				t.Fatal(err)
			}
		}},
		{"move it back", func() {
			if err := os.Rename(h.b("p2"), h.b("p")); err != nil {
				t.Fatal(err)
			}
		}},
		{"remove the ancestor", func() {
			if err := os.RemoveAll(h.b("p")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, st := range steps {
		st.do()
		h.sync()
		check(st.name)
		// Walk the directories again so that the next step invalidates
		// freshly cached entries.
		_, _ = os.ReadDir(h.m(""))
		_, _ = os.ReadDir(h.m("p"))
	}

	// A new watch stream starts a new epoch; ending one invalidates
	// everything too.
	h.stopWatch()
	check("watch stream ended")
	h.startWatch()
	check("new epoch")
}

// TestPlaceholderRevalidationWithoutServer checks that the kernel's
// revalidation of placeholder paths never fails, even when the server is
// unreachable: a failed LOOKUP invalidates the dentry and detaches the
// mounts on it. Without a watch stream every entry has the short TTL, so
// the kernel revalidates the path within the loop.
func TestPlaceholderRevalidationWithoutServer(t *testing.T) {
	h := newHarness(t, harnessOpts{noWatch: true, placeholders: []Placeholder{{Path: "/p/q", Dir: true}}})
	mkdirAll(t, h.b("p"))
	src := t.TempDir()
	writeFile(t, src+"/marker", "local")
	bindMount(t, src, h.m("p/q"))

	h.opener.broken.Store(true)
	defer h.opener.broken.Store(false)
	end := time.Now().Add(2*shortTTL + shortTTL/2)
	for time.Now().Before(end) {
		if _, err := os.Stat(h.m("p/q/marker")); err != nil {
			t.Fatalf("stat through placeholder with the server unreachable: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !mounted(t, h.m("p/q")) {
		t.Fatal("mount on placeholder detached")
	}
	if _, err := os.Stat(h.m("p/other")); err == nil {
		t.Fatal("remote lookup succeeded with the transport broken")
	}
}
