package endpoint

import (
	"net"
	"path/filepath"
	"testing"
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
		{in: "203.0.113.5:8443", wantErr: true},
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

	ln, err := ep.Listen(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A live server must not be displaced.
	if _, err := ep.Listen(t.Context()); err == nil {
		t.Fatal("Listen succeeded while another server was listening")
	}
	// Leave the socket file behind, as a crashed server would.
	ul, ok := ln.(*net.UnixListener)
	if !ok {
		t.Fatalf("listener is %T", ln)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()

	ln, err = ep.Listen(t.Context())
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
