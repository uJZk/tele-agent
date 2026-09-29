package server

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/mux"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/resume"
	"github.com/ujzk/tele-agent/internal/sstransport"
)

// serve runs a server with cfg on ln until the test ends.
func serve(t *testing.T, cfg Config, ln net.Listener) {
	t.Helper()
	cfg.FSRoot = t.TempDir()
	cfg.Target = &proto.TargetInfo{User: "bob", Home: t.TempDir(), Shell: "/bin/sh", LoginPath: "/usr/bin:/bin"}
	cfg.ScratchBase = t.TempDir()
	cfg.Logger = testLogger()
	srv := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.Serve(ctx, ln) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// hello opens a session over dial and returns the server's reply, or the
// error that prevented one.
func hello(t *testing.T, dial resume.DialFunc, token string) (*mux.Session, *proto.HelloReply, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := resume.Dial(ctx, dial, resume.Config{})
	if err != nil {
		return nil, nil, err
	}
	m, err := mux.Client(conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	t.Cleanup(func() { _ = m.Close() })
	st, err := m.Open(proto.StreamControl)
	if err != nil {
		return nil, nil, err
	}
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	c := proto.NewConn(st, proto.MaxControlFrame)
	_ = c.Send(&proto.Hello{Version: proto.Version, Token: []byte(token), SessionID: "0123456789abcdef"})
	var reply proto.HelloReply
	if err := c.Recv(&reply); err != nil {
		return nil, nil, err
	}
	return m, &reply, nil
}

func unixListener(t *testing.T) (net.Listener, resume.DialFunc) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	return ln, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}
}

func TestTokenAuth(t *testing.T) {
	for _, tt := range []struct {
		name        string
		serverToken string
		helloToken  string
		ok          bool
	}{
		{name: "matching token", serverToken: "s3cret", helloToken: "s3cret", ok: true},
		{name: "wrong token", serverToken: "s3cret", helloToken: "guess"},
		{name: "missing token", serverToken: "s3cret", helloToken: ""},
		// Two empty tokens compare equal: a server that has no token and
		// no authenticating transport must still refuse everyone.
		{name: "no token configured", serverToken: "", helloToken: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ln, dial := unixListener(t)
			serve(t, Config{Token: []byte(tt.serverToken)}, ln)
			_, reply, err := hello(t, dial, tt.helloToken)
			if err != nil {
				t.Fatal(err)
			}
			if ok := reply.Err == nil; ok != tt.ok {
				t.Fatalf("accepted = %v (%v), want %v", ok, reply.Err, tt.ok)
			}
		})
	}
}

// TestSS2022Session runs a session over the SS2022 transport, where the
// PSK alone authenticates the client, and a command through it.
func TestSS2022Session(t *testing.T) {
	psk, err := sstransport.NewPSK()
	if err != nil {
		t.Fatal(err)
	}
	lep := endpoint.Endpoint{Network: endpoint.NetworkSS2022, Address: "127.0.0.1:0"}.WithPSK(psk)
	ln, err := lep.Listen(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	serve(t, Config{TransportAuthenticated: true}, ln)
	ep, err := endpoint.Parse(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	m, reply, err := hello(t, ep.WithPSK(psk).Dial, "")
	if err != nil || reply.Err != nil {
		t.Fatalf("hello over SS2022: %v %v", err, reply)
	}
	e := &env{mux: m, home: reply.Target.Home}
	p, out := e.run(t, "echo over-ss2022")
	<-p.Done()
	if got := strings.TrimSpace(string(<-out)); got != "over-ss2022" {
		t.Fatalf("command output %q", got)
	}

	// A client with another PSK gets no session.
	other, err := sstransport.NewPSK()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if conn, err := resume.Dial(ctx, ep.WithPSK(other).Dial, resume.Config{}); err == nil {
		_ = conn.Close()
		t.Fatal("session established with a wrong PSK")
	}
}
