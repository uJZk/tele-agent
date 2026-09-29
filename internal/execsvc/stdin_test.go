package execsvc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ujzk/tele-agent/internal/proto"
)

func TestStdinQueue(t *testing.T) {
	q := newStdinQueue()
	if err := q.put(make([]byte, proto.ExecStdinWindow+1)); err == nil {
		t.Fatal("put beyond the window succeeded")
	}
	if err := q.put(make([]byte, proto.ExecStdinWindow)); err != nil {
		t.Fatal(err)
	}
	if err := q.put([]byte("x")); err == nil {
		t.Fatal("put into a full window succeeded")
	}
	if data, eof, ok := q.next(); !ok || eof || len(data) != proto.ExecStdinWindow {
		t.Fatalf("next = %d bytes, eof %v, ok %v", len(data), eof, ok)
	}
	if err := q.put([]byte("second")); err != nil {
		t.Fatal(err)
	}
	q.end()
	if err := q.put([]byte("after end")); err != nil { // dropped
		t.Fatal(err)
	}
	if data, _, ok := q.next(); !ok || string(data) != "second" {
		t.Fatalf("next = %q, %v; want second", data, ok)
	}
	if _, eof, ok := q.next(); ok || !eof {
		t.Fatalf("drained queue after end: eof %v ok %v", eof, ok)
	}

	// close drops data and releases a waiting next.
	q = newStdinQueue()
	if err := q.put([]byte("x")); err != nil {
		t.Fatal(err)
	}
	q.close()
	if _, eof, ok := q.next(); ok || eof {
		t.Fatalf("next after close: eof %v ok %v", eof, ok)
	}
	q = newStdinQueue()
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, _, ok := q.next(); ok {
			t.Error("next returned data after close")
		}
	})
	q.close()
	wg.Wait()
}

// flood sends n bytes of stdin in 32 KiB frames without waiting for
// acknowledgements. The stream write deadline turns a server that stopped
// reading into a test failure.
func (s *stream) flood(n int) {
	s.t.Helper()
	if err := s.raw.SetWriteDeadline(time.Now().Add(testTimeout)); err != nil {
		s.t.Fatal(err)
	}
	chunk := make([]byte, 32<<10)
	for ; n > 0; n -= len(chunk) {
		if err := s.conn.Send(&proto.ExecFrame{Op: proto.ExecStdin, Data: chunk[:min(n, len(chunk))]}); err != nil {
			s.t.Fatal(err)
		}
	}
}

// feed sends data as stdin within the window, as rexec does, then its
// end, and collects the stream.
func (s *stream) feed(data []byte) *outcome {
	s.t.Helper()
	credit := make(chan int, 1<<10)
	o := &outcome{onAck: func(n int) { credit <- n }}
	sent := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		avail := proto.ExecStdinWindow
		for rest := data; len(rest) > 0; {
			for avail == 0 {
				select {
				case n := <-credit:
					avail += n
				case <-done:
					sent <- nil
					return
				}
			}
			n := min(len(rest), avail, 32<<10)
			if err := s.conn.Send(&proto.ExecFrame{Op: proto.ExecStdin, Data: rest[:n]}); err != nil {
				sent <- err
				return
			}
			rest, avail = rest[n:], avail-n
		}
		sent <- s.conn.Send(&proto.ExecFrame{Op: proto.ExecStdinEOF})
	}()
	for s.next(o) {
	}
	if err := <-sent; err != nil {
		s.t.Fatal(err)
	}
	if o.exit == nil {
		s.t.Fatal("stream ended without ExecExit")
	}
	return o
}

func (s *stream) untilStarted(o *outcome) {
	s.t.Helper()
	for o.pid == 0 {
		if !s.next(o) {
			s.t.Fatal("no ExecStarted")
		}
	}
}

