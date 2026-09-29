package resume

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
	"github.com/ujzk/tele-agent/internal/testutil/faultnet"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fastConfig makes failures show quickly.
var fastConfig = Config{Lease: 5 * time.Second, Heartbeat: 50 * time.Millisecond}

// pairOf starts a server and a client connected through a fault proxy.
func pairOf(t *testing.T, cfg Config) (client, server *Conn, px *faultnet.Proxy, l *Listener) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", filepath.Join(t.TempDir(), "server.sock"))
	if err != nil {
		t.Fatal(err)
	}
	l = Listen(ln, cfg)
	t.Cleanup(func() { _ = l.Close() })
	px = faultnet.New(t, ln.Addr().String())
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", px.Addr())
	}
	client, err = Dial(t.Context(), dial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server, err = l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return client, server, px, l
}

func randomData(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// transfer writes data to w in chunks and reads len(data) bytes from r,
// returning them.
func transfer(t *testing.T, w, r io.ReadWriter, data []byte) <-chan []byte {
	t.Helper()
	out := make(chan []byte, 1)
	go func() {
		for rest := data; len(rest) > 0; {
			n := min(len(rest), 7919)
			if _, err := w.Write(rest[:n]); err != nil {
				t.Errorf("write: %v", err)
				return
			}
			rest = rest[n:]
		}
	}()
	go func() {
		got := make([]byte, len(data))
		if _, err := io.ReadFull(r, got); err != nil {
			t.Errorf("read: %v", err)
		}
		out <- got
	}()
	return out
}

func wait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// TestExactlyOnceAcrossCuts sends data both ways while the transport is
// cut again and again: every byte arrives once, in order.
func TestExactlyOnceAcrossCuts(t *testing.T) {
	client, server, px, _ := pairOf(t, fastConfig)
	up, down := randomData(t, 4<<20), randomData(t, 4<<20)
	gotUp := transfer(t, client, server, up)
	gotDown := transfer(t, server, client, down)
	stop := make(chan struct{})
	var cuts sync.WaitGroup
	cuts.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				px.Cut()
			}
		}
	})
	u := wait(t, gotUp, "the client's data")
	d := wait(t, gotDown, "the server's data")
	close(stop)
	cuts.Wait()
	if !bytes.Equal(u, up) {
		t.Error("server received other bytes than the client sent")
	}
	if !bytes.Equal(d, down) {
		t.Error("client received other bytes than the server sent")
	}
}

// TestHalfOpen stops forwarding without closing the connection: the
// heartbeat notices and the client reconnects.
func TestHalfOpen(t *testing.T) {
	client, server, px, _ := pairOf(t, fastConfig)
	px.Hole()
	data := randomData(t, 256<<10)
	got := wait(t, transfer(t, client, server, data), "data after a half-open transport")
	if !bytes.Equal(got, data) {
		t.Fatal("data corrupted")
	}
}

// TestLeaseExpiry keeps the client away for longer than the lease: both
// ends fail with ErrExpired, and the server's hook runs first.
func TestLeaseExpiry(t *testing.T) {
	cfg := fastConfig
	cfg.Lease = 300 * time.Millisecond
	client, server, px, _ := pairOf(t, cfg)
	var hooked atomic.Bool
	server.OnExpire(func() {
		if server.Err() != nil {
			t.Error("hook ran after the session failed")
		}
		hooked.Store(true)
	})
	px.SetRefuse(true)
	px.Cut()
	readErr := make(chan error, 1)
	go func() {
		_, err := server.Read(make([]byte, 1))
		readErr <- err
	}()
	if err := wait(t, readErr, "the server's read to fail"); !errors.Is(err, ErrExpired) {
		t.Fatalf("server read: %v, want ErrExpired", err)
	}
	if !hooked.Load() {
		t.Error("expiry hook did not run")
	}
	<-client.Done()
	if err := client.Err(); !errors.Is(err, ErrExpired) {
		t.Fatalf("client: %v, want ErrExpired", err)
	}
}

// TestFinEndsAtOnce closes the client: the server reads everything, then
// EOF, without waiting for the lease.
func TestFinEndsAtOnce(t *testing.T) {
	client, server, _, _ := pairOf(t, fastConfig)
	data := randomData(t, 1<<20)
	if _, err := client.Write(data); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(server)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("server read %d bytes, %v; want all %d, then EOF", len(got), err, len(data))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("session ended after %v", d)
	}
	if _, err := client.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("write after Close: %v", err)
	}
}

// TestSessionLost points the client at a server that never knew its
// session, as after a server restart.
func TestSessionLost(t *testing.T) {
	client, _, px, _ := pairOf(t, fastConfig)
	var lc net.ListenConfig
	other, err := lc.Listen(t.Context(), "unix", filepath.Join(t.TempDir(), "other.sock"))
	if err != nil {
		t.Fatal(err)
	}
	l2 := Listen(other, fastConfig)
	defer func() { _ = l2.Close() }()
	px.SetTarget(other.Addr().String())
	px.Cut()
	<-client.Done()
	if err := client.Err(); !errors.Is(err, ErrSessionLost) {
		t.Fatalf("client: %v, want ErrSessionLost", err)
	}
}

// TestResumeNeedsKey tries to resume a session knowing only its ID.
func TestResumeNeedsKey(t *testing.T) {
	client, server, _, l := pairOf(t, fastConfig)
	var d net.Dialer
	tr, err := d.DialContext(t.Context(), "unix", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	if err := proto.WriteFrameLimit(tr, &proto.ResumeHello{Version: proto.ResumeVersion, Session: client.ID()}, proto.MaxControlFrame); err != nil {
		t.Fatal(err)
	}
	var ch proto.ResumeReply
	if err := proto.ReadFrame(tr, &ch, proto.MaxControlFrame); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteFrameLimit(tr, &proto.ResumeProof{MAC: proof(randomData(t, proto.ResumeKeyLen), ch.Nonce)}, proto.MaxControlFrame); err != nil {
		t.Fatal(err)
	}
	var reply proto.ResumeReply
	if err := proto.ReadFrame(tr, &reply, proto.MaxControlFrame); err != nil {
		t.Fatal(err)
	}
	if reply.Err == nil || !errors.Is(reply.Err, unix.EACCES) {
		t.Fatalf("reply to a wrong proof: %+v", reply)
	}
	// The session still works.
	data := randomData(t, 1000)
	if got := wait(t, transfer(t, client, server, data), "data"); !bytes.Equal(got, data) {
		t.Fatal("session broken by the failed takeover")
	}
}
