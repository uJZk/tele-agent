package fssvc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

// watchMask selects the inotify events that invalidate client caches
// (docs/telefs.md "变更监视"). Symlinks are followed on purpose: a client
// directory reached through a symlinked ancestor is watched at its target.
// IN_EXCL_UNLINK drops events of files that were unlinked but stay open,
// which no client can see any more.
const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
	unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_ATTRIB |
	unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_ONLYDIR | unix.IN_EXCL_UNLINK

// Publishing limits.
const (
	// coalesceWindow is how long the background reader collects changes
	// before publishing them as one event. Sync publishes at once.
	coalesceWindow = 10 * time.Millisecond
	// maxEventBytes bounds the encoded changes of one event, so that every
	// event fits in proto.MaxControlFrame; beyond it the pending changes
	// are published early.
	maxEventBytes = 256 << 10
	// changeOverhead approximates the encoded size of a Change without
	// its strings.
	changeOverhead = 16
	// maxOutboxBytes bounds the published events waiting for a watch
	// stream. Beyond it they are replaced by a new epoch, which tells the
	// client to invalidate everything instead.
	maxOutboxBytes = 8 << 20
	// maxOrphans bounds the events kept for descriptors that are not
	// recorded yet (watchState.orphans). Beyond it they are dropped and a
	// new epoch declares the loss.
	maxOrphans = 4096
)

// inotifyEvent is one decoded inotify event.
type inotifyEvent struct {
	wd   int32
	mask uint32
	name string
}

// watchState holds the inotify watches and the published events. It is
// guarded by Service.mu.
type watchState struct {
	closed bool

	// watches maps a watch descriptor to the remote-view paths it was
	// added under. One directory can be reached under several paths
	// (symlinks, bind mounts), and inotify returns the same descriptor for
	// the same inode, so events are reported under every path.
	watches map[int32]map[string]struct{}
	byPath  map[string]int32

	// adding counts the inotify_add_watch calls in flight. They run
	// without locks, so the reader can see events of a new descriptor
	// before watch records it; while adding > 0, events of unknown
	// descriptors are kept in orphans, and watch replays those of its
	// descriptor after recording it. Once no add is in flight, the
	// remaining orphans belong to removed watches and are dropped.
	adding  int
	orphans []inotifyEvent

	pending      []proto.Change
	pendingSet   map[proto.Change]struct{}
	pendingBytes int
	pendingSince time.Time
	// epochDirty forces a publish so that the client learns a new epoch.
	epochDirty bool

	seq   uint64
	epoch uint64

	outbox      []proto.WatchEvent
	outboxBytes int

	// The active watch stream: its generation, cancel function, and the
	// channel that wakes it when events are published. Each stream has
	// its own channel so that a replaced stream cannot swallow a wakeup.
	streamGen    uint64
	streamCancel context.CancelFunc
	streamWake   chan struct{}
}

func (w *watchState) init(epoch uint64) {
	w.watches = make(map[int32]map[string]struct{})
	w.byPath = make(map[string]int32)
	w.pendingSet = make(map[proto.Change]struct{})
	w.epoch = epoch
}

// watch makes sure directory p is watched and reports whether it is.
//
// inotify_add_watch resolves p, which blocks for as long as the file
// system below p does (a hung NFS server, an automount, a slow FUSE mount).
// It therefore runs without any lock, so that it stalls only the request
// that names p, not the reader, Sync (the exec barrier) or requests for
// other directories.
func (s *Service) watch(p string) bool {
	s.mu.Lock()
	_, ok := s.w.byPath[p]
	closed := s.w.closed
	if !ok && !closed {
		s.w.adding++
	}
	s.mu.Unlock()
	if ok {
		return true
	}
	if closed {
		return false
	}
	wd, err := s.addWatch(p)
	return s.recordWatch(p, wd, err)
}

// addWatch asks the kernel to watch real directory p. It holds no lock;
// the caller counted it in watchState.adding.
func (s *Service) addWatch(p string) (int32, error) {
	var (
		wd  int
		err error
	)
	cerr := s.rawIn.Control(func(fd uintptr) {
		wd, err = unix.InotifyAddWatch(int(fd), s.real(p), watchMask)
	})
	if cerr != nil {
		return 0, cerr
	}
	return int32(wd), err
}

