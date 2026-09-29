// Package portfwd forwards local loopback ports to the same ports on the
// target host, so that HTTP and SSE MCP servers configured with a
// localhost URL in the project's .mcp.json, which run on the target, are
// reachable from Claude (docs/exec.md "端口转发").
package portfwd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ujzk/tele-agent/internal/netrelay"
	"github.com/ujzk/tele-agent/internal/proto"
)

// Opener opens a multiplexed stream to the server (*mux.Session).
type Opener interface {
	Open(kind proto.StreamKind) (net.Conn, error)
}

// MCPPorts returns the loopback ports of the HTTP and SSE servers in an
// .mcp.json document, sorted and without duplicates. Entries whose URL
// cannot be parsed, or still holds an unexpanded ${VAR}, are skipped.
func MCPPorts(mcpJSON []byte) ([]uint16, error) {
	var doc struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mcpJSON, &doc); err != nil {
		return nil, fmt.Errorf("parse .mcp.json: %w", err)
	}
	var ports []uint16
	for _, s := range doc.MCPServers {
		if s.URL == "" || s.Type != "http" && s.Type != "sse" && s.Type != "" {
			continue
		}
		if p, ok := loopbackPort(s.URL); ok {
			ports = append(ports, p)
		}
	}
	slices.Sort(ports)
	return slices.Compact(ports), nil
}

// loopbackPort returns the port of a URL whose host is loopback.
func loopbackPort(raw string) (uint16, bool) {
	if strings.Contains(raw, "${") {
		return 0, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" {
		return 0, false
	}
	switch h := u.Hostname(); {
	case h == "localhost", strings.HasSuffix(h, ".localhost"):
	default:
		ip := net.ParseIP(h)
		if ip == nil || !ip.IsLoopback() {
			return 0, false
		}
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint16(n), true
}

// Forwarder listens on local loopback ports and forwards each connection
// to the same port on the target host.
type Forwarder struct {
	Opener Opener
	Logger *slog.Logger

	mu  sync.Mutex
	lns []net.Listener
	wg  sync.WaitGroup
}

// Forward starts forwarding port on 127.0.0.1 and, where available, ::1.
// It fails when neither address can be bound: then something local already
// uses the port, and Claude would reach that instead of the MCP server.
func (f *Forwarder) Forward(ctx context.Context, port uint16) error {
	_, err := f.listen(ctx, []string{"127.0.0.1", "::1"}, port, port)
	return err
}

// listen forwards localPort on each of hosts to remotePort on the target
// and returns the bound addresses.
func (f *Forwarder) listen(ctx context.Context, hosts []string, localPort, remotePort uint16) ([]net.Addr, error) {
	var lc net.ListenConfig
	var addrs []net.Addr
	var firstErr error
	for _, host := range hosts {
		ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(localPort))))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		addrs = append(addrs, ln.Addr())
		f.mu.Lock()
		f.lns = append(f.lns, ln)
		f.mu.Unlock()
		f.wg.Go(func() { f.accept(ln, remotePort) })
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("forward port %d: %w", localPort, firstErr)
	}
	return addrs, nil
}

// Close stops listening and waits for the forwarded connections to end.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	lns := f.lns
	f.lns = nil
	f.mu.Unlock()
	for _, ln := range lns {
		_ = ln.Close()
	}
	f.wg.Wait()
	return nil
}

func (f *Forwarder) log() *slog.Logger {
	if f.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return f.Logger
}

func (f *Forwarder) accept(ln net.Listener, port uint16) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				f.log().Warn("port forward accept", "port", port, "err", err)
			}
			return
		}
		f.wg.Go(func() {
			defer c.Close()
			if err := f.forward(c, port); err != nil {
				f.log().Warn("port forward", "port", port, "err", err)
			}
		})
	}
}

func (f *Forwarder) forward(c net.Conn, port uint16) error {
	st, err := f.Opener.Open(proto.StreamForward)
	if err != nil {
		return err
	}
	defer st.Close()
	pc := proto.NewConn(st, proto.MaxControlFrame)
	if err := pc.Send(&proto.ForwardOpen{Port: port}); err != nil {
		return err
	}
	var reply proto.ForwardReply
	if err := pc.Recv(&reply); err != nil {
		return err
	}
	if reply.Err != nil {
		// The client sees the connection close, as with a refused port.
		return fmt.Errorf("target: %w", reply.Err)
	}
	return netrelay.Relay(c, st)
}
