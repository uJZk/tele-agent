package telefs

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/ujzk/tele-agent/internal/proto"
)

// forgetWorkers bounds concurrent FSForget round trips.
const forgetWorkers = 4

// registry records which nodes asked the server to watch which remote
// directory paths (docs/telefs.md "变更监视"). The server watches by path,
// so a path is forgotten only when no live node needs it any more, and a
// forget that was sent is ordered before any later request that watches
// the same path again: otherwise the server could process the new watch
// first and then drop it. A forget still queued when the path is needed
// again is cancelled instead: the server keeps the watch the new request
// relies on.
//
// A forget is bounded by the transport like every request (docs/telefs.md
// "断线与恢复"), not by a timeout of its own: once sent, its outcome is
// unknown until the server confirms it, and a request that watches the
// path again must not overtake it.
type registry struct {
	mu     sync.Mutex // guards the fields below and node.watchPaths
	byPath map[string]map[*node]struct{}
	// forgetting holds the paths with a queued or sent forget; the
	// channel is closed when the forget completed or was cancelled.
	forgetting map[string]chan struct{}
	queue      []string      // forgets not sent yet, in order
	kick       chan struct{} // wakes a forget worker; capacity 1
}

func (r *registry) init() {
	r.byPath = make(map[string]map[*node]struct{})
	r.forgetting = make(map[string]chan struct{})
	r.kick = make(chan struct{}, 1)
}

// watchDir records that n is about to send a request that makes the server
// of backend b watch directory p. It must be called before the request is
// sent. Local backends watch nothing.
func (f *FS) watchDir(n *node, b *backend, p string) {
	if b.local {
		return
	}
	r := &f.reg
	r.mu.Lock()
	for {
		ch, busy := r.forgetting[p]
		if !busy {
			break
		}
		if i := slices.Index(r.queue, p); i >= 0 {
			r.queue = slices.Delete(r.queue, i, i+1)
			delete(r.forgetting, p)
			close(ch)
			break
		}
		r.mu.Unlock()
		select {
		case <-ch:
		case <-f.done:
		}
		r.mu.Lock()
		if isDone(f.done) {
			break
		}
	}
	set := r.byPath[p]
	if set == nil {
		set = make(map[*node]struct{})
		r.byPath[p] = set
	}
	set[n] = struct{}{}
	if n.watchPaths == nil {
		n.watchPaths = make(map[string]struct{})
	}
	n.watchPaths[p] = struct{}{}
	r.mu.Unlock()
}

// releaseWatches drops the registrations of a forgotten node and queues an
// FSForget for every path no other node needs.
func (f *FS) releaseWatches(n *node) {
	r := &f.reg
	r.mu.Lock()
	queued := false
	for p := range n.watchPaths {
		set := r.byPath[p]
		delete(set, n)
		if len(set) > 0 {
			continue
		}
		delete(r.byPath, p)
		if _, busy := r.forgetting[p]; busy {
			continue
		}
		r.forgetting[p] = make(chan struct{})
		r.queue = append(r.queue, p)
		queued = true
	}
	n.watchPaths = nil
	r.mu.Unlock()
	if queued {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// watchers returns the nodes registered for remote directory p.
func (f *FS) watchers(p string) []*node {
	r := &f.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*node, 0, len(r.byPath[p]))
	for n := range r.byPath[p] {
		out = append(out, n)
	}
	return out
}

// unregister removes every registration of p, because the server stopped
// watching it on its own, and returns the nodes that had it.
func (f *FS) unregister(p string) []*node {
	r := &f.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.byPath[p]
	delete(r.byPath, p)
	out := make([]*node, 0, len(set))
	for n := range set {
		delete(n.watchPaths, p)
		out = append(out, n)
	}
	return out
}

func (f *FS) startForgetWorkers() {
	// stop ends the in-flight forgets when the file system is unmounted.
	stopCtx, cancel := context.WithCancel(context.Background())
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		<-f.done
		cancel()
	}()
	for range forgetWorkers {
		f.wg.Add(1)
		go f.forgetWorker(stopCtx)
	}
}