// recordWatch finishes an addWatch of p that returned wd and err: it
// records the watch, replays the events the reader kept for wd in the
// meantime, and reports whether p is watched.
func (s *Service) recordWatch(p string, wd int32, err error) bool {
	var toRemove []int32
	defer func() {
		for _, wd := range toRemove {
			s.rmWatch(wd)
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w.adding--
	defer func() {
		if s.w.adding == 0 {
			s.w.orphans = nil
		}
	}()
	if err != nil {
		// ENOENT and ENOTDIR fail the request itself. EACCES (a directory
		// with search but no read permission) and ENOSPC (the watch
		// limit) are reported to the client as Unwatched.
		s.logDebug("fssvc: inotify_add_watch", "path", p, "err", err)
		return false
	}
	if s.w.closed {
		return false
	}
	paths := s.w.watches[wd]
	if paths == nil {
		paths = make(map[string]struct{})
		s.w.watches[wd] = paths
	}
	paths[p] = struct{}{}
	s.w.byPath[p] = wd

	// Replay in order. An event that ends the watch (IN_IGNORED after a
	// concurrent unwatch of another path of the same directory removed
	// the kernel's mark) drops p again, and the client hears about it.
	var mine []inotifyEvent
	kept := s.w.orphans[:0]
	for _, ev := range s.w.orphans {
		if ev.wd == wd {
			mine = append(mine, ev)
		} else {
			kept = append(kept, ev)
		}
	}
	s.w.orphans = kept
	for _, ev := range mine {
		toRemove = s.processEventLocked(ev, toRemove)
	}
	_, ok := s.w.byPath[p]
	return ok
}

// unwatch stops watching directory p (FSForget).
func (s *Service) unwatch(p string) {
	s.mu.Lock()
	wd, last := s.forgetPathLocked(p)
	s.mu.Unlock()
	if last {
		s.rmWatch(wd)
	}
}

// forgetPathLocked removes path p from the watch tables and reports its
// descriptor and whether p was the last path of it.
func (s *Service) forgetPathLocked(p string) (int32, bool) {
	wd, ok := s.w.byPath[p]
	if !ok {
		return 0, false
	}
	delete(s.w.byPath, p)
	paths := s.w.watches[wd]
	delete(paths, p)
	if len(paths) > 0 {
		return wd, false
	}
	delete(s.w.watches, wd)
	return wd, true
}

// rmWatch removes a watch from the kernel. A concurrent watch of another
// path of the same directory may have been handed the same descriptor and
// record it after the removal; the IN_IGNORED event that the removal queues
// then drops that path again (processEventLocked), so the client learns
// that it is not watched instead of waiting for events that never come.
func (s *Service) rmWatch(wd int32) {
	_ = s.rawIn.Control(func(fd uintptr) {
		// EINVAL means the kernel already dropped the watch.
		_, _ = unix.InotifyRmWatch(int(fd), uint32(wd))
	})
}

// readLoop drains the inotify descriptor whenever it becomes readable and
// publishes pending changes once they are coalesceWindow old. It exits when
// the descriptor is closed.
func (s *Service) readLoop() {
	defer s.wg.Done()
	for {
		if err := s.inotify.SetReadDeadline(s.flushDeadline()); err != nil {
			return
		}
		err := s.waitReadable()
		switch {
		case err == nil:
			s.readMu.Lock()
			s.drainLocked()
			s.readMu.Unlock()
		case errors.Is(err, os.ErrDeadlineExceeded):
			s.mu.Lock()
			s.publishLocked()
			s.mu.Unlock()
		default:
			return
		}
	}
}

// flushDeadline is when the pending changes must be published, or the zero
// time (no deadline) if nothing is pending.
func (s *Service) flushDeadline() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.w.pending) == 0 && !s.w.epochDirty {
		return time.Time{}
	}
	return s.w.pendingSince.Add(coalesceWindow)
}

// waitReadable blocks until the inotify descriptor is readable, the read
// deadline passes, or the descriptor is closed. It reads nothing itself:
// reading happens under readMu so that Sync sees a consistent queue.
func (s *Service) waitReadable() error {
	polled := false
	return s.rawIn.Read(func(uintptr) bool {
		if polled {
			return true
		}
		polled = true
		return false
	})
}

