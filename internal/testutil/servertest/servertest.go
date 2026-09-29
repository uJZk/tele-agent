// Package servertest runs a tele server on a unix socket for tests and
// opens sessions with it.
package servertest

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/server"
)

// Start runs a tele server that accepts token and describes itself as
// target, serving a fresh temporary directory as "/", which it returns.
// The server stops when the test ends.
func Start(t testing.TB, token string, target *proto.TargetInfo) (endpoint.Endpoint, string) {
	t.Helper()
	root := t.TempDir()
	return StartRoot(t, token, target, root), root
}

// StartRoot is Start serving root as "/"; "/" serves this machine's own
// file system.
func StartRoot(t testing.TB, token string, target *proto.TargetInfo, root string) endpoint.Endpoint {
	t.Helper()
	return StartConfig(t, server.Config{Token: []byte(token), FSRoot: root, Target: target})
}

// StartConfig runs a tele server with cfg on a unix socket until the test
// ends.
func StartConfig(t testing.TB, cfg server.Config) endpoint.Endpoint {
	t.Helper()
	ep, err := endpoint.Parse("unix:" + filepath.Join(t.TempDir(), "tele.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ep.Listen(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.Serve(ctx, ln) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return ep
}

// Hello opens session sid with the server at ep and returns it with the
// server's reply. The session is closed when the test ends; register
// cleanups that must run while it is open after calling Hello.
func Hello(t testing.TB, ep endpoint.Endpoint, token, sid string) (*mux.Session, proto.HelloReply) {
	t.Helper()
	conn, err := ep.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m, err := mux.Client(conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	st, err := m.Open(proto.StreamControl)
	if err != nil {
		t.Fatal(err)
	}
	c := proto.NewConn(st, proto.MaxControlFrame)
	if err := c.Send(&proto.Hello{Version: proto.Version, Token: []byte(token), SessionID: sid}); err != nil {
		t.Fatal(err)
	}
	var reply proto.HelloReply
	if err := c.Recv(&reply); err != nil || reply.Err != nil {
		t.Fatalf("hello: %v %v", err, reply.Err)
	}
	return m, reply
}
