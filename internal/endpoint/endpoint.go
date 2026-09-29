// Package endpoint parses where a tele server listens and connects to it.
//
// "host:port" is the SS2022 transport over TCP (docs/transport.md "SS2022"),
// authenticated with the host's PSK. "unix:<path>" is a plain unix socket,
// used for a server on the same machine and in tests; there the file
// permissions and the session token protect the server.
package endpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/sstransport"
)

// Networks of an Endpoint.
const (
	NetworkUnix   = "unix"
	NetworkSS2022 = "ss2022"
)

// Endpoint is a parsed server address.
type Endpoint struct {
	Network string // NetworkUnix or NetworkSS2022
	Address string // socket path, or host:port
	// psk authenticates an SS2022 endpoint (WithPSK). It is not part of
	// the endpoint's text form and never printed.
	psk *sstransport.PSK
}

// Parse parses an endpoint string.
func Parse(s string) (Endpoint, error) {
	if path, ok := strings.CutPrefix(s, "unix:"); ok {
		if !filepath.IsAbs(path) {
			return Endpoint{}, fmt.Errorf("endpoint %q: unix socket path must be absolute", s)
		}
		return Endpoint{Network: "unix", Address: filepath.Clean(path)}, nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil || port == "" {
		return Endpoint{}, fmt.Errorf("endpoint %q: want host:port or unix:<path>", s)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return Endpoint{}, fmt.Errorf("endpoint %q: invalid port %q", s, port)
	}
	return Endpoint{Network: NetworkSS2022, Address: net.JoinHostPort(host, port)}, nil
}

// WithPSK returns e authenticated with psk; unix endpoints ignore it.
func (e Endpoint) WithPSK(psk sstransport.PSK) Endpoint {
	e.psk = &psk
	return e
}

// Authenticates reports whether the transport itself authenticates the
// peer (SS2022), so that the server need not check a session token.
func (e Endpoint) Authenticates() bool { return e.Network == NetworkSS2022 }

var errNoPSK = errors.New("SS2022 endpoint without a PSK")

// String returns the endpoint in the form Parse accepts; never the PSK.
func (e Endpoint) String() string {
	if e.Network == NetworkSS2022 {
		return e.Address
	}
	return e.Network + ":" + e.Address
}

// Dial connects to the endpoint.
func (e Endpoint) Dial(ctx context.Context) (net.Conn, error) {
	if e.Network == NetworkSS2022 {
		if e.psk == nil {
			return nil, fmt.Errorf("connect to %v: %w", e, errNoPSK)
		}
		return sstransport.Dial(ctx, e.Address, *e.psk)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, e.Network, e.Address)
	if err != nil {
		return nil, fmt.Errorf("connect to %v: %w", e, err)
	}
	return c, nil
}

// Listen listens on the endpoint. A stale unix socket file left by a server
// that is no longer running is replaced; a live one is an error. An SS2022
// listener returns only authenticated connections; log receives its
// diagnostics (nil discards them).
func (e Endpoint) Listen(ctx context.Context, log *slog.Logger) (net.Listener, error) {
	if e.Network == NetworkSS2022 {
		if e.psk == nil {
			return nil, fmt.Errorf("listen on %v: %w", e, errNoPSK)
		}
		return sstransport.Listen(ctx, e.Address, *e.psk, log)
	}
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