// drainLocked reads every queued inotify event and turns it into pending
// changes. The caller holds readMu.
func (s *Service) drainLocked() {
	var toRemove []int32
	for {
		var (
			n   int
			err error
		)
		cerr := s.rawIn.Control(func(fd uintptr) {
			err = ignoringEINTR(func() error {
				var err error
				n, err = unix.Read(int(fd), s.evBuf)
				return err
			})
		})
		if cerr != nil || errors.Is(err, unix.EAGAIN) {
			break
		}
		if err != nil || n <= 0 {
			// A failing inotify descriptor loses events: tell the client.
			s.log.Warn("fssvc: read inotify", "err", err)
			s.mu.Lock()
			s.bumpEpochLocked()
			s.mu.Unlock()
			break
		}
		s.mu.Lock()
		toRemove = s.processEventsLocked(s.evBuf[:n], toRemove)
		s.mu.Unlock()
	}
	for _, wd := range toRemove {
		s.rmWatch(wd)
	}
}

// processEventsLocked turns a buffer of inotify events into pending changes
// and returns the watches to remove from the kernel. The caller holds mu.
func (s *Service) processEventsLocked(buf []byte, toRemove []int32) []int32 {
	for len(buf) >= unix.SizeofInotifyEvent {
		wd := int32(binary.NativeEndian.Uint32(buf[0:4]))
		mask := binary.NativeEndian.Uint32(buf[4:8])
		nameLen := int(binary.NativeEndian.Uint32(buf[12:16]))
		end := unix.SizeofInotifyEvent + nameLen
		if end > len(buf) {
			break
		}
		name := buf[unix.SizeofInotifyEvent:end]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		toRemove = s.processEventLocked(inotifyEvent{wd: wd, mask: mask, name: string(name)}, toRemove)
		buf = buf[end:]
	}
	return toRemove
}

func (s *Service) processEventLocked(ev inotifyEvent, toRemove []int32) []int32 {
	wd, mask, name := ev.wd, ev.mask, ev.name
	if mask&unix.IN_Q_OVERFLOW != 0 {
		s.bumpEpochLocked()
		return toRemove
	}
	paths, ok := s.w.watches[wd]
	if !ok {
		// Either a watch that was removed in the meantime, or one that
		// an add in flight has not recorded yet.
		if s.w.adding > 0 {
			if len(s.w.orphans) >= maxOrphans {
				s.w.orphans = nil
				s.bumpEpochLocked()
			}
			s.w.orphans = append(s.w.orphans, ev)
		}
		return toRemove
	}
	if mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF|unix.IN_UNMOUNT|unix.IN_IGNORED) != 0 {
		// The directory is gone from these paths, and so is every
		// directory watched below them: the paths of their watches are
		// stale now. The client re-establishes watches when it looks the
		// directories up again.
		for _, p := range slices.Sorted(maps.Keys(paths)) {
			toRemove = s.dropTreeLocked(p, toRemove)
		}
		return toRemove
	}
	var kinds []proto.ChangeKind
	if mask&(unix.IN_CREATE|unix.IN_DELETE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO) != 0 {
		kinds = append(kinds, proto.ChangeEntry)
	}
	if mask&(unix.IN_MODIFY|unix.IN_CLOSE_WRITE) != 0 {
		kinds = append(kinds, proto.ChangeContent)
	}
	if mask&unix.IN_ATTRIB != 0 {
		kinds = append(kinds, proto.ChangeAttr)
	}
	for p := range paths {
		for _, k := range kinds {
			s.addChangeLocked(proto.Change{Dir: p, Name: name, Kind: k})
		}
	}
	return toRemove
}

// dropTreeLocked stops watching path p and every watched path below it,
// reporting each as gone, and returns descriptors left without paths.
func (s *Service) dropTreeLocked(p string, toRemove []int32) []int32 {
	prefix := p + "/"
	if p == "/" {
		prefix = "/"
	}
	var drop []string
	for q := range s.w.byPath {
		if q == p || strings.HasPrefix(q, prefix) {
			drop = append(drop, q)
		}
	}
	for _, q := range drop {
		s.addChangeLocked(proto.Change{Dir: q, Kind: proto.ChangeEntry})
		if wd, last := s.forgetPathLocked(q); last {
			toRemove = append(toRemove, wd)
		}
	}
	return toRemove
}

