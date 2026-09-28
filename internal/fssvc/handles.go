package fssvc

import (
	"sync"

	"golang.org/x/sys/unix"
)

// handle is an open file or directory of the client. The descriptor is
// closed only when the handle was released and no request uses it any more,
// so that a concurrent FSRelease can never make a read hit a reused
// descriptor number.
type handle struct {
	fd  int
	dir bool
	// path is the remote-view path the directory was opened under; the
	// server keeps watching it while the client lists it.
	path string

	refs   int  // guarded by handleTable.mu
	closed bool // guarded by handleTable.mu

	// dmu serializes FSReaddir calls, which move the descriptor's position.
	dmu     sync.Mutex
	dirPos  int64    // guarded by dmu: cookie after the last entry returned
	pending []dirent // guarded by dmu: entries read but not returned yet
	dirBuf  []byte   // guarded by dmu
}

// handleTable holds the open files and directories of the service. File
// and directory handles share one ID space so that FSFsync can name either.
type handleTable struct {
	mu   sync.Mutex // guards next and m
	next uint64
	m    map[uint64]*handle
}

func (t *handleTable) init() {
	t.next = 1 // 0 means "no handle" on the wire
	t.m = make(map[uint64]*handle)
}

// add registers h and returns its ID.
func (t *handleTable) add(h *handle) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.next
	t.next++
	t.m[id] = h
	return id
}

// acquire returns the handle with the given ID and pins its descriptor
// until put is called.
func (t *handleTable) acquire(id uint64) (*handle, uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.m[id]
	if h == nil {
		return nil, uint32(unix.EBADF)
	}
	h.refs++
	return h, 0
}

// put releases a reference taken by acquire.
func (t *handleTable) put(h *handle) {
	t.mu.Lock()
	h.refs--
	last := h.closed && h.refs == 0
	t.mu.Unlock()
	if last {
		_ = unix.Close(h.fd)
	}
}

// release removes the handle; its descriptor is closed once unused.
func (t *handleTable) release(id uint64) uint32 {
	t.mu.Lock()
	h := t.m[id]
	if h == nil {
		t.mu.Unlock()
		return uint32(unix.EBADF)
	}
	delete(t.m, id)
	h.closed = true
	last := h.refs == 0
	t.mu.Unlock()
	if last {
		// close(2) errors on a released handle have no one to report to:
		// the kernel ignores the reply to RELEASE.
		_ = unix.Close(h.fd)
	}
	return 0
}

// closeAll releases every handle.
func (t *handleTable) closeAll() {
	t.mu.Lock()
	ids := make([]uint64, 0, len(t.m))
	for id := range t.m {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	for _, id := range ids {
		t.release(id)
	}
}

// count reports the number of open handles.
func (t *handleTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
