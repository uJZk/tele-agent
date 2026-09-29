package rexec

import (
	"fmt"
	"sync"

	"github.com/ujzk/tele-agent/internal/proto"
)

// stdinCredit is the part of the stdin window (proto.ExecStdinWindow) the
// client may still send without an acknowledgement.
type stdinCredit struct {
	mu   sync.Mutex
	n    int           // guarded by mu
	wake chan struct{} // buffered, capacity 1; signalled when n grows
}

func newStdinCredit() *stdinCredit {
	return &stdinCredit{n: proto.ExecStdinWindow, wake: make(chan struct{}, 1)}
}

// add returns acknowledged bytes to the window. An acknowledgement of
// more than was sent is a protocol violation.
func (c *stdinCredit) add(n int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 || c.n+n > proto.ExecStdinWindow {
		return fmt.Errorf("rexec: server acknowledged %d bytes of stdin beyond what was sent", n)
	}
	c.n += n
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

// available returns the window left.
func (c *stdinCredit) available() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// spend takes n bytes from the window; n must not exceed available.
func (c *stdinCredit) spend(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > c.n {
		panic(fmt.Sprintf("rexec: spend %d bytes with %d left in the stdin window", n, c.n))
	}
	c.n -= n
}
