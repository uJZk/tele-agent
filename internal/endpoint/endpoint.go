// Package endpoint parses where a tele server listens and connects to it.
//
// "unix:<path>" is a plain unix socket, used for a server on the same machine
// and in tests. The SS2022 transport over TCP (docs/transport.md) is not
// implemented yet.
package endpoint

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Endpoint is a parsed server address.
type Endpoint struct {
	Network string // "unix"
	Address string
}

// Parse parses an endpoint string.
func Parse(s string) (Endpoint, error) {
	if path, ok := strings.CutPrefix(s, "unix:"); ok {
		if !filepath.IsAbs(path) {
			return Endpoint{}, fmt.Errorf("endpoint %q: unix socket path must be absolute", s)
		}
		return Endpoint{Network: "unix", Address: filepath.Clean(path)}, nil
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return Endpoint{}, fmt.Errorf("endpoint %q: the SS2022 transport is not implemented yet; use unix:<path>", s)
	}
	return Endpoint{}, fmt.Errorf("endpoint %q: want unix:<path>", s)
}

func (e Endpoint) String() string {
	return e.Network + ":" + e.Address
}

// Dial connects to the endpoint.
func (e Endpoint) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, e.Network, e.Address)
	if err != nil {
		return nil, fmt.Errorf("connect to %v: %w", e, err)
	}
	return c, nil
}

// Listen listens on the endpoint. A stale unix socket file left by a server
// that is no longer running is replaced; a live one is an error.
func (e Endpoint) Listen(ctx context.Context) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, e.Network, e.Address)
	if err == nil || !errors.Is(err, unix.EADDRINUSE) {
		return ln, err
	}
	var d net.Dialer
	if c, derr := d.DialContext(ctx, e.Network, e.Address); derr == nil {
		_ = c.Close()
		return nil, fmt.Errorf("listen on %v: another server is running", e)
	}
	if fi, serr := os.Lstat(e.Address); serr != nil || fi.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("listen on %v: %w", e, err)
	}
	if rerr := os.Remove(e.Address); rerr != nil {
		return nil, fmt.Errorf("listen on %v: remove stale socket: %w", e, rerr)
	}
	return lc.Listen(ctx, e.Network, e.Address)
}
