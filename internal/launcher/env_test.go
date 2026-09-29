package launcher

import (
	"slices"
	"strings"
	"testing"
)

func envMap(env []string) map[string][]string {
	m := map[string][]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = append(m[k], v)
	}
	return m
}

func TestClaudeEnv(t *testing.T) {
	user := []string{
		"PATH=/usr/bin", "HOME=/home/alice", "TERM=xterm", "EDITOR=vim",
		"https_proxy=http://corp:3128", "HTTPS_PROXY=http://corp:3128", "all_proxy=socks5://x",
		"SSL_CERT_FILE=/etc/corp.pem", "LD_PRELOAD=/x.so", "TELE_SESSION=/stale",
		"CLAUDE_CODE_SHELL=/bin/zsh", "TMPDIR=/tmp/alice", "noequals",
	}
	env := claudeEnv(claudeEnvSpec{
		UserEnv:  user,
		SessDir:  "/.tele/0123456789abcdef",
		Home:     "/home/bob",
		User:     "bob",
		ProxyURL: "http://tele:tok@127.0.0.1:4000",
		Switch:   []string{"LD_PRELOAD=/.tele/0123456789abcdef/lib/teleswitch.so", "TELE_SWITCH_FD=3"},
	})
	m := envMap(env)

	want := map[string]string{
		"PATH":                     "/.tele/0123456789abcdef/bin",
		"HOME":                     "/home/bob",
		"USER":                     "bob",
		"TERM":                     "xterm",
		"EDITOR":                   "vim",
		"CLAUDE_CODE_SHELL":        "/.tele/0123456789abcdef/bin/bash",
		"CLAUDE_CODE_SHELL_PREFIX": "/.tele/0123456789abcdef/bin/tele-exec",
		"CLAUDE_CODE_TMPDIR":       "/.tele/0123456789abcdef/tmp",
		"USE_BUILTIN_RIPGREP":      "0",
		"HTTPS_PROXY":              "http://tele:tok@127.0.0.1:4000",
		"https_proxy":              "http://tele:tok@127.0.0.1:4000",
		"NO_PROXY":                 "localhost,127.0.0.1,::1",
		"SSL_CERT_FILE":            "/.tele/0123456789abcdef/ca-bundle.pem",
		"NODE_EXTRA_CA_CERTS":      "/.tele/0123456789abcdef/ca-bundle.pem",
		"SSL_CERT_DIR":             "/.tele/0123456789abcdef/certs",
		"TELE_SESSION":             "/.tele/0123456789abcdef",
		"LD_PRELOAD":               "/.tele/0123456789abcdef/lib/teleswitch.so",
		"TELE_SWITCH_FD":           "3",
	}
	for k, v := range want {
		if got := m[k]; !slices.Equal(got, []string{v}) {
			t.Errorf("%s = %q, want exactly [%q]", k, got, v)
		}
	}
	for _, k := range []string{"all_proxy", "TMPDIR", "noequals"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s leaked into Claude's environment", k)
		}
	}
}
