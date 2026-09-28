package fssvc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// watchClient reads the events of one watch stream.
type watchClient struct {
	t      *testing.T
	events chan proto.WatchEvent
	conn   net.Conn
	done   chan error // ServeWatch's result
	last   uint64     // highest sequence number received
}

func startWatch(t *testing.T, s *Service) *watchClient {
	t.Helper()
	a, b := net.Pipe()
	w := &watchClient{t: t, events: make(chan proto.WatchEvent, 1024), conn: a, done: make(chan error, 1)}
	go func() { w.done <- s.ServeWatch(context.Background(), b) }()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(w.events)
		for {
			var ev proto.WatchEvent
			if err := proto.ReadFrame(a, &ev, proto.MaxControlFrame); err != nil {
				return
			}
			w.events <- ev
		}
	}()
	t.Cleanup(func() {
		_ = a.Close()
		<-readDone
		<-w.done
	})
	return w
}

// next returns the next event.
func (w *watchClient) next() proto.WatchEvent {
	w.t.Helper()
	select {
	case ev, ok := <-w.events:
		if !ok {
			w.t.Fatal("watch stream ended")
		}
		w.last = max(w.last, ev.Seq)
		return ev
	case <-time.After(10 * time.Second):
		w.t.Fatal("no watch event")
		return proto.WatchEvent{}
	}
}

// until collects events up to and including sequence number seq.
func (w *watchClient) until(seq uint64) []proto.WatchEvent {
	w.t.Helper()
	var evs []proto.WatchEvent
	for w.last < seq {
		ev := w.next()
		evs = append(evs, ev)
	}
	return evs
}

func changes(evs []proto.WatchEvent) []proto.Change {
	var out []proto.Change
	for _, ev := range evs {
		out = append(out, ev.Changes...)
	}
	return out
}

