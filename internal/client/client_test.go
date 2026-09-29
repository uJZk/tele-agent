package client

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/endpoint"
	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/resume"
	"github.com/ujzk/tele-agent/internal/rexec"
	"github.com/ujzk/tele-agent/internal/testutil/servertest"
)

func testTarget(t *testing.T) *proto.TargetInfo {
	return &proto.TargetInfo{
		Hostname: "target", User: "bob", Home: t.TempDir(), Shell: "/bin/sh",
		LoginPath: "/usr/local/bin:/usr/bin:/bin",
	}
}

func TestConnect(t *testing.T) {
	target := testTarget(t)
	ep, root := servertest.Start(t, "s3cret", target)
	s, err := Connect(t.Context(), ep, []byte("s3cret"), resume.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if proto.CheckSessionID(s.ID) != nil || s.Target.Hostname != "target" || s.Target.Home != target.Home {
		t.Fatalf("session %q, target %+v", s.ID, s.Target)
	}
	// File traffic runs over connections of its own.
	if len(s.Conns) != 3 || s.Meta == s.Mux || s.Bulk == s.Mux || s.Meta == s.Bulk {
		t.Errorf("session has %d connections; Meta and Bulk separate: %v %v", len(s.Conns), s.Meta != s.Mux, s.Bulk != s.Mux)
	}
	// Server and client share a clock here, so the estimate is small.
	if s.RTT <= 0 || s.ClockSkew < -time.Second || s.ClockSkew > time.Second {
		t.Errorf("RTT %v, clock skew %v", s.RTT, s.ClockSkew)
	}
	if !strings.HasPrefix(s.ScratchDir, target.Home+"/") || !strings.HasSuffix(s.ScratchDir, s.ID) {
		t.Errorf("scratch dir %q, want one per session under the target's home", s.ScratchDir)
	}

	// The session carries commands and file requests.
	var out bytes.Buffer
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = out.ReadFrom(r)
	}()
	p, err := (&rexec.Client{Opener: s.Mux}).Start(t.Context(), rexec.Command{Argv: []string{"echo", "over tele"}, Dir: "/", Stdout: w})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Wait(t.Context())
	<-p.Done()
	_ = w.Close()
	<-done
	_ = r.Close()
	if err != nil || res.Code != 0 || out.String() != "over tele\n" {
		t.Fatalf("remote echo: %+v, %v, output %q", res, err, out.String())
	}

	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := s.Mux.Open(proto.StreamFS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := proto.WriteFrame(st, &proto.FSRequest{Op: proto.FSGetattr, Path: "/f"}); err != nil {
		t.Fatal(err)
	}
	var resp proto.FSResponse
	if err := proto.ReadFrame(st, &resp, proto.MaxDataFrame); err != nil || resp.Errno != 0 || resp.Attr == nil || resp.Attr.Size != 1 {
		t.Fatalf("getattr /f: %+v, %v", resp, err)
	}
}

func TestConnectRejected(t *testing.T) {
	ep, _ := servertest.Start(t, "s3cret", testTarget(t))
	_, err := Connect(t.Context(), ep, []byte("wrong"), resume.Config{})
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("Connect with a wrong token = %v, want the server's refusal", err)
	}
}

func TestConnectHandshakeTimeout(t *testing.T) {
	// A server that accepts but never answers does not hang the client.
	sock := filepath.Join(t.TempDir(), "mute.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			<-t.Context().Done()
			_ = c.Close()
		}
	}()
	ep, _ := endpoint.Parse("unix:" + sock)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := Connect(ctx, ep, []byte("x"), resume.Config{}); err == nil {
		t.Fatal("Connect to a mute server succeeded")
	}
}

func TestCheckTarget(t *testing.T) {
	ok := proto.HelloReply{Target: proto.TargetInfo{User: "bob", Home: "/home/bob"}, ScratchDir: "/home/bob/.cache/tele/s/x"}
	if err := CheckTarget(&ok); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*proto.HelloReply){
		"relative home":   func(r *proto.HelloReply) { r.Target.Home = "home/bob" },
		"root home":       func(r *proto.HelloReply) { r.Target.Home = "/" },
		"unclean scratch": func(r *proto.HelloReply) { r.ScratchDir = "/a/../b" },
		"no user":         func(r *proto.HelloReply) { r.Target.User = "" },
	} {
		r := ok
		mod(&r)
		if err := CheckTarget(&r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestRotate puts an endpoint that cannot be reached first: the session is
// established through the second, and later dials start there.
func TestRotate(t *testing.T) {
	ep, _ := servertest.Start(t, "s3cret", testTarget(t))
	dead, err := endpoint.Parse("unix:" + filepath.Join(t.TempDir(), "nobody.sock"))
	if err != nil {
		t.Fatal(err)
	}
	dial := Rotate([]endpoint.Endpoint{dead, ep})
	s, err := ConnectDial(t.Context(), dial, []byte("s3cret"), resume.Config{})
	if err != nil {
		t.Fatalf("connect through the second endpoint: %v", err)
	}
	defer func() { _ = s.Close() }()
	if len(s.Conns) == 0 {
		t.Fatal("session has no resume connection")
	}
	// Sticky: the next dial goes straight to the endpoint that worked.
	c, err := dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	// With every endpoint down, the error names them all.
	_, err = Rotate([]endpoint.Endpoint{dead, dead})(t.Context())
	if err == nil || strings.Count(err.Error(), "connect to ") != 2 {
		t.Fatalf("dial with every endpoint down = %v", err)
	}
}