// forgetWorker sends queued FSForgets until the file system is unmounted.
func (f *FS) forgetWorker(ctx context.Context) {
	defer f.wg.Done()
	r := &f.reg
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-r.kick:
			}
			continue
		}
		p := r.queue[0]
		r.queue = r.queue[1:]
		ch := r.forgetting[p]
		more := len(r.queue) > 0
		r.mu.Unlock()
		if more {
			select {
			case r.kick <- struct{}{}:
			default:
			}
		}
		f.sendForget(ctx, p)
		r.mu.Lock()
		delete(r.forgetting, p)
		r.mu.Unlock()
		close(ch)
	}
}

// sendForget tells the server to stop watching p and waits until it did:
// the server closes the stream after removing the watch. The wait ends
// early only when the file system is unmounted (ctx).
func (f *FS) sendForget(ctx context.Context, p string) {
	c, err := f.cfg.Opener.Open(proto.StreamFS)
	if err != nil {
		// Not sent: the server keeps watching p, which costs a watch
		// but loses no change.
		f.log.Debug("telefs: open forget stream", "err", err)
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() { _ = c.Close() }()
	if err := proto.WriteFrame(c, &proto.FSRequest{Op: proto.FSForget, Path: p}); err != nil {
		f.log.Debug("telefs: send forget", "err", err)
		return
	}
	_, _ = io.Copy(io.Discard, c)
}

// isDone reports whether ch is closed.
func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// RunWatch applies the WatchEvents the server pushes on c until c fails or
// ctx ends; only one RunWatch runs at a time. While it runs and events
// arrive, the kernel caches with the configured TTLs; when it returns,
// everything is invalidated and the short TTL applies again.
func (f *FS) RunWatch(ctx context.Context, c net.Conn) error {
	f.mu.Lock()
	if f.watchRunning {
		f.mu.Unlock()
		_ = c.Close()
		return ErrWatchRunning
	}
	f.watchRunning = true
	f.watchStopped = false
	f.mu.Unlock()

	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer func() {
		stop()
		_ = c.Close()
		f.watchEnded()
	}()
	for {
		var ev proto.WatchEvent
		if err := proto.ReadFrame(c, &ev, proto.MaxControlFrame); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("telefs: watch stream: %w", err)
		}
		if err := checkEvent(&ev); err != nil {
			return err
		}
		f.apply(&ev)
	}
}

// checkEvent validates an event received from the server.
func checkEvent(ev *proto.WatchEvent) error {
	for _, ch := range ev.Changes {
		if err := proto.CheckPath(ch.Dir); err != nil {
			return fmt.Errorf("telefs: watch event: %w", err)
		}
		if ch.Name != "" {
			if err := proto.CheckName(ch.Name); err != nil {
				return fmt.Errorf("telefs: watch event: %w", err)
			}
		}
		switch ch.Kind {
		case proto.ChangeEntry, proto.ChangeContent, proto.ChangeAttr:
		default:
			return fmt.Errorf("telefs: watch event: unknown change kind %d", ch.Kind)
		}
	}
	return nil
}

// apply invalidates what ev reports and records it as applied. A new epoch
// or a gap in the sequence means changes were lost: everything is
// invalidated.
func (f *FS) apply(ev *proto.WatchEvent) {
	f.invalGen.Add(1)
	f.mu.Lock()
	full := !f.haveEpoch || ev.Epoch != f.epoch || ev.Seq != f.applied+1
	f.mu.Unlock()
	if full {
		f.invalidateAll()
	} else {
		for _, ch := range ev.Changes {
			f.applyChange(ch)
		}
	}
	f.mu.Lock()
	f.haveEpoch, f.epoch, f.applied = true, ev.Epoch, ev.Seq
	f.healthy.Store(true)
	f.wakeLocked()
	f.mu.Unlock()
}

// watchEnded switches back to short TTLs: nothing pushes changes any more,
// so what the kernel cached for long may already be stale.
func (f *FS) watchEnded() {
	f.mu.Lock()
	f.watchRunning = false
	f.watchStopped = true
	f.healthy.Store(false)
	f.wakeLocked()
	f.mu.Unlock()
	f.invalGen.Add(1)
	f.invalidateAll()
}

