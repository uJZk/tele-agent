package telefs

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func statSize(t *testing.T, p string) int64 {
	t.Helper()
	return lstat(t, p).Size
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// overwrite rewrites a backing file in place, keeping its inode.
func overwrite(t *testing.T, p, data string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestInvalidation changes the backing directory behind the mount's back
// with hour-long TTLs: only the pushed changes can make them visible, and
// the exec barrier (Sync, then WaitApplied) guarantees they are.
func TestInvalidation(t *testing.T) {
	h := newHarness(t, harnessOpts{ttl: time.Hour})
	writeFile(t, h.b("f"), "v1")
	mkdirAll(t, h.b("sub"))
	writeFile(t, h.b("sub/x"), "x1")
	h.sync()

	t.Run("content of an open file", func(t *testing.T) {
		fh, err := os.Open(h.m("f"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = fh.Close() }()
		buf := make([]byte, 64)
		n, _ := fh.ReadAt(buf, 0)
		if string(buf[:n]) != "v1" {
			t.Fatalf("first read %q", buf[:n])
		}
		overwrite(t, h.b("f"), "version 2")
		h.sync()
		n, _ = fh.ReadAt(buf, 0)
		if string(buf[:n]) != "version 2" {
			t.Fatalf("read after change %q", buf[:n])
		}
		if got := statSize(t, h.m("f")); got != 9 {
			t.Fatalf("size after change %d", got)
		}
	})

	t.Run("attributes", func(t *testing.T) {
		_ = lstat(t, h.m("f"))
		if err := os.Chmod(h.b("f"), 0o600); err != nil {
			t.Fatal(err)
		}
		h.sync()
		if st := lstat(t, h.m("f")); st.Mode&0o7777 != 0o600 {
			t.Fatalf("mode after chmod %o", st.Mode)
		}
	})

	t.Run("entries", func(t *testing.T) {
		if exists(h.m("new")) {
			t.Fatal("new exists before creation")
		}
		writeFile(t, h.b("new"), "n")
		h.sync()
		if got := readFile(t, h.m("new")); got != "n" {
			t.Fatalf("created entry reads %q", got)
		}
		if err := os.Rename(h.b("new"), h.b("renamed")); err != nil {
			t.Fatal(err)
		}
		h.sync()
		if exists(h.m("new")) || !exists(h.m("renamed")) {
			t.Fatal("rename not visible")
		}
		if err := os.Remove(h.b("renamed")); err != nil {
			t.Fatal(err)
		}
		h.sync()
		if exists(h.m("renamed")) {
			t.Fatal("removal not visible")
		}
		ents, err := os.ReadDir(h.m(""))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, h.b("listed"), "")
		h.sync()
		ents2, err := os.ReadDir(h.m(""))
		if err != nil {
			t.Fatal(err)
		}
		if len(ents2) != len(ents)+1 {
			t.Fatalf("listing has %d entries, want %d", len(ents2), len(ents)+1)
		}
	})

	// The checks below use lookups and lstat only: a read makes the kernel
	// drop the cached atime, so a stat after a read always asks the
	// server and would hide a missing invalidation.
	t.Run("moved directory", func(t *testing.T) {
		if got := statSize(t, h.m("sub/x")); got != 2 {
			t.Fatalf("sub/x size %d", got)
		}
		if err := os.Rename(h.b("sub"), h.b("sub2")); err != nil {
			t.Fatal(err)
		}
		h.sync()
		if exists(h.m("sub")) {
			t.Fatal("old path still resolves")
		}
		// Cache a negative entry and attributes under the new path, which
		// makes the server watch the directory there.
		if exists(h.m("sub2/y")) || statSize(t, h.m("sub2/x")) != 2 {
			t.Fatal("unexpected state of sub2")
		}
		writeFile(t, h.b("sub2/y"), "y")
		overwrite(t, h.b("sub2/x"), "x2 longer")
		h.sync()
		if got := statSize(t, h.m("sub2/x")); got != 9 {
			t.Fatalf("sub2/x size after change %d", got)
		}
		if !exists(h.m("sub2/y")) {
			t.Fatal("entry created in moved directory not visible")
		}
	})

	t.Run("local rename of a directory", func(t *testing.T) {
		mkdirAll(t, h.b("ld"))
		writeFile(t, h.b("ld/a"), "a1")
		h.sync()
		if got := statSize(t, h.m("ld/a")); got != 2 {
			t.Fatalf("ld/a size %d", got)
		}
		if err := os.Rename(h.m("ld"), h.m("ld2")); err != nil {
			t.Fatal(err)
		}
		h.sync()
		if exists(h.m("ld2/b")) || statSize(t, h.m("ld2/a")) != 2 {
			t.Fatal("unexpected state of ld2")
		}
		overwrite(t, h.b("ld2/a"), "a2 longer")
		writeFile(t, h.b("ld2/b"), "b")
		h.sync()
		if got := statSize(t, h.m("ld2/a")); got != 9 {
			t.Fatalf("ld2/a size after change %d", got)
		}
		if !exists(h.m("ld2/b")) {
			t.Fatal("entry created in renamed directory not visible")
		}
	})
}

// fakeWatch drives RunWatch with events the test writes itself.
type fakeWatch struct {
	t    *testing.T
	conn net.Conn
}

func startFakeWatch(h *harness) *fakeWatch {
	a, b := net.Pipe()
	h.runWatch(a)
	h.t.Cleanup(func() { _ = b.Close() })
	return &fakeWatch{t: h.t, conn: b}
}

func (w *fakeWatch) send(h *harness, ev proto.WatchEvent) {
	w.t.Helper()
	if err := proto.WriteFrame(w.conn, &ev); err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.fs.WaitApplied(ctx, ev.Seq); err != nil {
		w.t.Fatal(err)
	}
}

// TestEpochInvalidation checks that the cache really holds for the long
// TTL without events, and that a new epoch or a sequence gap invalidates
// everything.
func TestEpochInvalidation(t *testing.T) {
	h := newHarness(t, harnessOpts{noWatch: true, ttl: time.Hour})
	writeFile(t, h.b("f"), "ab")
	w := startFakeWatch(h)
	w.send(h, proto.WatchEvent{Seq: 1, Epoch: 7})

	if got := statSize(t, h.m("f")); got != 2 {
		t.Fatalf("size %d", got)
	}
	overwrite(t, h.b("f"), "abcdefghij")
	if got := statSize(t, h.m("f")); got != 2 {
		t.Fatalf("size %d without an event: the cache does not hold", got)
	}
	w.send(h, proto.WatchEvent{Seq: 2, Epoch: 7})
	if got := statSize(t, h.m("f")); got != 2 {
		t.Fatalf("size %d after an empty event", got)
	}
	w.send(h, proto.WatchEvent{Seq: 3, Epoch: 8})
	if got := statSize(t, h.m("f")); got != 10 {
		t.Fatalf("size %d after a new epoch", got)
	}

	overwrite(t, h.b("f"), "abc")
	w.send(h, proto.WatchEvent{Seq: 9, Epoch: 8})
	if got := statSize(t, h.m("f")); got != 3 {
		t.Fatalf("size %d after a sequence gap", got)
	}

	// Invalid events end the stream, which also invalidates everything
	// and falls back to the short TTL.
	writeFile(t, h.b("g"), "g")
	_ = lstat(t, h.m("f"))
	if err := proto.WriteFrame(w.conn, &proto.WatchEvent{Seq: 10, Epoch: 8, Changes: []proto.Change{{Dir: "relative", Kind: proto.ChangeEntry}}}); err != nil {
		t.Fatal(err)
	}
	if err := <-h.watchDone; err == nil {
		t.Fatal("RunWatch accepted an invalid event")
	}
	h.watchCancel = nil
	if h.fs.healthy.Load() {
		t.Fatal("still healthy after the stream ended")
	}
	if err := h.fs.WaitApplied(context.Background(), 11); !errors.Is(err, ErrWatchStopped) {
		t.Fatalf("WaitApplied after stream end = %v", err)
	}
}

// TestForgetRemovesWatch checks that when the kernel forgets a directory,
// the server stops watching it: changes in it no longer produce events.
func TestForgetRemovesWatch(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	mkdirAll(t, h.b("d"))
	h.sync()
	if exists(h.m("d/x")) {
		t.Fatal("d/x exists")
	}
	before := h.svc.Sync()
	writeFile(t, h.b("d/y"), "")
	if after := h.svc.Sync(); after == before {
		t.Fatal("no event for a change in a watched directory")
	}
	h.sync()

	// Drop the kernel's dentry of d; with nothing else referencing it,
	// the kernel forgets the inode.
	if errno := h.node("").NotifyEntry("d"); errno != 0 {
		t.Fatal(errno)
	}
	eventually(t, "the watch of /d to be released", func() bool {
		h.fs.reg.mu.Lock()
		defer h.fs.reg.mu.Unlock()
		_, registered := h.fs.reg.byPath["/d"]
		_, forgetting := h.fs.reg.forgetting["/d"]
		return !registered && !forgetting
	})
	before = h.svc.Sync()
	writeFile(t, h.b("d/z"), "")
	if after := h.svc.Sync(); after != before {
		t.Fatalf("event %d for a change in a forgotten directory", after)
	}
	// Looking it up again watches it again.
	if !exists(h.m("d/z")) {
		t.Fatal("d/z not visible")
	}
	before = h.svc.Sync()
	writeFile(t, h.b("d/w"), "")
	if after := h.svc.Sync(); after == before {
		t.Fatal("no event after the directory was looked up again")
	}
}

// TestNegativeEntries checks that a failed lookup is cached and invalidated
// when the entry appears.
func TestNegativeEntries(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	for range 3 {
		if exists(h.m("later")) {
			t.Fatal("exists too early")
		}
	}
	writeFile(t, h.b("later"), "now")
	h.sync()
	if got := readFile(t, h.m("later")); got != "now" {
		t.Fatalf("read %q", got)
	}
	// Hard links: a change is visible under every cached name.
	if err := os.Link(h.b("later"), h.b("alias")); err != nil {
		t.Fatal(err)
	}
	h.sync()
	names := []string{"later", "alias"}
	for _, n := range names {
		_ = lstat(t, h.m(n))
	}
	overwrite(t, h.b("later"), "much longer now")
	h.sync()
	for _, n := range names {
		if got := statSize(t, h.m(n)); got != int64(len("much longer now")) {
			t.Fatalf("%s size %d", n, got)
		}
	}
	if !slices.Contains(names, "alias") {
		t.Fatal("unreachable")
	}
	var st unix.Stat_t
	if err := unix.Stat(h.m("alias"), &st); err != nil || st.Nlink != 2 {
		t.Fatalf("nlink %d, %v", st.Nlink, err)
	}
}
