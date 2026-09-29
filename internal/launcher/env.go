package launcher

import (
	"slices"
	"strings"
)

// claudeEnvSpec holds what claudeEnv needs to build the environment of the
// Claude process (docs/claude-code.md "注入的环境").
type claudeEnvSpec struct {
	UserEnv  []string // the environment tele was started with
	SessDir  string   // session directory as Claude sees it: /.tele/<sid>
	Home     string   // target user's home
	User     string   // target user name
	ProxyURL string   // local CONNECT proxy, with credentials
	Switch   []string // teleswitch.Env entries
}

// droppedUserEnv lists variables of the user's environment that must not
// reach Claude: they point at local resources that are invisible or wrong
// inside the remote view, or they are replaced by tele's own values.
var droppedUserEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "PWD", "OLDPWD",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	"LD_PRELOAD", "XDG_RUNTIME_DIR", "SSH_AUTH_SOCK",
	"CLAUDE_CODE_SHELL", "CLAUDE_CODE_SHELL_PREFIX", "CLAUDE_CODE_TMPDIR", "USE_BUILTIN_RIPGREP",
}

func dropUserVar(key string) bool {
	switch {
	case strings.HasPrefix(key, "TELE_"):
		return true
	case isProxyVar(key):
		// Proxy variables are honoured in either case by most clients.
		return true
	default:
		return slices.Contains(droppedUserEnv, key)
	}
}

func isProxyVar(key string) bool {
	switch strings.ToUpper(key) {
	case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY":
		return true
	}
	return false
}

// claudeEnv returns the environment of the Claude process.
func claudeEnv(s claudeEnvSpec) []string {
	env := make([]string, 0, len(s.UserEnv)+24)
	for _, kv := range s.UserEnv {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || dropUserVar(k) {
			continue
		}
		env = append(env, kv)
	}
	bin := s.SessDir + "/" + binDir
	ca := s.SessDir + "/" + caBundleFile
	env = append(env,
		"PATH="+bin,
		"HOME="+s.Home,
		"USER="+s.User,
		"LOGNAME="+s.User,
		// Claude falls back to SHELL when choosing a shell; keep it on the shim.
		"SHELL="+bin+"/bash",
		"CLAUDE_CODE_SHELL="+bin+"/bash",
		"CLAUDE_CODE_SHELL_PREFIX="+bin+"/tele-exec",
		"CLAUDE_CODE_TMPDIR="+s.SessDir+"/"+tmpDir,
		"USE_BUILTIN_RIPGREP=0",
		"HTTPS_PROXY="+s.ProxyURL, "https_proxy="+s.ProxyURL,
		"HTTP_PROXY="+s.ProxyURL, "http_proxy="+s.ProxyURL,
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
		"SSL_CERT_FILE="+ca,
		"NODE_EXTRA_CA_CERTS="+ca,
		"TELE_SESSION="+s.SessDir,
	)
	return append(env, s.Switch...)
}

// proxyURL is the URL of the local CONNECT proxy at addr for Claude: the
// password travels in the userinfo, which Claude sends as Basic
// Proxy-Authorization (docs/security.md "信任边界"). token must be
// URL-safe, as rand.Text is.
func proxyURL(addr, token string) string {
	return "http://tele:" + token + "@" + addr
}
