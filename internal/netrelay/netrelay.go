// Package netrelay copies bytes between two connections.
package netrelay

import (
	"errors"
	"io"
	"net"
	"sync"
)

// Relay copies a to b and b to a until both directions end, passing each
// end-of-stream on as a half-close so that request/response protocols keep
// working. A mux stream has no CloseWrite, but its Close is already a
// half-close: yamux sends FIN and keeps the stream readable.
func Relay(a, b net.Conn) error {
	var wg sync.WaitGroup
	errs := make([]error, 2)
	pipe := func(i int, dst, src net.Conn) {
		_, errs[i] = io.Copy(dst, src)
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = hc.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Go(func() { pipe(0, b, a) })
	wg.Go(func() { pipe(1, a, b) })
	wg.Wait()
	return errors.Join(errs...)
}
