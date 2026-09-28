package connectproxy

import (
	"encoding/base64"
	"net/url"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":        true,
		"LocalHost.":       true,
		"app.localhost":    true,
		"127.0.0.1":        true,
		"127.1.2.3":        true,
		"::1":              true,
		"::ffff:127.0.0.1": true,
		"0.0.0.0":          true,
		"::":               true,
		"10.0.0.1":         false,
		"example.com":      false,
		"localhost.com":    false,
		"notlocalhost":     false,
		"":                 false,
	} {
		if got := isLoopback(host); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestCheckTunnelAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"example.com:443": true,
		"[::1]:22":        true,
		"1.2.3.4:65535":   true,
		"example.com":     false,
		":443":            false,
		"example.com:0":   false,
		"example.com:-1":  false,
		"example.com:1e3": false,
		"[::1]":           false,
		"":                false,
	} {
		if err := checkTunnelAddr(addr); (err == nil) != ok {
			t.Errorf("checkTunnelAddr(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestBasicPassword(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := []struct {
		h    string
		want string
		ok   bool
	}{
		{h: "Basic " + enc("u:p"), want: "p", ok: true},
		{h: "basic  " + enc(":p:q") + " ", want: "p:q", ok: true},
		{h: "Basic " + enc("u:"), want: "", ok: true},
		{h: "Basic " + enc("nocolon")},
		{h: "Basic"},
		{h: "Bearer " + enc("u:p")},
		{h: "Basic %%%"},
		{h: ""},
	}
	for _, tt := range tests {
		got, ok := basicPassword(tt.h)
		if ok != tt.ok || got != tt.want {
			t.Errorf("basicPassword(%q) = %q, %v; want %q, %v", tt.h, got, ok, tt.want, tt.ok)
		}
	}
}

func TestUpstreamFromEnv(t *testing.T) {
	https := &url.URL{Scheme: "https", Host: "api.example.com:443"}
	plain := &url.URL{Scheme: "http", Host: "example.com"}
	tests := []struct {
		name            string
		env             map[string]string
		wantNil         bool
		wantTLS, wantHT string // expected proxy hosts; "" is direct
	}{
		{name: "unset", env: map[string]string{"NO_PROXY": "x"}, wantNil: true},
		{
			name:    "upper before lower",
			env:     map[string]string{"HTTPS_PROXY": "http://up:1", "https_proxy": "http://low:1", "http_proxy": "low:2"},
			wantTLS: "up:1", wantHT: "low:2",
		},
		{
			name:    "no_proxy",
			env:     map[string]string{"HTTPS_PROXY": "http://p:1", "HTTP_PROXY": "http://p:1", "no_proxy": ".example.com"},
			wantTLS: "", wantHT: "p:1",
		},
		{
			name:    "loopback targets stay direct",
			env:     map[string]string{"HTTPS_PROXY": "http://p:1"},
			wantTLS: "p:1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := UpstreamFromEnv(func(k string) string { return tt.env[k] })
			if tt.wantNil {
				if f != nil {
					t.Fatal("want nil for an environment without proxies")
				}
				return
			}
			s := &Server{Upstream: f}
			for target, want := range map[*url.URL]string{https: tt.wantTLS, plain: tt.wantHT} {
				got, err := s.upstreamFor(target)
				if err != nil {
					t.Fatal(err)
				}
				if host := hostOf(got); host != want {
					t.Errorf("proxy for %v = %q, want %q", target, host, want)
				}
			}
			for _, lo := range []string{"http://localhost:8080", "https://127.0.0.1:9", "https://[::1]:9"} {
				u, _ := url.Parse(lo)
				if got, _ := s.upstreamFor(u); got != nil {
					t.Errorf("loopback %s sent to %v", lo, got)
				}
			}
		})
	}
}

func hostOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Host
}

func FuzzBasicPassword(f *testing.F) {
	f.Add("Basic dTpw")
	f.Add("basic ===")
	f.Fuzz(func(t *testing.T, h string) {
		pass, ok := basicPassword(h)
		if !ok && pass != "" {
			t.Fatalf("basicPassword(%q) = %q with ok=false", h, pass)
		}
	})
}

func FuzzCheckTunnelAddr(f *testing.F) {
	f.Add("example.com:443")
	f.Add("[::1]:1")
	f.Fuzz(func(t *testing.T, addr string) {
		if checkTunnelAddr(addr) != nil {
			return
		}
		// Anything accepted must be dialable in form.
		u := &url.URL{Scheme: "https", Host: addr}
		if u.Hostname() == "" || u.Port() == "" {
			t.Fatalf("accepted %q without host or port", addr)
		}
	})
}
