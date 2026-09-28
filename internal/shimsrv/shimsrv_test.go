package shimsrv

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var testToken = []byte("0123456789abcdef0123456789abcdef")

// handlerFunc adapts a function to Handler and counts calls.
type handlerFunc struct {
	fn    func(ctx context.Context, req *Request, sigs <-chan int) proto.ShimStatus
	calls atomic.Int32
}

func (h *handlerFunc) Serve(ctx context.Context, req *Request, sigs <-chan int) proto.ShimStatus {
	h.calls.Add(1)
	return h.fn(ctx, req, sigs)
}

func closeReq(req *Request) {
	_ = req.Stdin.Close()
	_ = req.Stdout.Close()
	_ = req.Stderr.Close()
}

func randomSID(t *testing.T) string {
	t.Helper()
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// serve runs srv on a fresh session socket until the test ends and returns
// the session ID and a function that stops the server early.
func serve(t *testing.T, srv *Server) (sid string, stop func()) {
	t.Helper()
	sid = randomSID(t)
	ln, err := Listen(sid)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	var stopped atomic.Bool
	stop = func() {
		if stopped.Swap(true) {
			return
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Serve did not return")
		}
	}
	t.Cleanup(stop)
	return sid, stop
}

func dial(t *testing.T, sid string) *net.UnixConn {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: SocketName(sid), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// sendFDs sends the marker byte with fds attached; the caller keeps them.
func sendFDs(t *testing.T, conn *net.UnixConn, fds ...int) {
	t.Helper()
	var oob []byte
	if len(fds) > 0 {
		oob = unix.UnixRights(fds...)
	}
	if _, _, err := conn.WriteMsgUnix([]byte{0}, oob, nil); err != nil {
		t.Fatal(err)
	}
}

// devNulls opens n fds on /dev/null, closed when the test ends.
func devNulls(t *testing.T, n int) []int {
	t.Helper()
	fds := make([]int, n)
	for i := range fds {
		fd, err := unix.Open(os.DevNull, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		fds[i] = fd
		t.Cleanup(func() { _ = unix.Close(fd) })
	}
	return fds
}

func request(name string, token []byte) *proto.ShimRequest {
	return &proto.ShimRequest{Token: token, Name: name, Argv: []string{"/.tele/s/bin/" + name, "-c", "x"}, Dir: "/work", Env: []string{"A=1"}}
}

// handshake sends three /dev/null fds and req.
func handshake(t *testing.T, conn *net.UnixConn, req *proto.ShimRequest) {
	t.Helper()
	sendFDs(t, conn, devNulls(t, 3)...)
	if err := proto.WriteFrame(conn, req); err != nil {
		t.Fatal(err)
	}
}

// readExit returns the exit status, or the error that ended the stream.
func readExit(conn *net.UnixConn) (*proto.ShimStatus, error) {
	var f proto.ShimFrame
	if err := proto.ReadFrame(conn, &f, proto.MaxControlFrame); err != nil {
		return nil, err
	}
	if f.Op != proto.ShimExit || f.Exit == nil {
		return nil, errors.New("not an exit frame")
	}
	return f.Exit, nil
}

// openNulls counts this process's fds open on /dev/null. The fds sent to
// the server are all /dev/null and nothing else in the test opens it, so
// the count is not disturbed by unrelated fds that come and go.
func openNulls(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if target, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && target == os.DevNull {
			n++
		}
	}
	return n
}

func TestServeRunsHandler(t *testing.T) {
	type seen struct {
		req      Request
		nonblock bool
	}
	got := make(chan seen, 1)
	h := &handlerFunc{fn: func(_ context.Context, req *Request, _ <-chan int) proto.ShimStatus {
		flags, _ := unix.FcntlInt(req.Stdout.Fd(), unix.F_GETFL, 0)
		_, _ = io.WriteString(req.Stdout, "out")
		got <- seen{*req, flags&unix.O_NONBLOCK != 0}
		closeReq(req)
		return proto.ShimStatus{Code: 3, Msg: "m"}
	}}
	sid, _ := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
	conn := dial(t, sid)

	// stdout is a non-blocking pipe shared with "Claude": its flags must
	// survive (docs/exec.md "shim 与会话主进程").
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	pr := os.NewFile(uintptr(p[0]), "pr")
	defer pr.Close()
	nulls := devNulls(t, 2)
	sendFDs(t, conn, nulls[0], p[1], nulls[1])
	_ = unix.Close(p[1])
	req := request("bash", testToken)
	if err := proto.WriteFrame(conn, req); err != nil {
		t.Fatal(err)
	}

	st, err := readExit(conn)
	if err != nil {
		t.Fatal(err)
	}
	if *st != (proto.ShimStatus{Code: 3, Msg: "m"}) {
		t.Errorf("exit status %+v", st)
	}
	s := <-got
	if s.req.Name != "bash" || !slices.Equal(s.req.Argv, req.Argv) || s.req.Dir != "/work" || !slices.Equal(s.req.Env, req.Env) || s.req.PeerPID != os.Getpid() {
		t.Errorf("handler got %+v", s.req)
	}
	if !s.nonblock {
		t.Error("O_NONBLOCK of the shim's stdout was cleared")
	}
	if b, _ := io.ReadAll(pr); string(b) != "out" {
		t.Errorf("stdout got %q", b)
	}
	flags, err := unix.FcntlInt(uintptr(p[0]), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_NONBLOCK == 0 {
		t.Errorf("pipe flags %#x, %v: O_NONBLOCK cleared", flags, err)
	}
}

func TestCheckPeer(t *testing.T) {
	if err := checkPeer(&unix.Ucred{Pid: 1, Uid: 1000}, 1000); err != nil {
		t.Errorf("same uid rejected: %v", err)
	}
	for _, uid := range []uint32{0, 999, 1001, 65534} {
		if err := checkPeer(&unix.Ucred{Pid: 1, Uid: uid}, 1000); err == nil {
			t.Errorf("uid %d accepted for 1000", uid)
		}
	}
}

// rejected checks that the server hung up without an exit status, without
// calling the handler, and without keeping any fd it received.
func rejected(t *testing.T, h *handlerFunc, before int, conn *net.UnixConn) {
	t.Helper()
	// ECONNRESET: the server closed with unread data, which the kernel
	// reports to the peer as a reset.
	if st, err := readExit(conn); !errors.Is(err, io.EOF) && !errors.Is(err, unix.ECONNRESET) {
		t.Errorf("got exit %+v, %v; want the connection closed", st, err)
	}
	_ = conn.Close()
	if n := h.calls.Load(); n != 0 {
		t.Errorf("handler called %d times", n)
	}
	if after := openNulls(t); after != before {
		t.Errorf("server kept received fds: %d fds on /dev/null before, %d after", before, after)
	}
}

func TestRejectsForeignUID(t *testing.T) {
	h := &handlerFunc{fn: func(context.Context, *Request, <-chan int) proto.ShimStatus { return proto.ShimStatus{} }}
	sid, _ := serve(t, &Server{Token: testToken, UID: os.Getuid() + 1, Handler: h})
	fds := devNulls(t, 3)
	before := openNulls(t)
	conn := dial(t, sid)
	// Both writes may fail: the uid is checked before anything is read.
	_, _, _ = conn.WriteMsgUnix([]byte{0}, unix.UnixRights(fds...), nil)
	_ = proto.WriteFrame(conn, request("bash", testToken))
	rejected(t, h, before, conn)
}

func TestRejectsWrongFDCount(t *testing.T) {
	h := &handlerFunc{fn: func(context.Context, *Request, <-chan int) proto.ShimStatus { return proto.ShimStatus{} }}
	sid, _ := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
	for _, n := range []int{0, 1, 2, 4, maxRecvFDs + 4} {
		fds := devNulls(t, n)
		before := openNulls(t)
		conn := dial(t, sid)
		sendFDs(t, conn, fds...)
		_ = proto.WriteFrame(conn, request("bash", testToken))
		rejected(t, h, before, conn)
	}
}

func TestRejectsBadRequest(t *testing.T) {
	h := &handlerFunc{fn: func(context.Context, *Request, <-chan int) proto.ShimStatus { return proto.ShimStatus{} }}
	sid, _ := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
	fds := devNulls(t, 3)

	noName := request("", testToken)
	noArgv := request("bash", testToken)
	noArgv.Argv = nil
	for _, req := range []*proto.ShimRequest{noName, noArgv} {
		before := openNulls(t)
		conn := dial(t, sid)
		sendFDs(t, conn, fds...)
		if err := proto.WriteFrame(conn, req); err != nil {
			t.Fatal(err)
		}
		rejected(t, h, before, conn)
	}

	// A frame over the limit is refused before it is read.
	before := openNulls(t)
	conn := dial(t, sid)
	sendFDs(t, conn, fds...)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], proto.MaxControlFrame+1)
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	rejected(t, h, before, conn)
}

func TestRejectsBadToken(t *testing.T) {
	h := &handlerFunc{fn: func(context.Context, *Request, <-chan int) proto.ShimStatus { return proto.ShimStatus{} }}
	sid, _ := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
	emptySID, _ := serve(t, &Server{UID: os.Getuid(), Handler: h})
	for _, tt := range []struct {
		sid   string
		token []byte
	}{
		{sid, []byte("wrong")},
		{sid, nil},
		{sid, append(slices.Clone(testToken), 'x')},
		{sid, testToken[:len(testToken)-1]},
		{emptySID, nil}, // an unset server token matches nothing
	} {
		fds := devNulls(t, 3)
		before := openNulls(t)
		conn := dial(t, tt.sid)
		sendFDs(t, conn, fds...)
		if err := proto.WriteFrame(conn, request("bash", tt.token)); err != nil {
			t.Fatal(err)
		}
		st, err := readExit(conn)
		if err != nil || *st != (proto.ShimStatus{Code: 255, Msg: "session token rejected"}) {
			t.Errorf("token %q: exit %+v, %v", tt.token, st, err)
		}
		if _, err := readExit(conn); !errors.Is(err, io.EOF) {
			t.Errorf("token %q: connection not closed: %v", tt.token, err)
		}
		_ = conn.Close()
		if after := openNulls(t); after != before {
			t.Errorf("token %q: server kept received fds: %d on /dev/null before, %d after", tt.token, before, after)
		}
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("handler called %d times", n)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 50 * time.Millisecond
	defer func() { handshakeTimeout = old }()
	h := &handlerFunc{fn: func(context.Context, *Request, <-chan int) proto.ShimStatus { return proto.ShimStatus{} }}
	sid, stop := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
	defer stop() // before handshakeTimeout is restored

	idle := dial(t, sid)
	if _, err := readExit(idle); !errors.Is(err, io.EOF) {
		t.Errorf("idle connection: %v, want closed", err)
	}
	half := dial(t, sid)
	sendFDs(t, half, devNulls(t, 3)...)
	if _, err := readExit(half); !errors.Is(err, io.EOF) {
		t.Errorf("connection without request: %v, want closed", err)
	}
}

func TestSignals(t *testing.T) {
	got := make(chan []int, 1)
	h := &handlerFunc{fn: func(ctx context.Context, req *Request, sigs <-chan int) proto.ShimStatus {
		closeReq(req)
		var seen []int
		for {
			select {
			case s := <-sigs:
				seen = append(seen, s)
				if s == int(unix.SIGTERM) {
					got <- seen
					return proto.ShimStatus{Signal: s}
				}
			case <-ctx.Done():
				got <- seen
				return proto.ShimStatus{Code: 1}
			}
		}
	}}
	sid, _ := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
	conn := dial(t, sid)
	handshake(t, conn, request("bash", testToken))
	for _, s := range []int{10, 0, 65, -1, 2, 10, 15} {
		if err := proto.WriteFrame(conn, &proto.ShimFrame{Op: proto.ShimSignal, Signal: s}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := readExit(conn)
	if err != nil || *st != (proto.ShimStatus{Signal: 15}) {
		t.Fatalf("exit %+v, %v", st, err)
	}
	seen := <-got
	slices.Sort(seen)
	seen = slices.Compact(seen) // repeats may coalesce
	if !slices.Equal(seen, []int{2, 10, 15}) {
		t.Errorf("handler got signals %v, want 2, 10, 15", seen)
	}
}

// TestSignalFloodDoesNotBlockEOF: a handler that never reads sigs must
// still learn that the shim went away.
func TestSignalFloodDoesNotBlockEOF(t *testing.T) {
	cancelled := make(chan struct{})
	h := &handlerFunc{fn: func(ctx context.Context, req *Request, _ <-chan int) proto.ShimStatus {
		closeReq(req)
		<-ctx.Done()
		close(cancelled)
		return proto.ShimStatus{}
	}}
	sid, _ := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
	conn := dial(t, sid)
	handshake(t, conn, request("bash", testToken))
	for i := range 10000 {
		if err := proto.WriteFrame(conn, &proto.ShimFrame{Op: proto.ShimSignal, Signal: 1 + i%64}); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
	select {
	case <-cancelled:
	case <-time.After(30 * time.Second):
		t.Fatal("handler context not cancelled")
	}
}

func TestConnectionEndCancelsHandler(t *testing.T) {
	for _, name := range []string{"eof", "protocol"} {
		t.Run(name, func(t *testing.T) {
			cancelled := make(chan struct{})
			h := &handlerFunc{fn: func(ctx context.Context, req *Request, _ <-chan int) proto.ShimStatus {
				closeReq(req)
				<-ctx.Done()
				close(cancelled)
				return proto.ShimStatus{}
			}}
			sid, _ := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
			conn := dial(t, sid)
			handshake(t, conn, request("bash", testToken))
			if name == "eof" {
				_ = conn.CloseWrite()
			} else if err := proto.WriteFrame(conn, &proto.ShimFrame{Op: proto.ShimExit}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-cancelled:
			case <-time.After(30 * time.Second):
				t.Fatal("handler context not cancelled")
			}
		})
	}
}

func TestStopDeliversStatus(t *testing.T) {
	started := make(chan struct{})
	h := &handlerFunc{fn: func(ctx context.Context, req *Request, _ <-chan int) proto.ShimStatus {
		closeReq(req)
		close(started)
		<-ctx.Done()
		return proto.ShimStatus{Code: 255, Msg: "session ended"}
	}}
	sid, stop := serve(t, &Server{Token: testToken, UID: os.Getuid(), Handler: h})
	conn := dial(t, sid)
	handshake(t, conn, request("bash", testToken))
	<-started
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	st, err := readExit(conn)
	if err != nil || *st != (proto.ShimStatus{Code: 255, Msg: "session ended"}) {
		t.Errorf("exit %+v, %v", st, err)
	}
	<-stopped
	if _, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: SocketName(sid), Net: "unix"}); err == nil {
		t.Error("listener still accepting after Serve returned")
	}
}

func TestListen(t *testing.T) {
	if _, err := Listen("not-a-session"); err == nil {
		t.Error("Listen accepted an invalid session ID")
	}
	sid := randomSID(t)
	ln, err := Listen(sid)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln.Addr().String() != "@tele-"+sid {
		t.Errorf("address %q", ln.Addr())
	}
	if _, err := Listen(sid); !errors.Is(err, unix.EADDRINUSE) {
		t.Errorf("second Listen = %v, want EADDRINUSE", err)
	}
}

func TestSignalSet(t *testing.T) {
	p := newSignalSet()
	for _, bad := range []int{0, -1, 65} {
		if p.add(bad) {
			t.Errorf("add(%d) accepted", bad)
		}
	}
	for _, s := range []int{64, 15, 2, 15, 1} {
		if !p.add(s) {
			t.Errorf("add(%d) rejected", s)
		}
	}
	var got []int
	for s := p.take(); s != 0; s = p.take() {
		got = append(got, s)
	}
	if !slices.Equal(got, []int{1, 2, 15, 64}) {
		t.Errorf("took %v", got)
	}
}

func TestIsTemporary(t *testing.T) {
	if !isTemporary(&net.OpError{Op: "accept", Err: os.NewSyscallError("accept4", unix.EMFILE)}) {
		t.Error("EMFILE not temporary")
	}
	if isTemporary(net.ErrClosed) || isTemporary(errors.New(strings.Repeat("x", 3))) {
		t.Error("permanent error reported temporary")
	}
}
