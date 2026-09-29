package launcher

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
	s, err := connect(t.Context(), ep, []byte("s3cret"), resume.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if proto.CheckSessionID(s.ID) != nil || s.Target.Hostname != "target" || s.Target.Home != target.Home {
		t.Fatalf("session %q, target %+v", s.ID, s.Target)
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
	_, err := connect(t.Context(), ep, []byte("wrong"), resume.Config{})
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("connect with a wrong token = %v, want the server's refusal", err)
	}
}

func TestConnectHandshakeTimeout(t *testing.T) {
	// A server that accepts but never answers does not hang the launcher.
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
	if _, err := connect(ctx, ep, []byte("x"), resume.Config{}); err == nil {
		t.Fatal("connect to a mute server succeeded")
	}
}

func TestCheckTarget(t *testing.T) {
	ok := proto.HelloReply{Target: proto.TargetInfo{User: "bob", Home: "/home/bob"}, ScratchDir: "/home/bob/.cache/tele/s/x"}
	if err := checkTarget(&ok); err != nil {
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
		if err := checkTarget(&r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
