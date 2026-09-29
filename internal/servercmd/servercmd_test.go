package servercmd

import (
	"bytes"
	"context"
	"log/slog"
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

// TestServe runs the server as "tele server run" does and opens a session
// with the PSK alone.
func TestServe(t *testing.T) {
	psk, err := sstransport.NewPSK()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan string, 1)
	var wg sync.WaitGroup
	var serveErr error
	wg.Go(func() {
		serveErr = serve(ctx, "127.0.0.1:0", psk, slog.New(slog.DiscardHandler), func(a string) { addrc <- a })
	})
	defer func() {
		cancel()
		wg.Wait()
		if serveErr != nil {
			t.Errorf("serve: %v", serveErr)
		}
	}()
	var addr string
	select {
	case addr = <-addrc:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	ep, err := endpoint.Parse(addr)
	if err != nil {
		t.Fatal(err)
	}
	dctx, dcancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer dcancel()
	conn, err := resume.Dial(dctx, ep.WithPSK(psk).Dial, resume.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := mux.Client(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	st, err := m.Open(proto.StreamControl)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	c := proto.NewConn(st, proto.MaxControlFrame)
	if err := c.Send(&proto.Hello{Version: proto.Version, SessionID: "0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	var reply proto.HelloReply
	if err := c.Recv(&reply); err != nil || reply.Err != nil {
		t.Fatalf("hello: %v %v", err, reply.Err)
	}
	if reply.Target.User == "" {
		t.Fatal("server reported no target user")
	}
}

func TestMainUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "usage: tele server run"},
		{[]string{"bogus"}, 2, `unknown command "bogus"`},
		{[]string{"run", "--nope"}, 2, "flag provided but not defined"},
		{[]string{"run", "extra"}, 2, `unexpected argument "extra"`},
		{[]string{"run", "--config", "/nonexistent/server.json"}, 1, "not configured"},
		{[]string{"help"}, 0, "usage:"},
	} {
		var out, errb bytes.Buffer
		code := Main(tc.args, &out, &errb)
		if code != tc.code || !strings.Contains(out.String()+errb.String(), tc.want) {
			t.Errorf("Main(%q) = %d, %q; want %d containing %q", tc.args, code, out.String()+errb.String(), tc.code, tc.want)
		}
	}
}
