package mux

import (
	"errors"
	"io"
	"net"
	"testing"

	"go.uber.org/goleak"

	"github.com/ujzk/tele-agent/internal/proto"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func pair(t *testing.T) (client, server *Session) {
	t.Helper()
	a, b := net.Pipe()
	c, err := Client(a)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Server(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = s.Close()
	})
	return c, s
}

func TestStreamKindsAndHalfClose(t *testing.T) {
	c, s := pair(t)

	done := make(chan error, 1)
	go func() {
		for range 2 {
			st, err := s.Accept()
			if err != nil {
				done <- err
				return
			}
			kind, err := ReadKind(st)
			if err != nil {
				done <- err
				return
			}
			var req proto.FSRequest
			if err := proto.ReadFrame(st, &req, proto.MaxDataFrame); err != nil {
				done <- err
				return
			}
			// Echo the kind back in the path, then half-close.
			if err := proto.WriteFrame(st, &proto.FSResponse{Target: kind.String() + ":" + req.Path}); err != nil {
				done <- err
				return
			}
			if err := st.Close(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	for _, kind := range []proto.StreamKind{proto.StreamFS, proto.StreamExec} {
		st, err := c.Open(kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.WriteFrame(st, &proto.FSRequest{Op: proto.FSGetattr, Path: "/x"}); err != nil {
			t.Fatal(err)
		}
		var resp proto.FSResponse
		if err := proto.ReadFrame(st, &resp, proto.MaxDataFrame); err != nil {
			t.Fatal(err)
		}
		if want := kind.String() + ":/x"; resp.Target != want {
			t.Fatalf("response %q, want %q", resp.Target, want)
		}
		if _, err := st.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("read after peer close = %v, want EOF", err)
		}
		_ = st.Close()
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReadKindRejectsUnknown(t *testing.T) {
	c, s := pair(t)
	go func() {
		st, err := c.ys.OpenStream()
		if err != nil {
			return
		}
		_ = proto.WriteFrame(st, &proto.StreamHeader{Kind: 200})
	}()
	st, err := s.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKind(st); err == nil {
		t.Fatal("ReadKind accepted unknown kind")
	}
}

func TestDoneOnClose(t *testing.T) {
	c, s := pair(t)
	_ = c.Close()
	<-s.Done()
	if _, err := s.Accept(); !IsClosed(err) {
		t.Fatalf("Accept after close = %v, want closed", err)
	}
}
