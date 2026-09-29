package endpoint

import (
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ujzk/tele-agent/internal/sstransport"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    Endpoint
		wantErr bool
	}{
		{in: "unix:/run/tele.sock", want: Endpoint{Network: "unix", Address: "/run/tele.sock"}},
		{in: "unix:/a//b/", want: Endpoint{Network: "unix", Address: "/a/b"}},
		{in: "unix:rel.sock", wantErr: true},
		{in: "203.0.113.5:8443", want: Endpoint{Network: "ss2022", Address: "203.0.113.5:8443"}},
		{in: "tele.example.com:443", want: Endpoint{Network: "ss2022", Address: "tele.example.com:443"}},
		{in: "[2001:db8::1]:8443", want: Endpoint{Network: "ss2022", Address: "[2001:db8::1]:8443"}},
		{in: "host:0", wantErr: true},
		{in: "host:70000", wantErr: true},
		{in: "host:", wantErr: true},
		{in: "nonsense", wantErr: true},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("Parse(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	ep := Endpoint{Network: "unix", Address: filepath.Join(t.TempDir(), "s.sock")}

	ln, err := ep.Listen(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A live server must not be displaced.
	if _, err := ep.Listen(t.Context(), nil); err == nil {
		t.Fatal("Listen succeeded while another server was listening")
	}
	// Leave the socket file behind, as a crashed server would.
	ul, ok := ln.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T", ln)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()

	ln, err = ep.Listen(t.Context(), nil)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	defer ln.Close()
	c, err := ep.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

func TestStringRoundTrip(t *testing.T) {
	for _, in := range []string{"unix:/run/tele.sock", "203.0.113.5:8443", "[2001:db8::1]:8443"} {
		ep, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := ep.String(); got != in {
			t.Errorf("Parse(%q).String() = %q", in, got)
		}
	}
}

func TestSS2022(t *testing.T) {
	psk, err := sstransport.NewPSK()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("127.0.0.1:0"); err == nil {
		t.Fatal("Parse accepted port 0")
	}
	ep := Endpoint{Network: NetworkSS2022, Address: "127.0.0.1:0"}.WithPSK(psk)
	if !ep.Authenticates() {
		t.Fatal("SS2022 endpoint does not authenticate")
	}
	ln, err := ep.Listen(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()

	dialEP, err := Parse(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialEP.Dial(t.Context()); !errors.Is(err, errNoPSK) {
		t.Fatalf("Dial without PSK = %v, want errNoPSK", err)
	}
	dialEP = dialEP.WithPSK(psk)
	// The PSK never shows in the endpoint's text form.
	if s := dialEP.String(); strings.Contains(s, psk.Encode()) {
		t.Fatalf("String() leaks the PSK: %q", s)
	}
	c, err := dialEP.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}
