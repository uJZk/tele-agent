package claudecompat

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ujzk/tele-agent/internal/testutil/claudetest"
)

// TestProxyAndCABundle pins two assumptions of docs/filesystem.md "本地集合"
// that docs/claude-code.md "待验证的行为" lists: API requests honor
// HTTPS_PROXY, so Claude itself never resolves the API host, and
// SSL_CERT_FILE plus NODE_EXTRA_CA_CERTS suffice to trust a CA.
//
// The API name does not resolve, so a request that bypasses the proxy
// fails, and its certificate is signed by nothing but the given CA file.
func TestProxyAndCABundle(t *testing.T) {
	claude := claudetest.Require(t)
	const host = "api.tele-compat.invalid"
	api := claudetest.NewTLSAPI(t, host, claudetest.Say("through the proxy"))
	proxy := claudetest.NewProxy(t, api.Addr(), "")
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "hi",
		Env:    append([]string{"HTTPS_PROXY=" + proxy.URL(), "https_proxy=" + proxy.URL()}, api.TrustEnv()...),
	})
	if r.Err != nil || r.Output.Result != "through the proxy" {
		t.Fatalf("claude: %v, result %q\nstdout: %s\nstderr: %s", r.Err, r.Output.Result, r.Stdout, r.Stderr)
	}
	if !slices.Contains(proxy.Targets(), host+":443") {
		t.Errorf("proxy saw %q, want %s:443", proxy.Targets(), host)
	}
}

// TestUntrustedCA is the control for TestProxyAndCABundle: without the CA
// file the same request fails, so it is the CA variables that make the
// certificate trusted.
func TestUntrustedCA(t *testing.T) {
	claude := claudetest.Require(t)
	api := claudetest.NewTLSAPI(t, "api.tele-compat.invalid", claudetest.Say("must not arrive"))
	proxy := claudetest.NewProxy(t, api.Addr(), "")
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "hi",
		Args:   []string{"--max-turns", "1"},
		Env:    []string{"HTTPS_PROXY=" + proxy.URL(), "https_proxy=" + proxy.URL()},
	})
	if r.Err == nil || !r.Output.IsError {
		t.Fatalf("claude trusted an unknown CA: %v, result %q", r.Err, r.Output.Result)
	}
	if n := len(api.Requests()); n != 0 {
		t.Fatalf("API received %d requests over an untrusted connection", n)
	}
}

// TestWebFetchThroughProxy pins that WebFetch, both its domain check and
// the fetch itself, goes through HTTPS_PROXY (docs/claude-code.md "代理与
// CA").
func TestWebFetchThroughProxy(t *testing.T) {
	claude := claudetest.Require(t)
	const page = "docs.tele-compat.invalid"
	fetch := claudetest.Use("WebFetch", map[string]any{"url": "https://" + page + "/page", "prompt": "summarize"})
	env := func(api *claudetest.API, proxy *claudetest.Proxy) []string {
		return append([]string{"HTTPS_PROXY=" + proxy.URL()}, api.TrustEnv()...)
	}

	t.Run("preflight", func(t *testing.T) {
		// The domain check asks api.anthropic.com, which the proxy sends
		// to the stand-in, whose certificate does not cover it: the check
		// fails, but only after the proxy saw it.
		api := claudetest.NewTLSAPI(t, "api.tele-compat.invalid", fetch, claudetest.Say("done"))
		proxy := claudetest.NewProxy(t, api.Addr(), "")
		r := claudetest.Run(t, claude, api, claudetest.Options{Prompt: "fetch", Args: []string{"--allowedTools", "WebFetch"}, Env: env(api, proxy)})
		r.Must(t)
		if !slices.Contains(proxy.Targets(), "api.anthropic.com:443") {
			t.Errorf("proxy saw %q, want the domain check to api.anthropic.com:443", proxy.Targets())
		}
	})

	t.Run("fetch", func(t *testing.T) {
		api := claudetest.NewTLSAPI(t, "api.tele-compat.invalid", fetch, claudetest.Say("done"))
		proxy := claudetest.NewProxy(t, api.Addr(), "")
		settings := filepath.Join(t.TempDir(), "settings.json")
		writeJSON(t, settings, map[string]any{"skipWebFetchPreflight": true})
		r := claudetest.Run(t, claude, api, claudetest.Options{
			Prompt: "fetch",
			Args:   []string{"--allowedTools", "WebFetch", "--settings", settings},
			Env:    env(api, proxy),
		})
		r.Must(t)
		if !slices.Contains(proxy.Targets(), page+":443") || !slices.Contains(api.Pages(), page+"/page") {
			t.Errorf("proxy saw %q, pages %q; want the fetch of %s through the proxy", proxy.Targets(), api.Pages(), page)
		}
	})
}