// wakeLocked wakes every WaitApplied. The caller holds f.mu.
func (f *FS) wakeLocked() {
	close(f.appliedWake)
	f.appliedWake = make(chan struct{})
}

// WaitApplied waits until the watch event with sequence number seq has been
// applied, so that the kernel no longer serves anything it invalidated. It
// is the local half of the exec barrier (docs/exec.md "exec 屏障"). It fails
// with ErrWatchStopped if the watch stream ended first.
func (f *FS) WaitApplied(ctx context.Context, seq uint64) error {
	for {
		f.mu.Lock()
		done := f.applied >= seq
		stopped := f.watchStopped
		wake := f.appliedWake
		f.mu.Unlock()
		switch {
		case done:
			return nil
		case stopped:
			return ErrWatchStopped
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// applyChange invalidates what one change reports, in every node that
// watches the directory.
func (f *FS) applyChange(ch proto.Change) {
	if ch.Kind == proto.ChangeEntry && ch.Name == "" {
		for _, n := range f.unregister(ch.Dir) {
			f.dirGone(n)
		}
		return
	}
	for _, d := range f.watchers(ch.Dir) {
		if d.local.owns(ch.Name) {
			continue // the remote entry is not shown
		}
		switch {
		case ch.Kind == proto.ChangeEntry:
			f.invalEntry(d, ch.Name)
			// The directory's own mtime, ctime, size and nlink changed
			// too, but inotify reports only the entry, and the kernel
			// refreshes the directory's attributes on an entry
			// invalidation only if it had cached that name.
			notifyContent(&d.Inode, -1)
		case ch.Name == "":
			notifyContent(&d.Inode, -1)
		default:
			c := d.GetChild(ch.Name)
			if c == nil {
				// Nothing cached: the kernel has no inode for it.
				continue
			}
			off := int64(-1)
			if ch.Kind == proto.ChangeContent {
				off = 0
			}
			notifyContent(c, off)
		}
	}
}

// invalEntry invalidates entry name of d, unless it is a placeholder or an
// ancestor of one: an entry invalidation detaches every mount on or below
// the dentry (docs/filesystem.md "已知陷阱"), so those only get their
// attributes invalidated.
func (f *FS) invalEntry(d *node, name string) {
	if c := d.synth[name]; c != nil {
		notifyContent(&c.Inode, -1)
		return
	}
	if errno := d.NotifyEntry(name); errno != 0 && errno != syscall.ENOENT {
		f.log.Debug("telefs: entry invalidation", "errno", errno)
	}
}

// dirGone handles a directory the server no longer watches under the path
// n registered it with (removed or moved): its entries are invalidated so
// that the next lookups watch it again, and so is its own entry.
func (f *FS) dirGone(n *node) {
	notifyContent(&n.Inode, -1)
	for name := range n.Children() {
		f.invalEntry(n, name)
	}
	name, parent := n.Parent()
	if parent == nil {
		return
	}
	if pn, ok := parent.Operations().(*node); ok {
		f.invalEntry(pn, name)
	}
}

// invalidateAll drops everything the kernel caches, except the entries of
// placeholders and their ancestors.
func (f *FS) invalidateAll() {
	visited := map[*node]bool{}
	var walk func(d *node)
	walk = func(d *node) {
		notifyContent(&d.Inode, -1)
		for name, c := range d.Children() {
			f.invalEntry(d, name)
			cn, ok := c.Operations().(*node)
			if !ok {
				continue
			}
			if c.IsDir() {
				if !visited[cn] {
					visited[cn] = true
					walk(cn)
				}
				continue
			}
			off := int64(-1)
			if c.StableAttr().Mode == syscall.S_IFREG {
				off = 0
			}
			notifyContent(c, off)
		}
	}
	visited[f.root] = true
	walk(f.root)
}

// notifyContent invalidates the attributes of in and, with off >= 0, its
// cached pages from off on.
func notifyContent(in *fs.Inode, off int64) {
	// ENOENT means the kernel dropped the inode already. Other failures
	// leave the TTL as the only bound on staleness; there is nothing
	// better to do.
	_ = in.NotifyContent(off, 0)
}
