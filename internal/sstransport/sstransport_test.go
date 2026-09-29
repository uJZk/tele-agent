package sstransport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func newPSK(t *testing.T) PSK {
	t.Helper()
	k, err := NewPSK()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func listen(t *testing.T, psk PSK) *Listener {
	t.Helper()
	l, err := Listen(t.Context(), "127.0.0.1:0", psk, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestRoundTrip(t *testing.T) {
	psk := newPSK(t)
	l := listen(t, psk)
	type result struct {
		got []byte
		err error
	}
	res := make(chan result, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			res <- result{err: err}
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			res <- result{err: err}
			return
		}
		_, err = c.Write(bytes.ToUpper(buf))
		res <- result{got: buf, err: err}
	}()

	c, err := Dial(t.Context(), l.Addr().String(), psk)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatal(err)
	}
	r := <-res
	if r.err != nil || string(r.got) != "hello" || string(reply) != "HELLO" {
		t.Fatalf("server got %q (%v), client got %q", r.got, r.err, reply)
	}
}

func TestLargeTransfer(t *testing.T) {
	psk := newPSK(t)
	l := listen(t, psk)
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	c, err := Dial(t.Context(), l.Addr().String(), psk)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	go func() { _, _ = c.Write(data) }()
	got := make([]byte, len(data))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("echoed data differs")
	}
}

// TestWrongPSK checks that a client with the wrong key learns nothing from
// the server and gets ErrNoResponse, and that the listener never hands the
// connection out.
func TestWrongPSK(t *testing.T) {
	l := listen(t, newPSK(t))
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- c
		}
	}()
	c, err := Dial(t.Context(), l.Addr().String(), newPSK(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	n, err := c.Read(make([]byte, 16))
	if n != 0 || !errors.Is(err, ErrNoResponse) {
		t.Fatalf("read = %d, %v; want 0, ErrNoResponse", n, err)
	}
	select {
	case <-accepted:
		t.Fatal("listener accepted an unauthenticated connection")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRejectIsSilent checks the reject policy: a prober's bytes get no
// answer, and the connection is closed only after rejectDrain.
func TestRejectIsSilent(t *testing.T) {
	old := rejectDrain
	rejectDrain = 500 * time.Millisecond
	t.Cleanup(func() { rejectDrain = old })
	l := listen(t, newPSK(t))

	c, err := dialRaw(t, l)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(bytes.Repeat([]byte{0x42}, 256)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := c.Read(make([]byte, 16))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("prober read %d bytes, %v; want 0 bytes and EOF", n, err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("closed after %v, before the drain period", d)
	}
}

// TestRejectLimit checks that probes beyond maxRejecting are closed at
// once instead of holding a descriptor for rejectDrain.
func TestRejectLimit(t *testing.T) {
	oldDrain, oldMax := rejectDrain, maxRejecting
	rejectDrain, maxRejecting = time.Minute, 2
	t.Cleanup(func() { rejectDrain, maxRejecting = oldDrain, oldMax })
	l := listen(t, newPSK(t))

	probe := func() net.Conn {
		c, err := dialRaw(t, l)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := c.Write(bytes.Repeat([]byte{1}, 256)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	for range maxRejecting {
		probe()
	}
	time.Sleep(200 * time.Millisecond) // let both drains start
	c := probe()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		t.Fatalf("probe over the limit: read error %v; want the connection closed at once", err)
	}
}

// dialRaw opens a plain TCP connection to l, as a prober would.
func dialRaw(t *testing.T, l *Listener) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(t.Context(), "tcp", l.Addr().String())
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestPSKEncoding(t *testing.T) {
	k := newPSK(t)
	got, err := ParsePSK(k.Encode())
	if err != nil || got != k {
		t.Fatalf("ParsePSK(Encode()) = %v, %v", got == k, err)
	}
	for _, bad := range []string{"", "not base64!", k.Encode()[:20]} {
		if _, err := ParsePSK(bad); err == nil {
			t.Errorf("ParsePSK(%q) succeeded", bad)
		}
	}
	// A PSK printed by mistake must not reveal the key.
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v %s %#v %+v", k, k, k, k)} {
		if strings.Contains(s, k.Encode()) || strings.Contains(s, string(k[:4])) {
			t.Errorf("formatted PSK leaks the key: %q", s)
		}
	}
}

func TestCloseStopsAccept(t *testing.T) {
	l, err := Listen(context.Background(), "127.0.0.1:0", newPSK(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	_ = l.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
}