func TestStdinBacklogKeepsSignals(t *testing.T) {
	// A full window of unread stdin does not delay a signal behind it.
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	s.untilStarted(o)
	s.flood(proto.ExecStdinWindow)
	s.send(&proto.ExecFrame{Op: proto.ExecSignal, Signal: int(unix.SIGTERM)})
	for s.next(o) {
	}
	if o.exit == nil || o.exit.Signal != int(unix.SIGTERM) {
		t.Fatalf("exit %+v, want signal 15", o.exit)
	}
}

func TestStdinBacklogKeepsAbandon(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	s.untilStarted(o)
	s.flood(proto.ExecStdinWindow)
	_ = s.raw.Close()
	waitGone(t, o.pid)
}

func TestStdinBeyondWindow(t *testing.T) {
	// A client that ignores the window violates the protocol: the command
	// is killed and the stream ends without an exit status. The command
	// takes up to a pipe buffer of input, so twice the window overflows.
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}})
	o := &outcome{}
	s.untilStarted(o)
	var wg sync.WaitGroup
	wg.Go(func() {
		chunk := make([]byte, 32<<10)
		for n := 0; n < 2*proto.ExecStdinWindow; n += len(chunk) {
			// Fails once the test closes the stream.
			if s.conn.Send(&proto.ExecFrame{Op: proto.ExecStdin, Data: chunk}) != nil {
				return
			}
		}
	})
	for s.next(o) {
	}
	_ = s.raw.Close()
	wg.Wait()
	if o.exit != nil {
		t.Fatalf("exit %+v, want none", o.exit)
	}
	waitGone(t, o.pid)
}

func TestStdinNotAckedAfterClose(t *testing.T) {
	// Input the command does not take is never acknowledged, so a client
	// stops reading its local stdin instead of consuming input meant for
	// the next reader.
	h := newHarness(t, nil)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	s := h.start(t, &proto.ExecStart{Argv: sh(`exec 0<&-; echo ready; read x < "$1"`, fifo)})
	o := &outcome{}
	for !strings.Contains(o.stdout.String(), "ready") {
		if !s.next(o) {
			t.Fatal("stream ended before the command closed its stdin")
		}
	}
	s.flood(64 << 10)
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("go\n")
	_ = w.Close()
	for s.next(o) {
	}
	if o.exit == nil || o.exit.Code != 0 || o.acked != 0 {
		t.Fatalf("exit %+v, acknowledged %d bytes; want exit 0 and none", o.exit, o.acked)
	}
}

func TestStdinBacklogOnPTY(t *testing.T) {
	h := newHarness(t, nil)
	s := h.start(t, &proto.ExecStart{Argv: []string{"sleep", "1000"}, TTY: &proto.TTYSize{Rows: 24, Cols: 80}})
	o := &outcome{}
	s.untilStarted(o)
	s.flood(proto.ExecStdinWindow)
	s.send(&proto.ExecFrame{Op: proto.ExecSignal, Signal: int(unix.SIGTERM)})
	for s.next(o) {
	}
	if o.exit == nil || o.exit.Signal != int(unix.SIGTERM) {
		t.Fatalf("exit %+v, want signal 15", o.exit)
	}
}

func TestLargeExecStart(t *testing.T) {
	// An ExecStart beyond MaxDataFrame is accepted: here a command line no
	// exec can take, so the start fails with E2BIG instead of the stream.
	h := newHarness(t, nil)
	raw, err := h.client.Open(proto.StreamExec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	env := make([]string, 10)
	for i := range env {
		env[i] = "V" + strconv.Itoa(i) + "=" + strings.Repeat("x", 1<<20)
	}
	if err := proto.WriteFrameLimit(raw, &proto.ExecStart{Argv: []string{"true"}, Dir: "/", Env: env}, proto.MaxExecStart); err != nil {
		t.Fatal(err)
	}
	s := &stream{t: t, raw: raw, conn: proto.NewConn(raw, proto.MaxDataFrame)}
	o := s.collect()
	if o.exit.Err == nil || o.exit.Err.Errno != uint32(unix.E2BIG) {
		t.Fatalf("exit %+v, want start error E2BIG", o.exit)
	}
}
