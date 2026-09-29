package portfwd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/testutil/servertest"
)

func TestMCPPorts(t *testing.T) {
	doc := `{"mcpServers": {
		"a": {"type": "http", "url": "http://localhost:3000/mcp"},
		"b": {"type": "sse",  "url": "http://127.0.0.1:8080/sse"},
		"c": {"type": "http", "url": "http://[::1]:9000"},
		"d": {"type": "http", "url": "http://localhost:3000/other"},
		"e": {"type": "http", "url": "https://example.com:443/mcp"},
		"f": {"type": "stdio", "command": "x"},
		"g": {"type": "http", "url": "http://localhost:${PORT}/mcp"},
		"h": {"type": "http", "url": "http://api.localhost:7000"},
		"i": {"url": "http://127.0.0.2:7100"},
		"j": {"type": "http", "url": "http://localhost/mcp"},
		"k": {"type": "http", "url": "ftp://localhost:21"},
		"l": {"type": "http", "url": "http://localhost:0"}
	}}`
	got, err := MCPPorts([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := []uint16{80, 3000, 7000, 7100, 8080, 9000}
	if !slices.Equal(got, want) {
		t.Fatalf("MCPPorts = %v, want %v", got, want)
	}
	if _, err := MCPPorts([]byte(`{`)); err == nil {
		t.Fatal("MCPPorts accepted broken JSON")
	}
	if got, err := MCPPorts([]byte(`{}`)); err != nil || len(got) != 0 {
		t.Fatalf("MCPPorts({}) = %v, %v", got, err)
	}
}

// startEcho runs a line-echo service on a loopback port of "the target"
// (this machine) and returns its port.
func startEcho(t *testing.T) uint16 {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				// Reply only after the request ended: this needs the
				// half-close to pass through the forward.
				req, _ := io.ReadAll(c)
				_, _ = c.Write([]byte(strings.ToUpper(string(req))))
			})
		}
	})
	return tcpPort(t, ln.Addr())
}

func tcpPort(t *testing.T, a net.Addr) uint16 {
	t.Helper()
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		t.Fatalf("address %T", a)
	}
	return uint16(ta.Port)
}

func TestForward(t *testing.T) {
	ep, _ := servertest.Start(t, "s3cret", nil)
	m, _ := servertest.Hello(t, ep, "s3cret", "0123456789abcdef")
	remote := startEcho(t)

	f := &Forwarder{Opener: m}
	defer f.Close()
	addrs, err := f.listen(t.Context(), []string{"127.0.0.1"}, 0, remote)
	if err != nil {
		t.Fatal(err)
	}
	local := addrs[0].String()

	call := func(msg string) (string, error) {
		var d net.Dialer
		c, err := d.DialContext(t.Context(), "tcp", local)
		if err != nil {
			return "", err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.Write([]byte(msg)); err != nil {
			return "", err
		}
		tc, ok := c.(*net.TCPConn)
		if !ok {
			return "", fmt.Errorf("connection %T", c)
		}
		if err := tc.CloseWrite(); err != nil {
			return "", err
		}
		b, err := io.ReadAll(c)
		return string(b), err
	}
	if got, err := call("hello"); err != nil || got != "HELLO" {
		t.Fatalf("forwarded request = %q, %v", got, err)
	}

	// Many connections at once, each its own stream.
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Go(func() {
			msg := "m" + strconv.Itoa(i)
			if got, err := call(msg); err != nil || got != strings.ToUpper(msg) {
				errs <- fmt.Errorf("%s: %q: %w", msg, got, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestForwardRefused checks that a closed port on the target shows up as a
// closed connection locally, as a refused connection would.
func TestForwardRefused(t *testing.T) {
	ep, _ := servertest.Start(t, "s3cret", nil)
	m, _ := servertest.Hello(t, ep, "s3cret", "0123456789abcdef")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := tcpPort(t, ln.Addr())
	_ = ln.Close()

	f := &Forwarder{Opener: m}
	defer f.Close()
	addrs, err := f.listen(t.Context(), []string{"127.0.0.1"}, 0, closedPort)
	if err != nil {
		t.Fatal(err)
	}
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", addrs[0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := bufio.NewReader(c).ReadByte(); err == nil {
		t.Fatal("read data from a forward to a closed port")
	} else if ne := net.Error(nil); errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("forward to a closed port was not closed")
	}
}

// TestForwardPortInUse checks that Forward reports a local port that is
// taken, instead of letting Claude reach whatever holds it.
func TestForwardPortInUse(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := tcpPort(t, ln.Addr())
	f := &Forwarder{Opener: nil}
	defer f.Close()
	if _, err := f.listen(context.Background(), []string{"127.0.0.1"}, port, port); err == nil {
		t.Fatal("forwarding a taken port succeeded")
	}
}