func TestWatchEvents(t *testing.T) {
	s, root := newSvc(t)
	w := startWatch(t, s)
	first := w.next()
	if len(first.Changes) != 0 {
		t.Fatalf("first event carries changes %v", first.Changes)
	}
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d/f"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantErrno(t, call(t, s, &proto.FSRequest{Op: proto.FSLookup, Path: "/d", Name: "new"}), unix.ENOENT)

	p := func(rel string) string { return filepath.Join(root, rel) }
	if err := os.WriteFile(p("d/new"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := os.WriteFile(p("d/f"), []byte("22"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(p("d/f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p("d/new")); err != nil {
		t.Fatal(err)
	}
	seq := s.Sync()
	evs := w.until(seq)
	got := changes(evs)
	for _, want := range []proto.Change{
		{Dir: "/d", Name: "new", Kind: proto.ChangeEntry},
		{Dir: "/d", Name: "f", Kind: proto.ChangeContent},
		{Dir: "/d", Name: "f", Kind: proto.ChangeAttr},
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %+v in %+v", want, got)
		}
	}
	// Duplicates are coalesced within an event.
	for _, ev := range evs {
		seen := map[proto.Change]bool{}
		for _, c := range ev.Changes {
			if seen[c] {
				t.Errorf("duplicate %+v in event %d", c, ev.Seq)
			}
			seen[c] = true
		}
	}
	// Sequence numbers increase by one, within one epoch.
	prev := first
	for _, ev := range evs {
		if ev.Seq != prev.Seq+1 || ev.Epoch != prev.Epoch {
			t.Fatalf("event %d/%d after %d/%d", ev.Seq, ev.Epoch, prev.Seq, prev.Epoch)
		}
		prev = ev
	}
	// Nothing new: Sync publishes nothing and returns the last sequence.
	if again := s.Sync(); again != seq {
		t.Fatalf("idle Sync returned %d after %d", again, seq)
	}
}

// TestWatchBackground checks that changes are published without Sync.
func TestWatchBackground(t *testing.T) {
	s, root := newSvc(t)
	w := startWatch(t, s)
	w.next()
	ok(t, s, &proto.FSRequest{Op: proto.FSOpendir, Path: "/"})
	if err := os.WriteFile(filepath.Join(root, "bg"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ev := w.next()
	if !slices.Contains(ev.Changes, proto.Change{Dir: "/", Name: "bg", Kind: proto.ChangeEntry}) {
		t.Fatalf("background event %+v", ev)
	}
}

func TestWatchDirGone(t *testing.T) {
	s, root := newSvc(t)
	for _, d := range []string{"a/b/c", "keep"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := startWatch(t, s)
	w.next()
	for _, dir := range []string{"/", "/a", "/a/b", "/a/b/c", "/keep"} {
		call(t, s, &proto.FSRequest{Op: proto.FSLookup, Path: dir, Name: "x"})
	}
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	got := changes(w.until(s.Sync()))
	for _, want := range []proto.Change{
		{Dir: "/", Name: "a", Kind: proto.ChangeEntry},
		{Dir: "/", Name: "moved", Kind: proto.ChangeEntry},
		{Dir: "/a", Kind: proto.ChangeEntry},
		{Dir: "/a/b", Kind: proto.ChangeEntry},
		{Dir: "/a/b/c", Kind: proto.ChangeEntry},
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %+v in %+v", want, got)
		}
	}
	s.mu.Lock()
	_, stale := s.w.byPath["/a/b"]
	_, kept := s.w.byPath["/keep"]
	s.mu.Unlock()
	if stale || !kept {
		t.Fatalf("watches after move: /a/b %v, /keep %v", stale, kept)
	}
	// Changes below the moved directory are no longer reported under
	// the old paths.
	if err := os.WriteFile(filepath.Join(root, "moved/b/c/f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range changes(w.until(s.Sync())) {
		if strings.HasPrefix(c.Dir, "/a") {
			t.Fatalf("change %+v under a stale path", c)
		}
	}

	if err := os.Remove(filepath.Join(root, "keep")); err != nil {
		t.Fatal(err)
	}
	if got := changes(w.until(s.Sync())); !slices.Contains(got, proto.Change{Dir: "/keep", Kind: proto.ChangeEntry}) {
		t.Fatalf("removed directory not reported: %+v", got)
	}
}

func TestForgetStopsWatching(t *testing.T) {
	s, root := newSvc(t)
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	call(t, s, &proto.FSRequest{Op: proto.FSLookup, Path: "/d", Name: "x"})
	before := s.Sync()
	if err := os.WriteFile(filepath.Join(root, "d/a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if s.Sync() == before {
		t.Fatal("no event for a watched directory")
	}
	call(t, s, &proto.FSRequest{Op: proto.FSForget, Path: "/d"})
	before = s.Sync()
	if err := os.WriteFile(filepath.Join(root, "d/b"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if after := s.Sync(); after != before {
		t.Fatalf("event %d after forget", after)
	}
	// Forgetting an unwatched or invalid path is harmless.
	call(t, s, &proto.FSRequest{Op: proto.FSForget, Path: "/d"})
	call(t, s, &proto.FSRequest{Op: proto.FSForget, Path: "relative"})
}

// TestWatchSharedInode checks that a directory reached under two paths
// reports changes under both, and stays watched until both are forgotten.
func TestWatchSharedInode(t *testing.T) {
	s, root := newSvc(t)
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	w := startWatch(t, s)
	w.next()
	call(t, s, &proto.FSRequest{Op: proto.FSLookup, Path: "/real", Name: "x"})
	call(t, s, &proto.FSRequest{Op: proto.FSLookup, Path: "/alias", Name: "x"})
	if err := os.WriteFile(filepath.Join(root, "real/f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := changes(w.until(s.Sync()))
	for _, dir := range []string{"/real", "/alias"} {
		if !slices.Contains(got, proto.Change{Dir: dir, Name: "f", Kind: proto.ChangeEntry}) {
			t.Errorf("no change under %s: %+v", dir, got)
		}
	}
	call(t, s, &proto.FSRequest{Op: proto.FSForget, Path: "/real"})
	if err := os.WriteFile(filepath.Join(root, "real/g"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got = changes(w.until(s.Sync()))
	if !slices.Contains(got, proto.Change{Dir: "/alias", Name: "g", Kind: proto.ChangeEntry}) ||
		slices.Contains(got, proto.Change{Dir: "/real", Name: "g", Kind: proto.ChangeEntry}) {
		t.Fatalf("after forgetting one path: %+v", got)
	}
}

func TestUnwatched(t *testing.T) {
	s, _ := newSvc(t)
	resp := call(t, s, &proto.FSRequest{Op: proto.FSLookup, Path: "/missing", Name: "x"})
	if resp.Errno != uint32(unix.ENOENT) || !resp.Unwatched {
		t.Fatalf("errno %d unwatched %v", resp.Errno, resp.Unwatched)
	}
	if resp := ok(t, s, &proto.FSRequest{Op: proto.FSOpendir, Path: "/"}); resp.Unwatched {
		t.Fatal("root reported unwatched")
	}
}

// TestWatchStreamTakeover checks that a new watch stream replaces the old
// one and starts a new epoch.
func TestWatchStreamTakeover(t *testing.T) {
	s, _ := newSvc(t)
	w1 := startWatch(t, s)
	e1 := w1.next()
	w2 := startWatch(t, s)
	e2 := w2.next()
	if e2.Epoch == e1.Epoch || e2.Seq != e1.Seq+1 {
		t.Fatalf("second stream starts with %d/%d after %d/%d", e2.Seq, e2.Epoch, e1.Seq, e1.Epoch)
	}
	select {
	case err := <-w1.done:
		if err != nil {
			t.Fatalf("replaced stream: %v", err)
		}
		w1.done <- err // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("replaced stream still running")
	}
}

// TestOverflow lets the kernel's inotify queue overflow and checks that
// the loss is announced with a new epoch.
func TestOverflow(t *testing.T) {
	b, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events")
	if err != nil {
		t.Skipf("max_queued_events: %v", err)
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || limit > 1<<20 {
		t.Skipf("max_queued_events = %q", b)
	}
	root := t.TempDir()
	// No reader: nothing drains the queue until Sync.
	s, err := newService(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	f := filepath.Join(root, "f")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.watch("/") {
		t.Fatal("watch failed")
	}
	epoch := s.w.epoch
	fh, err := os.OpenFile(f, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	// Alternate event types: the kernel merges identical consecutive
	// events.
	for i := range limit/2 + 10 {
		if _, err := fh.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f, os.FileMode(0o600+i%2)); err != nil {
			t.Fatal(err)
		}
	}
	seq := s.Sync()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w.epoch == epoch {
		t.Fatal("no new epoch after overflow")
	}
	last := s.w.outbox[len(s.w.outbox)-1]
	if last.Seq != seq || last.Epoch != s.w.epoch {
		t.Fatalf("last event %d/%d, want %d/%d", last.Seq, last.Epoch, seq, s.w.epoch)
	}
}

// outboxChanges returns the changes of every published event not yet sent.
func outboxChanges(s *Service) []proto.Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	return changes(s.w.outbox)
}

// TestWatchEventsBeforeRecord interleaves an add by hand: the kernel
// already watches the directory, and reports a change, before the service
// recorded the descriptor. The change must not be lost.
func TestWatchEventsBeforeRecord(t *testing.T) {
	root := t.TempDir()
	// No reader: Sync drains the queue at chosen points.
	s, err := newService(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.w.adding++
	s.mu.Unlock()
	wd, err := s.addWatch("/d")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d/f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.Sync()
	want := proto.Change{Dir: "/d", Name: "f", Kind: proto.ChangeEntry}
	if slices.Contains(outboxChanges(s), want) {
		t.Fatal("change published before the watch was recorded")
	}
	if !s.recordWatch("/d", wd, nil) {
		t.Fatal("watch not recorded")
	}
	s.Sync()
	if got := outboxChanges(s); !slices.Contains(got, want) {
		t.Fatalf("change read before the record was lost: %+v", got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.w.orphans) != 0 || s.w.adding != 0 {
		t.Fatalf("orphans %v adding %d", s.w.orphans, s.w.adding)
	}
}

// TestWatchRemovedBeforeRecord interleaves an add of a second path of one
// directory with the unwatch of its first path: the add is handed the
// existing descriptor, whose kernel mark the unwatch then removes. The
// second path must end up reported as not watched rather than silently
// losing its events.
func TestWatchRemovedBeforeRecord(t *testing.T) {
	root := t.TempDir()
	s, err := newService(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("d", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if !s.watch("/d") {
		t.Fatal("watch failed")
	}
	s.mu.Lock()
	s.w.adding++
	first := s.w.byPath["/d"]
	s.mu.Unlock()
	wd, err := s.addWatch("/alias")
	if err != nil || wd != first {
		t.Fatalf("add of the second path: wd %d (first %d), %v", wd, first, err)
	}
	s.unwatch("/d")
	s.Sync() // reads IN_IGNORED before the record
	if s.recordWatch("/alias", wd, nil) {
		t.Fatal("second path reported watched after its mark was removed")
	}
	s.Sync()
	if got := outboxChanges(s); !slices.Contains(got, proto.Change{Dir: "/alias", Kind: proto.ChangeEntry}) {
		t.Fatalf("client not told: %+v", got)
	}
	s.mu.Lock()
	_, watched := s.w.byPath["/alias"]
	s.mu.Unlock()
	if watched {
		t.Fatal("second path still recorded")
	}
}

func FuzzProcessEvents(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, unix.SizeofInotifyEvent+4))
	s, _ := newSvc(f)
	f.Fuzz(func(_ *testing.T, b []byte) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = s.processEventsLocked(b, nil)
	})
}
