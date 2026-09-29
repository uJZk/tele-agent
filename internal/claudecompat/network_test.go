package claudecompat

import (
	"slices"
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
	proxy := claudetest.NewProxy(t, api.Addr())
	r := claudetest.Run(t, claude, api, claudetest.Options{
		Prompt: "hi",
		Env: []string{
			"HTTPS_PROXY=" + proxy.URL(), "https_proxy=" + proxy.URL(),
			"SSL_CERT_FILE=" + api.CAFile(), "NODE_EXTRA_CA_CERTS=" + api.CAFile(),
		},
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
	proxy := claudetest.NewProxy(t, api.Addr())
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