// addChangeLocked queues a change unless an identical one is pending.
func (s *Service) addChangeLocked(c proto.Change) {
	if _, dup := s.w.pendingSet[c]; dup {
		return
	}
	size := changeOverhead + len(c.Dir) + len(c.Name)
	if s.w.pendingBytes+size > maxEventBytes {
		s.publishLocked()
	}
	if len(s.w.pending) == 0 && !s.w.epochDirty {
		s.w.pendingSince = time.Now()
	}
	s.w.pending = append(s.w.pending, c)
	s.w.pendingSet[c] = struct{}{}
	s.w.pendingBytes += size
}

// bumpEpochLocked declares that events were lost: pending changes are moot
// because the client will invalidate everything.
func (s *Service) bumpEpochLocked() {
	s.w.epoch++
	s.w.pending = nil
	clear(s.w.pendingSet)
	s.w.pendingBytes = 0
	if !s.w.epochDirty {
		s.w.pendingSince = time.Now()
	}
	s.w.epochDirty = true
}

// publishLocked turns the pending changes into the next event.
func (s *Service) publishLocked() {
	if len(s.w.pending) == 0 && !s.w.epochDirty {
		return
	}
	s.w.seq++
	ev := proto.WatchEvent{Seq: s.w.seq, Epoch: s.w.epoch, Changes: s.w.pending}
	size := s.w.pendingBytes + changeOverhead
	s.w.pending = nil
	clear(s.w.pendingSet)
	s.w.pendingBytes = 0
	s.w.epochDirty = false

	if s.w.outboxBytes+size > maxOutboxBytes {
		// No stream drains the outbox fast enough. Replace everything
		// queued by a new epoch: the client invalidates everything, and
		// the sequence gap makes it do so even for this epoch number.
		s.w.epoch++
		ev = proto.WatchEvent{Seq: s.w.seq, Epoch: s.w.epoch}
		s.w.outbox = nil
		s.w.outboxBytes = 0
		size = changeOverhead
	}
	s.w.outbox = append(s.w.outbox, ev)
	s.w.outboxBytes += size
	if s.w.streamWake != nil {
		select {
		case s.w.streamWake <- struct{}{}:
		default:
		}
	}
}

// Sync reads every event queued by the kernel now, publishes them together
// with the pending changes as one WatchEvent, and returns the sequence
// number of the last published event. It is the exec barrier: every change
// made before the call is covered by events with Seq <= the result
// (docs/exec.md "exec 屏障").
func (s *Service) Sync() uint64 {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.drainLocked()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishLocked()
	return s.w.seq
}

// ServeWatch pushes WatchEvents on c until ctx ends, the peer closes the
// stream, the service is closed, or another ServeWatch call takes over: at
// most one watch stream is active, and a new one replaces the old. Every
// stream starts with a new epoch because the server cannot know what the
// previous stream delivered.
func (s *Service) ServeWatch(ctx context.Context, c net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.w.closed {
		s.mu.Unlock()
		cancel()
		_ = c.Close()
		return ErrClosed
	}
	if s.w.streamCancel != nil {
		s.w.streamCancel()
	}
	s.w.streamGen++
	gen := s.w.streamGen
	s.w.streamCancel = cancel
	wake := make(chan struct{}, 1)
	s.w.streamWake = wake
	s.w.outbox = nil
	s.w.outboxBytes = 0
	s.bumpEpochLocked()
	s.publishLocked()
	s.mu.Unlock()

	// The client never writes on this stream; reading detects its close.
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		_, _ = io.Copy(io.Discard, c)
		cancel()
	}()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer func() {
		stop()
		cancel()
		_ = c.Close()
		<-peerDone
		s.mu.Lock()
		if s.w.streamGen == gen {
			s.w.streamCancel = nil
			s.w.streamWake = nil
		}
		s.mu.Unlock()
	}()

	for {
		s.mu.Lock()
		if s.w.streamGen != gen {
			s.mu.Unlock()
			return nil
		}
		evs := s.w.outbox
		s.w.outbox = nil
		s.w.outboxBytes = 0
		s.mu.Unlock()
		for i := range evs {
			if err := proto.WriteFrame(c, &evs[i]); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("fssvc: write watch event: %w", err)
			}
		}
		if len(evs) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		}
	}
}
