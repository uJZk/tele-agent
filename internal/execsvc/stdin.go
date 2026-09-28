package execsvc

import "sync"

// stdinQueueMax bounds the stdin data queued for a command that does not
// read it. The exec stream has no flow control for stdin apart from the
// stream window, and signals and the client's end of the stream arrive
// behind stdin data, so the reader must not wait for the command to read
// its input. Up to this much it does not: it covers the JSON a hook
// receives on stdin, which holds whole file contents for Write and Edit.
// Beyond it the reader waits until the command reads or exits.
const stdinQueueMax = 8 << 20

// stdinQueue passes stdin data from the frame reader to the goroutine
// that writes it to the command.
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

// put queues data, waiting while the queue is full. Data that arrives
// after end or close is dropped.
func (q *stdinQueue) put(data []byte) {
	if len(data) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// An empty queue takes any chunk, so that no frame waits forever.
	for !q.closed && q.size > 0 && q.size+len(data) > stdinQueueMax {
		q.cond.Wait()
	}
	if q.closed || q.eof {
		return
	}
	q.chunks = append(q.chunks, data)
	q.size += len(data)
	q.cond.Broadcast()
}

// end records the end of the client's input, after the data queued so
// far.
func (q *stdinQueue) end() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.eof = true
	q.cond.Broadcast()
}

// close stops forwarding: queued and later data is dropped, and put and
// next return at once.
func (q *stdinQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.chunks, q.size = nil, 0
	q.cond.Broadcast()
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
	q.cond.Broadcast()
	return data, false, true
}