// TestNoDirectConnections pins that with non-essential traffic enabled,
// as under tele, every connection Claude opens goes to the proxy and it
// resolves no names itself (docs/claude-code.md "代理与 CA").
func TestNoDirectConnections(t *testing.T) {
	claude := claudetest.Require(t)
	tr := claudetest.NewTrace(t, claude, "connect", "sendto", "sendmmsg")
	api := claudetest.NewTLSAPI(t, "api.tele-compat.invalid", claudetest.Say("done"))
	proxy := claudetest.NewProxy(t, api.Addr(), "")
	r := claudetest.Run(t, tr.Claude, api, claudetest.Options{
		Prompt: "hi",
		Env: append([]string{
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=", "DISABLE_AUTOUPDATER=",
			"HTTPS_PROXY=" + proxy.URL(), "HTTP_PROXY=" + proxy.URL(),
		}, api.TrustEnv()...),
	})
	if r.Err != nil || r.Output.Result != "done" {
		t.Fatalf("claude: %v, result %q\nstderr: %s", r.Err, r.Output.Result, r.Stderr)
	}
	proxyAddr := strings.TrimPrefix(proxy.URL(), "http://")
	host, port, _ := strings.Cut(proxyAddr, ":")
	toProxy := fmt.Sprintf(`sin_port=htons(%s), sin_addr=inet_addr("%s")`, port, host)
	n := 0
	for _, c := range tr.Calls(t) {
		if !c.Own || !strings.Contains(c.Line, "sa_family=AF_INET") {
			continue // unix sockets stay local
		}
		if c.Name != "connect" || !strings.Contains(c.Line, toProxy) {
			t.Errorf("network call not to the proxy: %s(%s", c.Name, c.Line)
			continue
		}
		n++
	}
	if n == 0 {
		t.Error("no connection to the proxy recorded")
	}
}

// TestCertDirectory pins that Claude's runtime reads a certificate
// directory besides SSL_CERT_FILE: SSL_CERT_DIR if set, else
// /etc/ssl/certs, which the remote view would take from the target. tele
// points SSL_CERT_DIR at an empty local directory (docs/claude-code.md
// "代理与 CA").
func TestCertDirectory(t *testing.T) {
	claude := claudetest.Require(t)
	for _, tc := range []struct {
		name    string
		certDir bool // set SSL_CERT_DIR
	}{
		{"default directory", false},
		{"SSL_CERT_DIR", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := claudetest.NewTrace(t, claude, "openat")
			api := claudetest.NewTLSAPI(t, "api.tele-compat.invalid", claudetest.Say("done"))
			proxy := claudetest.NewProxy(t, api.Addr(), "")
			dir := t.TempDir()
			env := append([]string{"HTTPS_PROXY=" + proxy.URL()}, api.TrustEnv()...)
			if tc.certDir {
				env = append(env, "SSL_CERT_DIR="+dir)
			}
			r := claudetest.Run(t, tr.Claude, api, claudetest.Options{Prompt: "hi", Env: env})
			r.Must(t)
			var system, given bool
			for _, c := range tr.Calls(t) {
				if !c.Own || c.Name != "openat" {
					continue
				}
				system = system || strings.Contains(c.Line, `"/etc/ssl`)
				given = given || strings.Contains(c.Line, `"`+dir+`"`)
			}
			if tc.certDir && (system || !given) {
				t.Errorf("with SSL_CERT_DIR: opened /etc/ssl %v, the given directory %v; want only the given one", system, given)
			}
			if !tc.certDir && !system {
				t.Errorf("without SSL_CERT_DIR Claude read nothing under /etc/ssl; tele's empty SSL_CERT_DIR may no longer be needed")
			}
		})
	}
}
