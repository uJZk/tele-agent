package execsvc

import (
	"fmt"
	"sync"

	"github.com/ujzk/tele-agent/internal/proto"
)

// stdinQueue passes stdin data from the frame reader to the goroutine
// that writes it to the command. The client keeps at most
// proto.ExecStdinWindow of stdin unacknowledged, so the queue is bounded
// without the reader ever waiting: signals and the end of the stream,
// which arrive behind stdin data, are handled at once however slowly the
// command reads (docs/exec.md "进程与信号").
type stdinQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond // signalled whenever a field below changes
	chunks [][]byte   // guarded by mu
	size   int        // guarded by mu; bytes in chunks
	eof    bool       // guarded by mu; the client ended its input
	closed bool       // guarded by mu; forwarding stopped, data is dropped
}

func newStdinQueue() *stdinQueue {
	q := &stdinQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// put queues data. Data that arrives after end or close is dropped. Data
// beyond the window is a protocol violation: the chunk the writer took
// last is not acknowledged yet either, so a client within its window
// never fills the queue beyond it.
func (q *stdinQueue) put(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.eof {
		return nil
	}
	if q.size+len(data) > proto.ExecStdinWindow {
		return fmt.Errorf("stdin beyond the window of %d bytes", proto.ExecStdinWindow)
	}
	q.chunks = append(q.chunks, data)
	q.size += len(data)
	q.cond.Broadcast()
	return nil
}

// end records the end of the client's input, after the data queued so
// far.
func (q *stdinQueue) end() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.eof = true
	q.cond.Broadcast()
}

// close stops forwarding: queued and later data is dropped, and next
// returns at once.
func (q *stdinQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.chunks, q.size = nil, 0
	q.cond.Broadcast()
}

// empty reports whether no chunk is queued.
func (q *stdinQueue) empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.chunks) == 0
}

// next waits for the next chunk. ok is false once the queue was closed,
// or drained after end; eof tells the two apart.
func (q *stdinQueue) next() (data []byte, eof, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.closed && len(q.chunks) == 0 && !q.eof {
		q.cond.Wait()
	}
	switch {
	case q.closed:
		return nil, false, false
	case len(q.chunks) == 0:
		return nil, true, false
	}
	data = q.chunks[0]
	q.chunks[0] = nil
	q.chunks = q.chunks[1:]
	q.size -= len(data)
	return data, false, true
}
