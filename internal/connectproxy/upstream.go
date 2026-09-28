package connectproxy

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// errUpstreamLookup replaces errors from Upstream: those may quote the raw
// proxy setting, credentials included, and would reach logs and clients.
var errUpstreamLookup = errors.New("the upstream proxy setting is invalid; check HTTPS_PROXY, HTTP_PROXY and NO_PROXY")

// UpstreamFromEnv returns an Upstream function for the proxy variables of
// an environment, with the semantics of golang.org/x/net/http/httpproxy:
// HTTPS_PROXY for tunnels, HTTP_PROXY for plain requests, NO_PROXY
// exclusions, upper-case names before lower-case ones. It returns nil when
// no proxy is configured.
//
// getenv must read the user's original environment, not the one tele gives
// Claude, which points at this proxy.
func UpstreamFromEnv(getenv func(string) string) func(*url.URL) (*url.URL, error) {
	cfg := &httpproxy.Config{
		HTTPProxy:  firstEnv(getenv, "HTTP_PROXY", "http_proxy"),
		HTTPSProxy: firstEnv(getenv, "HTTPS_PROXY", "https_proxy"),
		NoProxy:    firstEnv(getenv, "NO_PROXY", "no_proxy"),
	}
	if cfg.HTTPProxy == "" && cfg.HTTPSProxy == "" {
		return nil
	}
	return cfg.ProxyFunc()
}

func firstEnv(getenv func(string) string, names ...string) string {
	for _, n := range names {
		if v := getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// upstreamFor returns the upstream proxy for target, or nil for a direct
// connection.
func (s *Server) upstreamFor(target *url.URL) (*url.URL, error) {
	if s.Upstream == nil || isLoopback(target.Hostname()) {
		return nil, nil
	}
	proxy, err := s.Upstream(target)
	if err != nil {
		return nil, errUpstreamLookup
	}
	if proxy == nil {
		return nil, nil
	}
	// httpproxy retries an unparseable setting with "http://" prepended,
	// which can turn "http://user:pw@[bad" into host "http" and a path
	// holding the credentials, which Redacted would not hide. A real proxy
	// URL has no path, so reject one outright.
	if proxy.Opaque != "" || proxy.RawQuery != "" || proxy.Path != "" && proxy.Path != "/" {
		return nil, errUpstreamLookup
	}
	if proxy.Scheme != "http" && proxy.Scheme != "https" {
		return nil, fmt.Errorf("upstream proxy %s: scheme %q is not supported; use http or https",
			describe(proxy), proxy.Scheme)
	}
	if proxy.Hostname() == "" {
		return nil, fmt.Errorf("upstream proxy %s: missing host", describe(proxy))
	}
	return proxy, nil
}

// describe names a proxy in messages by scheme and host only, so that no
// credential can leak however the URL was parsed.
func describe(proxy *url.URL) string {
	return (&url.URL{Scheme: proxy.Scheme, Host: proxy.Host}).String()
}

// isLoopback reports whether host names this machine: "localhost", a
// *.localhost name (RFC 6761), or a loopback or unspecified address.
func isLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsUnspecified()
}
