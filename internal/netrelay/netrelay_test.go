package netrelay

import (
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Error(err)
		}
		accepted <- c
	}()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := <-accepted
	if s == nil {
		t.FailNow()
	}
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	ct, ok1 := c.(*net.TCPConn)
	st, ok2 := s.(*net.TCPConn)
	if !ok1 || !ok2 {
		t.Fatalf("not TCP connections: %T, %T", c, s)
	}
	return ct, st
}

// noHalfClose hides CloseWrite, like a mux stream.
type noHalfClose struct{ net.Conn }

// relay starts Relay between client↔a and b↔server and returns its result.
func relay(a, b net.Conn) <-chan error {
	done := make(chan error, 1)
	go func() { done <- Relay(a, b) }()
	return done
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Relay did not return")
		return nil
	}
}

// TestHalfClose checks that a request ended by a half-close still gets its
// response: the server sees the end of the request while the way back
// stays open.
func TestHalfClose(t *testing.T) {
	client, a := tcpPair(t)
	b, server := tcpPair(t)
	done := relay(a, b)

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	req, err := io.ReadAll(server)
	if err != nil || string(req) != "ping" {
		t.Fatalf("server read %q, %v", req, err)
	}
	if _, err := server.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := io.ReadAll(client)
	if err != nil || string(resp) != "pong" {
		t.Fatalf("client read %q, %v", resp, err)
	}
	if err := wait(t, done); err != nil {
		t.Fatalf("Relay = %v", err)
	}
}

// TestCloseWithoutHalfClose checks that the end of one direction reaches a
// connection without CloseWrite as a Close, so that Relay still ends.
func TestCloseWithoutHalfClose(t *testing.T) {
	client, a := tcpPair(t)
	b, server := tcpPair(t)
	done := relay(a, noHalfClose{b})

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	req, err := io.ReadAll(server)
	if err != nil || string(req) != "ping" {
		t.Fatalf("server read %q, %v", req, err)
	}
	_ = wait(t, done) // b was closed under the other direction's copy: an error is expected
	if _, err := io.ReadAll(client); err != nil {
		t.Fatalf("client did not see the end: %v", err)
	}
}
